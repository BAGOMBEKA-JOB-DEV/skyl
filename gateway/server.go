package gateway

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/BAGOMBEKA-JOB-DEV/skyl"
)

// Config configures a [Server].
type Config struct {
	// Providers maps a name to a configured skyl client. Must not be empty.
	Providers map[string]*skyl.Client

	// DefaultProvider is used when a request omits one. If empty, the
	// alphabetically first registered provider is chosen, so the default is
	// deterministic rather than dependent on map iteration order.
	DefaultProvider string

	// AuthToken is the bearer token clients must present. Required — the
	// gateway proxies paid APIs, and an open relay must not be one
	// misconfiguration away.
	AuthToken string

	// AuthTokens are additional accepted tokens, keyed by a label that
	// identifies the caller in logs and metrics.
	//
	// Two tokens accepted at once is what makes rotation possible without a
	// restart: issue the new one, let callers migrate, retire the old one. A
	// single token means every rotation is an outage.
	AuthTokens map[string]string

	// RequestTimeout bounds a single upstream request. Defaults to 120s.
	RequestTimeout time.Duration

	// MaxConcurrent caps in-flight requests. Zero means unlimited.
	//
	// A gateway fronting slow paid APIs holds a goroutine and an upstream
	// connection for the life of every request, so without a cap a burst is
	// passed straight through to the provider — and paid for.
	MaxConcurrent int

	// AllowedOrigins enables CORS for the listed origins. Empty disables it,
	// which is the right default for a server holding API keys: a browser
	// reaching it directly would need the bearer token in client-side code.
	AllowedOrigins []string

	// HeartbeatInterval is how often an idle SSE stream emits a comment frame
	// to keep intermediaries from reaping it. Defaults to 15s; negative
	// disables it.
	//
	// A reasoning model can think for minutes before its first token, and an
	// idle proxy will cut a connection long before that.
	HeartbeatInterval time.Duration

	// MetricsHandler serves /metrics when set. Build one with [NewTelemetry],
	// which also returns the hook that produces the numbers.
	MetricsHandler http.Handler

	// Logger receives structured request logs. Defaults to slog.Default().
	Logger *slog.Logger

	// IncludeRaw echoes each provider's raw body in responses. Off by
	// default: raw bodies can carry request content back to a caller who
	// should not see it.
	IncludeRaw bool
}

// Server routes HTTP requests to skyl providers.
type Server struct {
	cfg      Config
	log      *slog.Logger
	router   chi.Router
	defaultP string
	names    []string

	// tokens maps an accepted bearer value to its caller label.
	tokens map[string]string

	// writeMu serialises SSE writes between the event loop and the heartbeat
	// goroutine. Without it a keep-alive could land inside a data frame.
	writeMu sync.Mutex

	// draining is set when shutdown begins, so readiness fails before the
	// listener stops accepting. An orchestrator needs that window to take the
	// instance out of rotation while in-flight work finishes.
	draining atomic.Bool
}

// NewServer builds a Server.
//
// It returns an error rather than panicking, because both failure modes are
// operator configuration problems that deserve a readable message: no
// providers registered, or no auth token.
func NewServer(cfg Config) (*Server, error) {
	if len(cfg.Providers) == 0 {
		return nil, errors.New("gateway: no providers registered; set at least one provider API key")
	}
	if cfg.AuthToken == "" {
		return nil, errors.New("gateway: an auth token is required; refusing to start an open relay to paid APIs")
	}
	if cfg.RequestTimeout <= 0 {
		cfg.RequestTimeout = 120 * time.Second
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}

	names := make([]string, 0, len(cfg.Providers))
	for name := range cfg.Providers {
		names = append(names, name)
	}
	sort.Strings(names)

	def := cfg.DefaultProvider
	if def == "" {
		def = names[0]
	} else if _, ok := cfg.Providers[def]; !ok {
		return nil, fmt.Errorf("gateway: default provider %q is not registered", def)
	}

	if cfg.HeartbeatInterval == 0 {
		cfg.HeartbeatInterval = 15 * time.Second
	}

	// The primary token plus any rotation tokens, in one lookup keyed by the
	// full header value so authentication is a single compare per candidate.
	tokens := map[string]string{"Bearer " + cfg.AuthToken: "default"}
	for label, tok := range cfg.AuthTokens {
		if tok == "" {
			return nil, fmt.Errorf("gateway: auth token %q is empty", label)
		}
		tokens["Bearer "+tok] = label
	}

	s := &Server{cfg: cfg, log: cfg.Logger, defaultP: def, names: names, tokens: tokens}
	s.router = s.routes()
	return s, nil
}

