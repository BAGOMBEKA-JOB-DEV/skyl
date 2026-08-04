package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/BAGOMBEKA-JOB-DEV/skyl"
)

const testToken = "test-token"

// stubProvider is a scriptable skyl.Provider; no gateway test touches the
// network.
type stubProvider struct {
	name       string
	resp       *skyl.Response
	err        error
	models     []skyl.ModelInfo
	streamEvts []skyl.StreamEvent
	streamErr  error
	lastReq    *skyl.Request
}

func (s *stubProvider) Name() string { return s.name }

func (s *stubProvider) Complete(_ context.Context, req *skyl.Request) (*skyl.Response, error) {
	s.lastReq = req
	if s.err != nil {
		return nil, s.err
	}
	return s.resp, nil
}

func (s *stubProvider) Stream(_ context.Context, req *skyl.Request) (skyl.Stream, error) {
	s.lastReq = req
	if s.err != nil {
		return nil, s.err
	}
	return &stubStream{events: s.streamEvts, err: s.streamErr}, nil
}

func (s *stubProvider) Models(context.Context) ([]skyl.ModelInfo, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.models, nil
}

type stubStream struct {
	events []skyl.StreamEvent
	err    error
	i      int
}

func (s *stubStream) Next() bool {
	if s.i >= len(s.events) {
		return false
	}
	s.i++
	return true
}
func (s *stubStream) Event() skyl.StreamEvent { return s.events[s.i-1] }
func (s *stubStream) Err() error              { return s.err }
func (s *stubStream) Close() error            { return nil }

func newTestServer(t *testing.T, p skyl.Provider) *Server {
	t.Helper()
	srv, err := NewServer(Config{
		Providers: map[string]*skyl.Client{p.Name(): skyl.New(p, skyl.WithMaxRetries(0))},
		AuthToken: testToken,
		Logger:    slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	return srv
}

func do(t *testing.T, srv *Server, method, path, body, token string) *httptest.ResponseRecorder {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, r)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

func okProvider() *stubProvider {
	return &stubProvider{
		name: "fake",
		resp: &skyl.Response{
			ID:         "resp-1",
			Provider:   "fake",
			Model:      "some-model",
			Message:    skyl.AssistantText("hello there"),
			StopReason: skyl.StopEndTurn,
			Usage:      skyl.Usage{InputTokens: 5, OutputTokens: 2},
			Raw:        json.RawMessage(`{"secret":"raw body"}`),
		},
	}
}

// ---------------------------------------------------------------------------
// Construction
// ---------------------------------------------------------------------------

func TestNewServerRequiresProviders(t *testing.T) {
	t.Parallel()

	_, err := NewServer(Config{AuthToken: testToken})
	if err == nil {
		t.Fatal("NewServer() error = nil, want a failure with no providers")
	}
	if !strings.Contains(err.Error(), "no providers") {
		t.Errorf("err = %q, want it to explain the missing providers", err)
	}
}

func TestNewServerRequiresAuthToken(t *testing.T) {
	t.Parallel()

	_, err := NewServer(Config{
		Providers: map[string]*skyl.Client{"fake": skyl.New(okProvider())},
	})
	if err == nil {
		t.Fatal("NewServer() error = nil; the gateway must refuse to start as an open relay")
	}
	if !strings.Contains(err.Error(), "auth token") {
		t.Errorf("err = %q, want it to name the missing token", err)
	}
}

func TestNewServerRejectsUnknownDefaultProvider(t *testing.T) {
	t.Parallel()

	_, err := NewServer(Config{
		Providers:       map[string]*skyl.Client{"fake": skyl.New(okProvider())},
		DefaultProvider: "nonexistent",
		AuthToken:       testToken,
	})
	if err == nil {
		t.Fatal("NewServer() error = nil, want a failure for an unregistered default")
	}
}

func TestDefaultProviderIsDeterministic(t *testing.T) {
	t.Parallel()

	// Map iteration order is random; the chosen default must not be.
	for range 10 {
		srv, err := NewServer(Config{
			Providers: map[string]*skyl.Client{
				"zeta":  skyl.New(&stubProvider{name: "zeta"}),
				"alpha": skyl.New(&stubProvider{name: "alpha"}),
				"mid":   skyl.New(&stubProvider{name: "mid"}),
			},
			AuthToken: testToken,
			Logger:    slog.New(slog.DiscardHandler),
		})
		if err != nil {
			t.Fatalf("NewServer() error = %v", err)
		}
		if srv.defaultP != "alpha" {
			t.Fatalf("default provider = %q, want the alphabetically first", srv.defaultP)
		}
	}
}

// ---------------------------------------------------------------------------
// Auth
// ---------------------------------------------------------------------------

func TestAuthentication(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t, okProvider())

	tests := []struct {
		name       string
		token      string
		wantStatus int
	}{
		{"correct token", testToken, http.StatusOK},
		{"no token", "", http.StatusUnauthorized},
		{"wrong token", "nope", http.StatusUnauthorized},
		{"token prefix", testToken[:4], http.StatusUnauthorized},
		{"token with suffix", testToken + "x", http.StatusUnauthorized},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rec := do(t, srv, http.MethodGet, "/v1/providers", "", tc.token)
			if rec.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
		})
	}
}

