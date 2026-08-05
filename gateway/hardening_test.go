package gateway

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/BAGOMBEKA-JOB-DEV/skyl"
)

// Readiness and liveness answer different questions, and conflating them is
// what makes a rolling deploy drop requests.
func TestReadinessFailsWhileDrainingButLivenessDoesNot(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t, okProvider())

	if rec := do(t, srv, http.MethodGet, "/readyz", "", ""); rec.Code != http.StatusOK {
		t.Errorf("/readyz = %d before draining, want 200", rec.Code)
	}

	srv.StartDraining()

	if rec := do(t, srv, http.MethodGet, "/readyz", "", ""); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("/readyz = %d while draining, want 503 so traffic stops arriving", rec.Code)
	}
	// Liveness must stay green: the process is alive and finishing work, and a
	// failing liveness probe would have the orchestrator kill it mid-request.
	if rec := do(t, srv, http.MethodGet, "/healthz", "", ""); rec.Code != http.StatusOK {
		t.Errorf("/healthz = %d while draining, want 200 — draining is not dying", rec.Code)
	}
}

// Rotation is the point: two tokens valid at once means the old one can be
// retired after callers move, instead of every rotation being an outage.
func TestMultipleAuthTokensAreAccepted(t *testing.T) {
	t.Parallel()

	srv, err := NewServer(Config{
		Providers:  map[string]*skyl.Client{"fake": skyl.New(okProvider())},
		AuthToken:  testToken,
		AuthTokens: map[string]string{"rotating": "second-token", "ci": "third-token"},
		Logger:     slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	for _, tok := range []string{testToken, "second-token", "third-token"} {
		if rec := do(t, srv, http.MethodGet, "/v1/providers", "", tok); rec.Code != http.StatusOK {
			t.Errorf("token %q = %d, want 200", tok, rec.Code)
		}
	}
	if rec := do(t, srv, http.MethodGet, "/v1/providers", "", "retired"); rec.Code != http.StatusUnauthorized {
		t.Errorf("unknown token = %d, want 401", rec.Code)
	}
}

func TestEmptyRotationTokenIsRejectedAtStartup(t *testing.T) {
	t.Parallel()

	_, err := NewServer(Config{
		Providers:  map[string]*skyl.Client{"fake": skyl.New(okProvider())},
		AuthToken:  testToken,
		AuthTokens: map[string]string{"broken": ""},
		Logger:     slog.New(slog.DiscardHandler),
	})
	if err == nil {
		t.Fatal("NewServer() error = nil for an empty rotation token")
	}
}

// The caller needs the request ID to quote when reporting a problem. chi puts
// it in the context and the log line, and never on the response.
func TestRequestIDIsEchoedToTheCaller(t *testing.T) {
	t.Parallel()

	rec := do(t, newTestServer(t, okProvider()), http.MethodGet, "/healthz", "", "")
	if rec.Header().Get("X-Request-Id") == "" {
		t.Error("X-Request-Id is absent; the caller has nothing to correlate with")
	}
}

func TestCORSPreflightIsAnsweredWithoutACredential(t *testing.T) {
	t.Parallel()

	srv, err := NewServer(Config{
		Providers:      map[string]*skyl.Client{"fake": skyl.New(okProvider())},
		AuthToken:      testToken,
		AllowedOrigins: []string{"https://app.example.com"},
		Logger:         slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	req := httptest.NewRequest(http.MethodOptions, "/v1/chat", nil)
	req.Header.Set("Origin", "https://app.example.com")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	// A preflight carries no Authorization header, so answering it must not
	// require one — authenticating it would reject every browser client.
	if rec.Code != http.StatusNoContent {
		t.Errorf("preflight = %d, want 204", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://app.example.com" {
		t.Errorf("Allow-Origin = %q, want the echoed origin", got)
	}
	// A wildcard cannot carry credentials, so the specific origin is required.
	if rec.Header().Get("Access-Control-Allow-Origin") == "*" {
		t.Error("Allow-Origin is a wildcard; credentials are involved")
	}

	// An origin that was not configured gets no CORS headers at all.
	other := httptest.NewRequest(http.MethodOptions, "/v1/chat", nil)
	other.Header.Set("Origin", "https://evil.example.com")
	otherRec := httptest.NewRecorder()
	srv.ServeHTTP(otherRec, other)
	if got := otherRec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("Allow-Origin = %q for an unlisted origin, want empty", got)
	}
}

// nginx buffers proxied responses by default, which turns a stream into one
// delivery and removes the property the endpoint exists for.
func TestStreamDisablesProxyBuffering(t *testing.T) {
	t.Parallel()

	p := okProvider()
	p.streamEvts = []skyl.StreamEvent{{Type: skyl.EventDone}}
	srv := newTestServer(t, p)

	rec := do(t, srv, http.MethodPost, "/v1/chat/stream",
		`{"model":"m","messages":[{"role":"user","text":"hi"}]}`, testToken)

	if got := rec.Header().Get("X-Accel-Buffering"); got != "no" {
		t.Errorf("X-Accel-Buffering = %q, want %q", got, "no")
	}
}

// ---------------------------------------------------------------------------
// Wire format
// ---------------------------------------------------------------------------

// The assistant turn must round-trip. Before this, ChatMessage could not carry
// a tool call, so a client received one in the response and had no way to send
// it back — and a provider rejects a tool result that does not follow its call.
func TestAssistantToolCallRoundTrips(t *testing.T) {
	t.Parallel()

	p := okProvider()
	p.resp = &skyl.Response{
		Provider: "fake", Model: "m",
		Message: skyl.Message{Role: skyl.RoleAssistant, Parts: []skyl.Part{
			skyl.ToolCall{ID: "call_1", Name: "get_weather", Arguments: json.RawMessage(`{"city":"Kampala"}`)},
		}},
		StopReason: skyl.StopToolUse,
	}
	srv := newTestServer(t, p)

	rec := do(t, srv, http.MethodPost, "/v1/chat",
		`{"model":"m","messages":[{"role":"user","text":"weather?"}]}`, testToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (body %s)", rec.Code, rec.Body)
	}

	var resp ChatResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if len(resp.Message.Content) != 1 || resp.Message.Content[0].Type != PartToolCall {
		t.Fatalf("Message.Content = %+v, want one tool_call part", resp.Message.Content)
	}

	// Now send that exact turn back, which is what a tool loop does.
	replay, err := json.Marshal(ChatRequest{
		Model: "m",
		Messages: []ChatMessage{
			{Role: "user", Text: "weather?"},
			resp.Message,
			{Role: "tool", Content: []ChatPart{{
				Type: PartToolResult, ToolCallID: "call_1", Content: "22C",
			}}},
		},
	})
	if err != nil {
		t.Fatalf("encoding replay: %v", err)
	}

	rec = do(t, srv, http.MethodPost, "/v1/chat", string(replay), testToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("replay status = %d (body %s)", rec.Code, rec.Body)
	}

	// The assistant turn must have reached skyl as a ToolCall part, not as
	// text — that is the bug this format exists to fix.
	got := p.lastReq.Messages[1]
	if got.Role != skyl.RoleAssistant {
		t.Fatalf("replayed role = %q, want assistant", got.Role)
	}
	call, ok := got.Parts[0].(skyl.ToolCall)
	if !ok {
		t.Fatalf("replayed part = %T, want skyl.ToolCall", got.Parts[0])
	}
	if call.ID != "call_1" || call.Name != "get_weather" {
		t.Errorf("replayed call = %+v, want call_1/get_weather", call)
	}
}

func TestToolErrorIsMarked(t *testing.T) {
	t.Parallel()

	p := okProvider()
	srv := newTestServer(t, p)

	body := `{"model":"m","messages":[
		{"role":"user","text":"weather?"},
		{"role":"tool","content":[{"type":"tool_result","tool_call_id":"c1","content":"boom","is_error":true}]}
	]}`
	if rec := do(t, srv, http.MethodPost, "/v1/chat", body, testToken); rec.Code != http.StatusOK {
		t.Fatalf("status = %d (body %s)", rec.Code, rec.Body)
	}

	result := p.lastReq.Messages[1].Parts[0].(skyl.ToolResult)
	if !result.IsError {
		t.Error("IsError = false; a failed tool reported as a successful one lets the model build on nothing")
	}
}

func TestImagePartIsDecoded(t *testing.T) {
	t.Parallel()

	p := okProvider()
	srv := newTestServer(t, p)

	data := base64.StdEncoding.EncodeToString([]byte{0x89, 0x50})
	body := `{"model":"m","messages":[{"role":"user","content":[
		{"type":"image","media_type":"image/png","data":"` + data + `"},
		{"type":"text","text":"what is this?"}
	]}]}`

	if rec := do(t, srv, http.MethodPost, "/v1/chat", body, testToken); rec.Code != http.StatusOK {
		t.Fatalf("status = %d (body %s)", rec.Code, rec.Body)
	}

	parts := p.lastReq.Messages[0].Parts
	if len(parts) != 2 {
		t.Fatalf("got %d parts, want 2 — order carries meaning", len(parts))
	}
	img, ok := parts[0].(skyl.Image)
	if !ok {
		t.Fatalf("parts[0] = %T, want skyl.Image", parts[0])
	}
	if img.MediaType != "image/png" || len(img.Data) != 2 {
		t.Errorf("image = %+v, want png with 2 decoded bytes", img)
	}
	if _, ok := parts[1].(skyl.Text); !ok {
		t.Errorf("parts[1] = %T, want the caption to survive", parts[1])
	}
}

func TestToolChoiceAndThinkingReachSkyl(t *testing.T) {
	t.Parallel()

	p := okProvider()
	srv := newTestServer(t, p)

	body := `{"model":"m","messages":[{"role":"user","text":"hi"}],
		"tools":[{"name":"ping"}],
		"tool_choice":{"mode":"tool","name":"ping"},
		"thinking":{"enabled":true,"effort":"high"}}`

	if rec := do(t, srv, http.MethodPost, "/v1/chat", body, testToken); rec.Code != http.StatusOK {
		t.Fatalf("status = %d (body %s)", rec.Code, rec.Body)
	}

	if p.lastReq.ToolChoice == nil || p.lastReq.ToolChoice.Mode != skyl.ToolChoiceSpecific {
		t.Errorf("ToolChoice = %+v, want the specific mode", p.lastReq.ToolChoice)
	}
	if p.lastReq.Thinking == nil || p.lastReq.Thinking.Effort != skyl.EffortHigh {
		t.Errorf("Thinking = %+v, want enabled at high effort", p.lastReq.Thinking)
	}
}

func TestResponseFormatReachesSkyl(t *testing.T) {
	t.Parallel()

	p := okProvider()
	srv := newTestServer(t, p)

	body := `{"model":"m","messages":[{"role":"user","text":"hi"}],
		"response_format":{"name":"person","schema":{"type":"object",
			"properties":{"name":{"type":"string"}},"required":["name"]}}}`

	if rec := do(t, srv, http.MethodPost, "/v1/chat", body, testToken); rec.Code != http.StatusOK {
		t.Fatalf("status = %d (body %s)", rec.Code, rec.Body)
	}

	rf := p.lastReq.ResponseFormat
	if rf == nil {
		t.Fatal("ResponseFormat is nil; the wire field never reached skyl")
	}
	if rf.Name != "person" {
		t.Errorf("Name = %q, want person", rf.Name)
	}
	// Verbatim, per ADR-0008 — the gateway translates no dialects either.
	if rf.Schema["type"] != "object" {
		t.Errorf("schema was altered in transit: %#v", rf.Schema)
	}
	props, ok := rf.Schema["properties"].(map[string]any)
	if !ok || props["name"] == nil {
		t.Errorf("schema properties were lost: %#v", rf.Schema)
	}
}

// The gateway decodes with DisallowUnknownFields, so a field it does not model
// is a 400 rather than a silent drop. That is the right behaviour and it is
// also why this field had to be added here in the same change as the library.
func TestResponseFormatWithoutASchemaIsRejected(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t, okProvider())

	tests := []struct {
		name string
		body string
	}{
		{
			name: "no schema key",
			body: `{"model":"m","messages":[{"role":"user","text":"hi"}],
				"response_format":{"name":"person"}}`,
		},
		{
			name: "empty schema object",
			body: `{"model":"m","messages":[{"role":"user","text":"hi"}],
				"response_format":{"schema":{}}}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			rec := do(t, srv, http.MethodPost, "/v1/chat", tt.body, testToken)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %s)", rec.Code, rec.Body)
			}
			// The message must name the wire field, not skyl's Go field: an
			// operator reading this only ever saw the JSON they sent.
			if !strings.Contains(rec.Body.String(), "response_format") {
				t.Errorf("error does not name the offending field: %s", rec.Body)
			}
		})
	}
}

func TestMalformedPartsAreRejectedWithAUsefulMessage(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t, okProvider())

	tests := []struct {
		name string
		body string
		want string
	}{
		{
			"image with neither data nor url",
			`{"model":"m","messages":[{"role":"user","content":[{"type":"image"}]}]}`,
			"needs data or url",
		},
		{
			"image data without a media type",
			`{"model":"m","messages":[{"role":"user","content":[{"type":"image","data":"aGk="}]}]}`,
			"media_type",
		},
		{
			"image data that is not base64",
			`{"model":"m","messages":[{"role":"user","content":[{"type":"image","media_type":"image/png","data":"!!!"}]}]}`,
			"base64",
		},
		{
			"tool call without a name",
			`{"model":"m","messages":[{"role":"assistant","content":[{"type":"tool_call","id":"c1"}]}]}`,
			"id and a name",
		},
		{
			"unknown part type",
			`{"model":"m","messages":[{"role":"user","content":[{"type":"video"}]}]}`,
			"unknown part type",
		},
		{
			"both text and content",
			`{"model":"m","messages":[{"role":"user","text":"hi","content":[{"type":"text","text":"hi"}]}]}`,
			"not both",
		},
		{
			"unknown tool choice mode",
			`{"model":"m","messages":[{"role":"user","text":"hi"}],"tool_choice":{"mode":"maybe"}}`,
			"unknown mode",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rec := do(t, srv, http.MethodPost, "/v1/chat", tc.body, testToken)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %s)", rec.Code, rec.Body)
			}
			if !strings.Contains(rec.Body.String(), tc.want) {
				t.Errorf("body = %s, want it to mention %q", rec.Body, tc.want)
			}
		})
	}
}

// A stream that produces nothing for a while must still keep the connection
// alive, or an intermediary reaps it before the first token.
func TestIdleStreamEmitsHeartbeats(t *testing.T) {
	t.Parallel()

	srv, err := NewServer(Config{
		Providers: map[string]*skyl.Client{
			"fake": skyl.New(&slowProvider{delay: 120 * time.Millisecond}),
		},
		AuthToken:         testToken,
		HeartbeatInterval: 20 * time.Millisecond,
		Logger:            slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	rec := do(t, srv, http.MethodPost, "/v1/chat/stream",
		`{"model":"m","messages":[{"role":"user","text":"hi"}]}`, testToken)

	body := rec.Body.String()
	if !strings.Contains(body, ": keep-alive") {
		t.Errorf("no heartbeat in %q; an idle stream would be reaped", body)
	}
	// Heartbeats are SSE comments, so they must not look like data frames.
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, ": ") && strings.Contains(line, "data") {
			t.Errorf("heartbeat %q looks like a data frame", line)
		}
	}
}

// slowProvider returns a stream that stalls before its only event, which is
// what a reasoning model looks like before its first token.
type slowProvider struct{ delay time.Duration }

func (slowProvider) Name() string { return "slow" }

func (slowProvider) Complete(context.Context, *skyl.Request) (*skyl.Response, error) {
	return &skyl.Response{Provider: "slow"}, nil
}

func (p slowProvider) Stream(context.Context, *skyl.Request) (skyl.Stream, error) {
	return &slowStream{delay: p.delay}, nil
}

func (slowProvider) Models(context.Context) ([]skyl.ModelInfo, error) { return nil, nil }

// slowStream delays before producing its single terminal event.
type slowStream struct {
	delay time.Duration
	done  bool
}

func (s *slowStream) Next() bool {
	if s.done {
		return false
	}
	time.Sleep(s.delay)
	s.done = true
	return true
}

func (s *slowStream) Event() skyl.StreamEvent {
	return skyl.StreamEvent{Type: skyl.EventDone, StopReason: skyl.StopEndTurn}
}
func (s *slowStream) Err() error   { return nil }
func (s *slowStream) Close() error { return nil }

// Metrics come from skyl/otel so the gateway and a Go service importing skyl
// directly report the same vocabulary. A gateway-specific metric set would be
// one more thing for an operator to reconcile.
func TestMetricsEndpointServesGenAIConventions(t *testing.T) {
	t.Parallel()

	tel, err := NewTelemetry()
	if err != nil {
		t.Fatalf("NewTelemetry() error = %v", err)
	}

	srv, err := NewServer(Config{
		Providers:      map[string]*skyl.Client{"fake": skyl.New(okProvider(), tel.Hook)},
		AuthToken:      testToken,
		MetricsHandler: tel.Handler,
		Logger:         slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	// Produce some traffic so there is something to report.
	if rec := do(t, srv, http.MethodPost, "/v1/chat",
		`{"model":"m","messages":[{"role":"user","text":"hi"}]}`, testToken); rec.Code != http.StatusOK {
		t.Fatalf("chat = %d (%s)", rec.Code, rec.Body)
	}

	// Scraping needs no credential: an in-cluster scraper has none.
	rec := do(t, srv, http.MethodGet, "/metrics", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("/metrics = %d, want 200", rec.Code)
	}

	body := rec.Body.String()
	// Prometheus renames the dots, so the conventions' gen_ai.client.* becomes
	// gen_ai_client_*.
	for _, want := range []string{"gen_ai_client_operation_duration", "gen_ai_client_token_usage"} {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics does not mention %q", want)
		}
	}
	// No prompt content may reach a metric label.
	if strings.Contains(body, "hi\"") {
		t.Error("prompt text appears in the metrics output")
	}
}

func TestMetricsEndpointIsAbsentWhenNotConfigured(t *testing.T) {
	t.Parallel()

	rec := do(t, newTestServer(t, okProvider()), http.MethodGet, "/metrics", "", "")
	if rec.Code != http.StatusNotFound {
		t.Errorf("/metrics = %d without a handler, want 404", rec.Code)
	}
}