// StartDraining marks the server as not ready.
//
// Call it before http.Server.Shutdown: readiness fails immediately, the
// orchestrator stops sending new traffic, and in-flight requests still finish.
// Shutdown alone cannot do this — it stops the listener, which looks to a load
// balancer like a refused connection rather than a planned withdrawal.
func (s *Server) StartDraining() { s.draining.Store(true) }

// Draining reports whether shutdown has begun.
func (s *Server) Draining() bool { return s.draining.Load() }

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.router.ServeHTTP(w, r)
}

// Providers returns the registered provider names, sorted.
func (s *Server) Providers() []string { return s.names }

// routes builds the chi router and its middleware stack.
func (s *Server) routes() chi.Router {
	r := chi.NewRouter()

	// Outermost first. Recoverer sits above the logger so a panic is still
	// logged as a completed request rather than vanishing.
	//
	// chi's RealIP middleware is deliberately NOT used. It rewrites
	// r.RemoteAddr from X-Forwarded-For / True-Client-IP / X-Real-IP whether
	// or not the deployment actually sets them, so any client can claim any
	// address (GHSA-3fxj-6jh8-hvhx and friends). Nothing here needs the
	// client IP, so the safe choice is to leave RemoteAddr as the real peer
	// address. A deployment that needs the originating IP should extract it
	// from a header its own trusted proxy is known to set.
	r.Use(middleware.RequestID)
	r.Use(s.echoRequestID)
	r.Use(middleware.Recoverer)
	r.Use(s.logRequests)

	// CORS runs before authentication: a preflight OPTIONS carries no
	// Authorization header, so authenticating it would reject every browser
	// client before it ever sent the real request.
	if len(s.cfg.AllowedOrigins) > 0 {
		r.Use(s.cors)
	}

	// Liveness is unauthenticated so orchestrators can probe without a
	// credential. It reveals nothing beyond "the process is up", and it stays
	// green while draining — the process is alive and finishing work, and
	// failing liveness here would have the orchestrator kill it mid-request.
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
	})

	// Readiness is the one that flips on shutdown.
	r.Get("/readyz", s.handleReady)

	// Metrics are unauthenticated so an in-cluster scraper needs no
	// credential. They carry no prompt content — only counts, durations and
	// the provider/model labels the conventions define.
	if s.cfg.MetricsHandler != nil {
		r.Handle("/metrics", s.cfg.MetricsHandler)
	}

	r.Group(func(r chi.Router) {
		r.Use(s.authenticate)

		// Concurrency limiting sits inside authentication so unauthenticated
		// traffic cannot consume the budget, and it is chi's own middleware
		// rather than a new dependency.
		if s.cfg.MaxConcurrent > 0 {
			r.Use(middleware.Throttle(s.cfg.MaxConcurrent))
		}

		r.Route("/v1", func(r chi.Router) {
			r.Get("/providers", s.handleProviders)
			r.Get("/models", s.handleModels)
			r.Post("/chat", s.handleChat)
			r.Post("/chat/stream", s.handleChatStream)
		})
	})

	return r
}

// tenantKey types the context value holding the caller label.
type tenantKey struct{}

// TenantFrom returns the label of the token that authenticated the request.
//
// It is what makes per-caller metrics and log attribution possible without the
// token itself ever leaving the auth middleware.
func TenantFrom(ctx context.Context) string {
	label, _ := ctx.Value(tenantKey{}).(string)
	return label
}

// handleReady reports readiness, which is false once draining starts.
func (s *Server) handleReady(w http.ResponseWriter, _ *http.Request) {
	if s.Draining() {
		writeJSON(w, http.StatusServiceUnavailable,
			map[string]any{"status": "draining"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ready"})
}

// echoRequestID returns the request ID to the caller.
//
// chi generates one into the context and logs it, but never sets it on the
// response — so the caller has nothing to quote when reporting a problem.
func (s *Server) echoRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if id := middleware.GetReqID(r.Context()); id != "" {
			w.Header().Set("X-Request-Id", id)
		}
		next.ServeHTTP(w, r)
	})
}

