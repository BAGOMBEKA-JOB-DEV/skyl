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

	// RequestTimeout bounds a single upstream request. Defaults to 120s.
	RequestTimeout time.Duration

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

	s := &Server{cfg: cfg, log: cfg.Logger, defaultP: def, names: names}
	s.router = s.routes()
	return s, nil
}

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
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)
	r.Use(s.logRequests)

	// Liveness is unauthenticated so orchestrators can probe without a
	// credential. It reveals nothing beyond "the process is up".
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})

	r.Group(func(r chi.Router) {
		r.Use(s.authenticate)

		r.Route("/v1", func(r chi.Router) {
			r.Get("/providers", s.handleProviders)
			r.Get("/models", s.handleModels)
			r.Post("/chat", s.handleChat)
			r.Post("/chat/stream", s.handleChatStream)
		})
	})

	return r
}

// ---------------------------------------------------------------------------
// Middleware
// ---------------------------------------------------------------------------

// authenticate enforces the bearer token.
func (s *Server) authenticate(next http.Handler) http.Handler {
	want := []byte("Bearer " + s.cfg.AuthToken)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := []byte(r.Header.Get("Authorization"))
		// Constant-time compare so a timing side channel cannot reveal the
		// token one byte at a time.
		if subtle.ConstantTimeCompare(got, want) != 1 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeError(w, http.StatusUnauthorized, "unauthorized", "auth")
			return
		}
		next.ServeHTTP(w, r)
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
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

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

		if !writeSSE(w, payload) {
			return
		}
		flusher.Flush()
	}

	if err := stream.Err(); err != nil {
		// Headers are already sent, so the error has to ride the stream.
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