func TestHealthzIsUnauthenticated(t *testing.T) {
	t.Parallel()

	rec := do(t, newTestServer(t, okProvider()), http.MethodGet, "/healthz", "", "")
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 — orchestrators probe without a credential", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

func TestHandleProviders(t *testing.T) {
	t.Parallel()

	rec := do(t, newTestServer(t, okProvider()), http.MethodGet, "/v1/providers", "", testToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var body struct {
		Providers []string `json:"providers"`
		Default   string   `json:"default"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	if len(body.Providers) != 1 || body.Providers[0] != "fake" {
		t.Errorf("providers = %v, want [fake]", body.Providers)
	}
	if body.Default != "fake" {
		t.Errorf("default = %q, want fake", body.Default)
	}
}

func TestHandleChat(t *testing.T) {
	t.Parallel()

	p := okProvider()
	srv := newTestServer(t, p)

	rec := do(t, srv, http.MethodPost, "/v1/chat", `{
		"model": "some-model",
		"system": "be terse",
		"max_tokens": 100,
		"messages": [{"role": "user", "text": "hi"}]
	}`, testToken)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body)
	}

	var got ChatResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	if got.Text != "hello there" {
		t.Errorf("text = %q, want %q", got.Text, "hello there")
	}
	if got.Provider != "fake" || got.Model != "some-model" {
		t.Errorf("provider/model = %q/%q, want fake/some-model", got.Provider, got.Model)
	}
	if got.Usage.InputTokens != 5 || got.Usage.OutputTokens != 2 {
		t.Errorf("usage = %+v, want 5/2", got.Usage)
	}
	// Raw is off by default: it can echo request content back to a caller.
	if got.Raw != nil {
		t.Errorf("raw = %s, want it withheld unless IncludeRaw is set", got.Raw)
	}

	if p.lastReq.System != "be terse" {
		t.Errorf("system = %q, want it forwarded", p.lastReq.System)
	}
	if p.lastReq.Model != "some-model" {
		t.Errorf("model = %q, want it passed through untouched", p.lastReq.Model)
	}
}

func TestHandleChatValidation(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t, okProvider())

	tests := []struct {
		name string
		body string
		want int
	}{
		{"not JSON", `{{{`, http.StatusBadRequest},
		{"unknown field", `{"model":"m","messages":[],"bogus":1}`, http.StatusBadRequest},
		{"no model", `{"messages":[{"role":"user","text":"hi"}]}`, http.StatusBadRequest},
		{"no messages", `{"model":"m","messages":[]}`, http.StatusBadRequest},
		{"unknown role", `{"model":"m","messages":[{"role":"wizard","text":"hi"}]}`, http.StatusBadRequest},
		{
			"tool message without call ID",
			`{"model":"m","messages":[{"role":"tool","content":[{"type":"tool_result","content":"42"}]}]}`,
			http.StatusBadRequest,
		},
		{
			"unknown provider",
			`{"provider":"nope","model":"m","messages":[{"role":"user","text":"hi"}]}`,
			http.StatusNotFound,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rec := do(t, srv, http.MethodPost, "/v1/chat", tc.body, testToken)
			if rec.Code != tc.want {
				t.Errorf("status = %d, want %d (body: %s)", rec.Code, tc.want, rec.Body)
			}
		})
	}
}

func TestUpstreamErrorMapping(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		err      error
		want     int
		wantKind string
	}{
		{"rate limit", skyl.NewError("fake", 429, skyl.ErrRateLimit, "slow", nil),
			http.StatusTooManyRequests, "rate_limit"},
		{"not found", skyl.NewError("fake", 404, skyl.ErrNotFound, "no model", nil),
			http.StatusNotFound, "not_found"},
		{"bad request", skyl.NewError("fake", 400, skyl.ErrBadRequest, "bad", nil),
			http.StatusBadRequest, "bad_request"},
		{"refusal", skyl.NewError("fake", 200, skyl.ErrRefusal, "declined", nil),
			http.StatusUnprocessableEntity, "refusal"},
		{"server error", skyl.NewError("fake", 500, skyl.ErrServer, "boom", nil),
			http.StatusBadGateway, "server"},
		{"our credential rejected", skyl.NewError("fake", 401, skyl.ErrAuth, "bad key", nil),
			http.StatusBadGateway, "auth"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			srv := newTestServer(t, &stubProvider{name: "fake", err: tc.err})
			rec := do(t, srv, http.MethodPost, "/v1/chat",
				`{"model":"m","messages":[{"role":"user","text":"hi"}]}`, testToken)

			if rec.Code != tc.want {
				t.Errorf("status = %d, want %d", rec.Code, tc.want)
			}
			var body ErrorResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("error body is not JSON: %v", err)
			}
			if body.Kind != tc.wantKind {
				t.Errorf("kind = %q, want %q", body.Kind, tc.wantKind)
			}
		})
	}
}

func TestUpstreamAuthFailureIsNotReportedAs401(t *testing.T) {
	t.Parallel()

	// A 401 would tell the caller *their* token was rejected. It was not —
	// ours was. Returning 502 keeps the two failures distinguishable.
	srv := newTestServer(t, &stubProvider{
		name: "fake",
		err:  skyl.NewError("fake", 401, skyl.ErrAuth, "bad provider key", nil),
	})
	rec := do(t, srv, http.MethodPost, "/v1/chat",
		`{"model":"m","messages":[{"role":"user","text":"hi"}]}`, testToken)

	if rec.Code == http.StatusUnauthorized {
		t.Error("status = 401; an upstream credential failure must not look like a client auth failure")
	}
}

func TestHandleModels(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t, &stubProvider{
		name: "fake",
		models: []skyl.ModelInfo{
			{ID: "m1", DisplayName: "Model One", ContextWindow: 1000},
			{ID: "m2"},
		},
	})

	rec := do(t, srv, http.MethodGet, "/v1/models?provider=fake", "", testToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body)
	}

	var got ModelsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	if got.Provider != "fake" || len(got.Models) != 2 {
		t.Fatalf("response = %+v, want two models from fake", got)
	}
	if got.Models[0].ID != "m1" || got.Models[0].ContextWindow != 1000 {
		t.Errorf("model[0] = %+v, want m1 with a 1000 window", got.Models[0])
	}
}

func TestHandleChatStream(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t, &stubProvider{
		name: "fake",
		streamEvts: []skyl.StreamEvent{
			{Type: skyl.EventTextDelta, Text: "Hello"},
			{Type: skyl.EventTextDelta, Text: ", world"},
			{Type: skyl.EventDone, StopReason: skyl.StopEndTurn,
				Usage: &skyl.Usage{InputTokens: 3, OutputTokens: 4}},
		},
	})

	rec := do(t, srv, http.MethodPost, "/v1/chat/stream",
		`{"model":"m","messages":[{"role":"user","text":"hi"}]}`, testToken)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}

	var (
		text  bytes.Buffer
		saw   int
		final map[string]any
	)
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		payload, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue
		}
		saw++
		var ev map[string]any
		if err := json.Unmarshal([]byte(payload), &ev); err != nil {
			t.Fatalf("frame is not JSON: %v (%s)", err, payload)
		}
		switch ev["type"] {
		case "text_delta":
			text.WriteString(ev["text"].(string))
		case "done":
			final = ev
		}
	}

	if saw != 3 {
		t.Errorf("saw %d SSE frames, want 3", saw)
	}
	if text.String() != "Hello, world" {
		t.Errorf("streamed text = %q, want %q", text.String(), "Hello, world")
	}
	if final == nil {
		t.Fatal("no done frame")
	}
	if final["stop_reason"] != "end_turn" {
		t.Errorf("stop_reason = %v, want end_turn", final["stop_reason"])
	}
}

func TestStreamHandshakeErrorUsesHTTPStatus(t *testing.T) {
	t.Parallel()

	// Nothing has been written yet, so a handshake failure can still be a
	// proper status code rather than an in-band SSE error.
	srv := newTestServer(t, &stubProvider{
		name: "fake",
		err:  skyl.NewError("fake", 429, skyl.ErrRateLimit, "slow down", nil),
	})
	rec := do(t, srv, http.MethodPost, "/v1/chat/stream",
		`{"model":"m","messages":[{"role":"user","text":"hi"}]}`, testToken)

	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429", rec.Code)
	}
}

func TestStreamMidFlightErrorRidesTheStream(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t, &stubProvider{
		name:       "fake",
		streamEvts: []skyl.StreamEvent{{Type: skyl.EventTextDelta, Text: "partial"}},
		streamErr:  skyl.NewError("fake", 500, skyl.ErrServer, "died", nil),
	})

	rec := do(t, srv, http.MethodPost, "/v1/chat/stream",
		`{"model":"m","messages":[{"role":"user","text":"hi"}]}`, testToken)

	// Headers were already flushed, so the status stays 200 and the failure
	// must be reported in-band.
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"type":"error"`) {
		t.Errorf("body = %q, want an in-band error frame", rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"kind":"server"`) {
		t.Errorf("body = %q, want the classification in the error frame", rec.Body)
	}
}

func TestIncludeRawIsOptIn(t *testing.T) {
	t.Parallel()

	p := okProvider()
	srv, err := NewServer(Config{
		Providers:  map[string]*skyl.Client{"fake": skyl.New(p)},
		AuthToken:  testToken,
		IncludeRaw: true,
		Logger:     slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	rec := do(t, srv, http.MethodPost, "/v1/chat",
		`{"model":"m","messages":[{"role":"user","text":"hi"}]}`, testToken)

	var got ChatResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	if got.Raw == nil {
		t.Error("raw is absent even though IncludeRaw is set")
	}
}

func TestToolResultMessageIsForwarded(t *testing.T) {
	t.Parallel()

	p := okProvider()
	srv := newTestServer(t, p)

	rec := do(t, srv, http.MethodPost, "/v1/chat", `{
		"model": "m",
		"messages": [
			{"role": "user", "text": "weather?"},
			{"role": "tool", "content": [
				{"type": "tool_result", "tool_call_id": "call_1", "content": "22C"}
			]}
		]
	}`, testToken)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body)
	}
	last := p.lastReq.Messages[len(p.lastReq.Messages)-1]
	if last.Role != skyl.RoleTool {
		t.Errorf("role = %q, want tool", last.Role)
	}
	tr, ok := last.Parts[0].(skyl.ToolResult)
	if !ok || tr.CallID != "call_1" {
		t.Errorf("part = %+v, want a ToolResult for call_1", last.Parts[0])
	}
}