// cors answers preflights and marks responses for the configured origins.
func (s *Server) cors(next http.Handler) http.Handler {
	allowed := make(map[string]bool, len(s.cfg.AllowedOrigins))
	for _, o := range s.cfg.AllowedOrigins {
		allowed[o] = true
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		// Echo the specific origin rather than "*": credentials are involved,
		// and a wildcard cannot carry them.
		if origin != "" && (allowed[origin] || allowed["*"]) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ---------------------------------------------------------------------------
// Middleware
// ---------------------------------------------------------------------------

// authenticate enforces the bearer token and labels the caller.
func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := []byte(r.Header.Get("Authorization"))

		// Every candidate is compared, without an early exit on the first
		// match: returning as soon as one succeeds would make the time taken
		// depend on which token was presented, and constant-time comparison
		// of the individual tokens would not save it.
		label := ""
		for want, name := range s.tokens {
			if subtle.ConstantTimeCompare(got, []byte(want)) == 1 {
				label = name
			}
		}
		if label == "" {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeError(w, http.StatusUnauthorized, "unauthorized", "auth")
			return
		}

		next.ServeHTTP(w, r.WithContext(
			context.WithValue(r.Context(), tenantKey{}, label)))
	})
}

// logRequests emits one structured line per request. It deliberately logs no
// headers and no bodies: those carry credentials and prompt content.
func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)

		next.ServeHTTP(ww, r)

		s.log.Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", ww.Status(),
			"bytes", ww.BytesWritten(),
			"duration", time.Since(start),
			"request_id", middleware.GetReqID(r.Context()),
			"tenant", TenantFrom(r.Context()),
		)
	})
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

func (s *Server) handleProviders(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"providers": s.names,
		"default":   s.defaultP,
	})
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("provider")
	client, resolved, err := s.resolve(name)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error(), "not_found")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.RequestTimeout)
	defer cancel()

	models, err := client.Models(ctx)
	if err != nil {
		s.writeUpstreamError(w, err)
		return
	}

	out := ModelsResponse{Provider: resolved, Models: make([]ModelItem, 0, len(models))}
	for _, m := range models {
		out.Models = append(out.Models, ModelItem{
			ID:              m.ID,
			DisplayName:     m.DisplayName,
			ContextWindow:   m.ContextWindow,
			MaxOutputTokens: m.MaxOutputTokens,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	req, client, ok := s.decodeChat(w, r)
	if !ok {
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.RequestTimeout)
	defer cancel()

	resp, err := client.Complete(ctx, req)
	if err != nil {
		s.writeUpstreamError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toChatResponse(resp, s.cfg.IncludeRaw))
}

func (s *Server) handleChatStream(w http.ResponseWriter, r *http.Request) {
	req, client, ok := s.decodeChat(w, r)
	if !ok {
		return
	}

	flusher, canFlush := w.(http.Flusher)
	if !canFlush {
		writeError(w, http.StatusInternalServerError, "streaming unsupported by this server", "")
		return
	}

	// The stream is bound to the request context, so a client hanging up
	// cancels the upstream call rather than leaving a paid request running.
	stream, err := client.Stream(r.Context(), req)
	if err != nil {
		s.writeUpstreamError(w, err)
		return
	}
	defer stream.Close() //nolint:errcheck // best effort on the response path

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	// nginx buffers proxied responses by default, which turns a stream into
	// one delivery at the end and quietly removes the only property this
	// endpoint has. This header is how you tell it not to.
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	// A reasoning model can think for minutes before its first token, and an
	// idle intermediary will reap the connection long before that. A comment
	// frame keeps it alive and is ignored by any conforming SSE client.
	stopBeat := s.startHeartbeat(r.Context(), w, flusher)
	defer stopBeat()

	for stream.Next() {
		ev := stream.Event()

		payload := map[string]any{"type": string(ev.Type)}
		switch ev.Type {
		case skyl.EventTextDelta, skyl.EventThinkingDelta:
			payload["text"] = ev.Text
		case skyl.EventToolCall:
			if ev.ToolCall != nil {
				payload["tool_call"] = ChatToolCall{
					ID: ev.ToolCall.ID, Name: ev.ToolCall.Name, Arguments: ev.ToolCall.Arguments,
				}
			}
		case skyl.EventDone:
			payload["stop_reason"] = string(ev.StopReason)
			if ev.Usage != nil {
				payload["usage"] = ChatUsage{
					InputTokens:      ev.Usage.InputTokens,
					OutputTokens:     ev.Usage.OutputTokens,
					CacheReadTokens:  ev.Usage.CacheReadTokens,
					CacheWriteTokens: ev.Usage.CacheWriteTokens,
				}
			}
		}

		s.writeMu.Lock()
		ok := writeSSE(w, payload)
		if ok {
			flusher.Flush()
		}
		s.writeMu.Unlock()
		if !ok {
			return
		}
	}

	if err := stream.Err(); err != nil {
		// Headers are already sent, so the error has to ride the stream.
		s.writeMu.Lock()
		defer s.writeMu.Unlock()
		_ = writeSSE(w, map[string]any{
			"type":  "error",
			"error": err.Error(),
			"kind":  kindOf(err),
		})
		flusher.Flush()
	}
}

// decodeChat parses and validates a chat request, writing any error itself.
func (s *Server) decodeChat(w http.ResponseWriter, r *http.Request) (*skyl.Request, *skyl.Client, bool) {
	var body ChatRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error(), "bad_request")
		return nil, nil, false
	}

	client, _, err := s.resolve(body.Provider)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error(), "not_found")
		return nil, nil, false
	}

	req, err := body.toSkylRequest()
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "bad_request")
		return nil, nil, false
	}
	if err := req.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "bad_request")
		return nil, nil, false
	}
	return req, client, true
}

