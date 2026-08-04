// Package sandbox serves each provider's wire protocol locally, with no
// credentials and no cost.
//
// # Why this exists
//
// Every adapter unit test in this repository replays payloads written from
// provider documentation. That proves the mapping is self-consistent, but a
// fake built from the same assumptions as the adapter agrees with the adapter
// by construction — if a field name is wrong, the fake is wrong the same way
// and the test stays green. Only a real call closes that gap, and a real call
// needs a paid credential.
//
// The sandbox does not close that gap, and nothing here should be read as
// claiming it does. What it does is make everything *between* the mapping and
// the provider real: a genuine TCP connection, a genuine net/http round trip,
// genuine chunked SSE framing, genuine status codes and error bodies. In-process
// httptest covers some of that; the sandbox additionally lets the full
// integration suite — the same suite that runs against live APIs — execute end
// to end, so the harness itself is exercised rather than merely compiled.
//
// It is also useful on its own: point an application at it to develop against
// skyl without burning credits or leaking a key into a dev environment.
//
// # Running it
//
//	go run ./cmd/skyl-sandbox
//
// Then mount each provider at its own prefix — one server serves all four:
//
//	anthropic     http://localhost:8099/anthropic
//	openai        http://localhost:8099/openai/v1
//	gemini        http://localhost:8099/gemini/v1beta
//	openaicompat  http://localhost:8099/compat/v1
//
// # Deliberate limits
//
// There is no model here. Replies come from a lookup table, so the sandbox can
// verify transport, framing, and error handling but says nothing about how a
// real model behaves. Token counts are word counts, not a real tokenizer.
package sandbox

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

// DefaultAPIKey is the credential the sandbox accepts when none is configured.
//
// It is a fixed, well-known string on purpose: this server must never be
// mistaken for something guarding real capability, and a random-looking secret
// would suggest otherwise.
const DefaultAPIKey = "sandbox-key"

// StatusModelPrefix makes a request fail on demand. A model ID of
// "sandbox-status-429" returns HTTP 429 in that provider's error shape, which
// is how the retry and classification paths get exercised without waiting for
// a real provider to rate-limit us.
const StatusModelPrefix = "sandbox-status-"

// Stream faults make a response fail *after* it has started.
//
// StatusModelPrefix cannot express this: it is evaluated before the response
// begins, and once the SSE header has been written the status is fixed. But a
// stream that dies mid-flight is an ordinary production event — a dropped load
// balancer, a proxy timeout, a provider erroring after the first token — and
// every adapter's handling of it was dead code, because nothing could produce
// one.
//
// The model ID carries the instruction, as it does for injected statuses:
//
//	sandbox-stream-truncate   cut the connection with no terminal event
//	sandbox-stream-error      emit the provider's own in-band error frame
const (
	StreamFaultPrefix = "sandbox-stream-"

	// FaultTruncate ends the stream after the first frame, with no
	// finish_reason, no [DONE], and no message_stop. A caller must not mistake
	// the partial answer for a complete one.
	FaultTruncate = "sandbox-stream-truncate"

	// FaultMidStreamError emits an error inside the stream body, after a
	// successful 200 and at least one good frame.
	FaultMidStreamError = "sandbox-stream-error"
)

// streamFault reports the fault a model ID asks for, if any.
func streamFault(model string) (string, bool) {
	if !strings.HasPrefix(model, StreamFaultPrefix) {
		return "", false
	}
	switch model {
	case FaultTruncate, FaultMidStreamError:
		return model, true
	default:
		return "", false
	}
}

// Handler serves the sandbox. Safe for concurrent use.
type Handler struct {
	mux    *http.ServeMux
	apiKey string

	// requests counts served calls, so a test can assert that a retry actually
	// reached the wire rather than being satisfied from a cache.
	requests atomic.Int64
}

// Option configures a [Handler].
type Option func(*Handler)

// WithAPIKey sets the credential the sandbox requires.
func WithAPIKey(key string) Option {
	return func(h *Handler) { h.apiKey = key }
}