// resolve maps a provider name (possibly empty) to a client.
func (s *Server) resolve(name string) (*skyl.Client, string, error) {
	if name == "" {
		name = s.defaultP
	}
	client, ok := s.cfg.Providers[name]
	if !ok {
		return nil, "", fmt.Errorf("unknown provider %q; registered: %s",
			name, strings.Join(s.names, ", "))
	}
	return client, name, nil
}

// ---------------------------------------------------------------------------
// Responses
// ---------------------------------------------------------------------------

// maxRequestBytes bounds a request body. Generous enough for a long
// conversation, small enough that a hostile client cannot exhaust memory.
const maxRequestBytes = 16 << 20

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg, kind string) {
	writeJSON(w, status, ErrorResponse{Error: msg, Kind: kind})
}

// writeSSE emits one event, reporting whether the client is still connected.
func writeSSE(w http.ResponseWriter, payload any) bool {
	data, err := json.Marshal(payload)
	if err != nil {
		return false
	}
	if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
		return false
	}
	return true
}

// writeUpstreamError maps a skyl error onto an HTTP status.
//
// The provider's raw body is deliberately not forwarded: it can echo request
// content back to a caller who should not see it.
func (s *Server) writeUpstreamError(w http.ResponseWriter, err error) {
	status := http.StatusBadGateway
	switch {
	case errors.Is(err, skyl.ErrAuth):
		// The caller's token was fine; ours was not. That is our problem.
		status = http.StatusBadGateway
	case errors.Is(err, skyl.ErrRateLimit):
		status = http.StatusTooManyRequests
	case errors.Is(err, skyl.ErrNotFound):
		status = http.StatusNotFound
	case errors.Is(err, skyl.ErrBadRequest), errors.Is(err, skyl.ErrUnsupported):
		status = http.StatusBadRequest
	case errors.Is(err, skyl.ErrRefusal):
		status = http.StatusUnprocessableEntity
	}

	s.log.Warn("upstream error", "kind", kindOf(err), "status", status, "error", err.Error())
	writeError(w, status, err.Error(), kindOf(err))
}

// kindOf renders a skyl sentinel as a stable machine-readable string, so
// clients can branch without parsing prose.
func kindOf(err error) string {
	switch {
	case errors.Is(err, skyl.ErrAuth):
		return "auth"
	case errors.Is(err, skyl.ErrRateLimit):
		return "rate_limit"
	case errors.Is(err, skyl.ErrNotFound):
		return "not_found"
	case errors.Is(err, skyl.ErrBadRequest):
		return "bad_request"
	case errors.Is(err, skyl.ErrUnsupported):
		return "unsupported"
	case errors.Is(err, skyl.ErrRefusal):
		return "refusal"
	case errors.Is(err, skyl.ErrServer):
		return "server"
	default:
		return "unknown"
	}
}

// startHeartbeat emits SSE comment frames while the stream is idle, and
// returns a function that stops it.
//
// The writes are serialised with the event loop by a mutex, because a comment
// interleaved halfway through a data frame would corrupt it. The mutex is the
// reason this cannot simply be a goroutine writing to w.
func (s *Server) startHeartbeat(ctx context.Context, w http.ResponseWriter, flusher http.Flusher) func() {
	if s.cfg.HeartbeatInterval <= 0 {
		return func() {}
	}

	done := make(chan struct{})
	var stopOnce sync.Once
	stop := func() { stopOnce.Do(func() { close(done) }) }

	go func() {
		ticker := time.NewTicker(s.cfg.HeartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.writeMu.Lock()
				_, err := fmt.Fprint(w, ": keep-alive\n\n")
				if err == nil {
					flusher.Flush()
				}
				s.writeMu.Unlock()
				if err != nil {
					return
				}
			}
		}
	}()

	return stop
}