// New returns a Handler serving all four provider protocols.
func New(opts ...Option) *Handler {
	h := &Handler{mux: http.NewServeMux(), apiKey: DefaultAPIKey}
	for _, opt := range opts {
		opt(h)
	}

	// Anthropic: /anthropic/v1/...
	h.mux.HandleFunc("POST /anthropic/v1/messages", h.anthropicMessages)
	h.mux.HandleFunc("GET /anthropic/v1/models", h.anthropicModels)

	// OpenAI and the generic compatible adapter speak the same protocol, so
	// they share handlers behind two mounts.
	for _, prefix := range []string{"/openai/v1", "/compat/v1"} {
		h.mux.HandleFunc("POST "+prefix+"/chat/completions", h.oaiChat)
		h.mux.HandleFunc("GET "+prefix+"/models", h.oaiModels)
	}

	// Gemini: /gemini/v1beta/...
	h.mux.HandleFunc("GET /gemini/v1beta/models", h.geminiModels)
	h.mux.HandleFunc("POST /gemini/v1beta/models/{model}", h.geminiGenerate)

	h.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "sandbox": true})
	})

	return h
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.requests.Add(1)
	h.mux.ServeHTTP(w, r)
}

// Requests returns the number of requests served.
func (h *Handler) Requests() int64 { return h.requests.Load() }

// ---------------------------------------------------------------------------
// The "model"
// ---------------------------------------------------------------------------

// reply produces a deterministic answer to a prompt.
//
// The live suite asks for the capital of France because it wants one
// unambiguous word back; the sandbox answers the same question the same way so
// the identical assertions hold against both.
func reply(prompt string) string {
	switch {
	case strings.Contains(strings.ToLower(prompt), "capital of france"):
		return "Paris"
	case prompt == "":
		return "Hello from the skyl sandbox."
	default:
		return "You said: " + prompt
	}
}

// countTokens approximates usage by counting words.
//
// Real tokenizers split differently, so this is only ever a plausible non-zero
// number — enough to prove usage is parsed and carried, not enough to reason
// about cost.
func countTokens(s string) int {
	n := len(strings.Fields(s))
	if n == 0 {
		return 1
	}
	return n
}

// chunk splits a reply into streaming deltas, keeping the trailing space so
// the concatenation is exactly the non-streamed answer.
func chunk(s string) []string {
	words := strings.Fields(s)
	if len(words) == 0 {
		return nil
	}
	out := make([]string, 0, len(words))
	for i, w := range words {
		if i < len(words)-1 {
			w += " "
		}
		out = append(out, w)
	}
	return out
}

// knownModels is the sandbox's catalogue, per provider.
//
// A request for anything else returns that provider's not-found error, which
// is what makes the "rejects a bogus model" check meaningful: skyl passes model
// IDs through unvalidated (ADR-0004), so the provider's own 404 is the only
// thing standing between a typo and a confusing failure.
var knownModels = map[string][]modelEntry{
	"anthropic": {
		{ID: "claude-opus-5", Display: "Claude Opus 5", In: 400000, Out: 64000},
		{ID: "claude-sonnet-5", Display: "Claude Sonnet 5", In: 400000, Out: 64000},
		{ID: "claude-haiku-4-5", Display: "Claude Haiku 4.5", In: 200000, Out: 32000},
	},
	"openai": {
		{ID: "gpt-5.6", Display: "GPT-5.6", In: 400000, Out: 128000},
		{ID: "gpt-5.4-nano", Display: "GPT-5.4 nano", In: 128000, Out: 16000},
	},
	"gemini": {
		{ID: "gemini-3.6-flash", Display: "Gemini 3.6 Flash", In: 1000000, Out: 64000},
		{ID: "gemini-3.6-pro", Display: "Gemini 3.6 Pro", In: 2000000, Out: 64000},
	},
}

type modelEntry struct {
	ID      string
	Display string
	In      int
	Out     int
}

func known(provider, id string) bool {
	for _, m := range knownModels[provider] {
		if m.ID == id {
			return true
		}
	}
	return false
}

// injectedStatus reports the HTTP status a model ID asks the sandbox to return.
func injectedStatus(model string) (int, bool) {
	rest, ok := strings.CutPrefix(model, StatusModelPrefix)
	if !ok {
		return 0, false
	}
	var code int
	if _, err := fmt.Sscanf(rest, "%d", &code); err != nil {
		return 0, false
	}
	if code < 100 || code > 599 {
		return 0, false
	}
	return code, true
}

// ---------------------------------------------------------------------------
// Shared helpers
// ---------------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// beginSSE sets the headers an SSE response needs and returns a flush func.
//
// Flushing after every frame is the point: without it the server buffers the
// whole body and the client sees one delivery, which would let a broken
// incremental parser pass.
func beginSSE(w http.ResponseWriter) func() {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	flusher, ok := w.(http.Flusher)
	if !ok {
		return func() {}
	}
	flusher.Flush()
	return flusher.Flush
}

// streamDelay paces frames so a stream is observably incremental rather than
// arriving as one buffered blob. Small enough not to slow the suite down.
const streamDelay = 2 * time.Millisecond
