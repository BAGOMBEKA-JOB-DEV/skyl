// Tests for the sandbox itself.
//
// The sandbox is a test fixture, but it is also the thing the end-to-end suite
// trusts: if it silently stops requiring a credential, or answers a bogus
// model with a 200, every test built on it keeps passing while checking
// nothing. So it gets tested like production code.
package sandbox

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/BAGOMBEKA-JOB-DEV/skyl/internal/testutil"
)

func newServer(t *testing.T, opts ...Option) *httptest.Server {
	t.Helper()

	srv := httptest.NewServer(New(opts...))
	t.Cleanup(srv.Close)
	return srv
}

// do issues a request with the given headers and returns status and body.
func do(t *testing.T, method, url string, headers map[string]string, body any) (int, []byte) {
	t.Helper()

	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		reader = bytes.NewReader(raw)
	}

	req, err := http.NewRequestWithContext(testutil.Context(t), method, url, reader)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck // test cleanup

	out, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, out
}

// ---------------------------------------------------------------------------
// Anthropic
// ---------------------------------------------------------------------------

func anthropicHeaders() map[string]string {
	return map[string]string{
		"x-api-key":         DefaultAPIKey,
		"anthropic-version": "2023-06-01",
		"Content-Type":      "application/json",
	}
}

func anthropicBody(model string, maxTokens int, stream bool) map[string]any {
	body := map[string]any{
		"model": model,
		"messages": []any{map[string]any{
			"role":    "user",
			"content": []any{map[string]any{"type": "text", "text": "What is the capital of France?"}},
		}},
	}
	if maxTokens > 0 {
		body["max_tokens"] = maxTokens
	}
	if stream {
		body["stream"] = true
	}
	return body
}

func TestAnthropicComplete(t *testing.T) {
	srv := newServer(t)

	status, body := do(t, http.MethodPost, srv.URL+"/anthropic/v1/messages",
		anthropicHeaders(), anthropicBody("claude-opus-5", 64, false))
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", status, body)
	}

	var out struct {
		Type    string `json:"type"`
		Role    string `json:"role"`
		Model   string `json:"model"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
		Usage      struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if out.Type != "message" || out.Role != "assistant" {
		t.Errorf("type/role = %q/%q", out.Type, out.Role)
	}
	if len(out.Content) != 1 || out.Content[0].Text != "Paris" {
		t.Errorf("content = %+v, want one text block saying Paris", out.Content)
	}
	if out.StopReason != "end_turn" {
		t.Errorf("stop_reason = %q", out.StopReason)
	}
	if out.Usage.InputTokens == 0 || out.Usage.OutputTokens == 0 {
		t.Errorf("usage = %+v, want non-zero", out.Usage)
	}
}

// TestAnthropicRequiresMaxTokens mirrors the real API, which rejects the
// request outright. The adapter supplies a default, and this is what proves it.
func TestAnthropicRequiresMaxTokens(t *testing.T) {
	srv := newServer(t)

	status, body := do(t, http.MethodPost, srv.URL+"/anthropic/v1/messages",
		anthropicHeaders(), anthropicBody("claude-opus-5", 0, false))
	if status != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 without max_tokens: %s", status, body)
	}
}

func TestAnthropicStreamEventSequence(t *testing.T) {
	srv := newServer(t)

	status, body := do(t, http.MethodPost, srv.URL+"/anthropic/v1/messages",
		anthropicHeaders(), anthropicBody("claude-opus-5", 64, true))
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", status, body)
	}

	var events []string
	for _, line := range strings.Split(string(body), "\n") {
		if name, ok := strings.CutPrefix(line, "event: "); ok {
			events = append(events, name)
		}
	}

	// The SDK accumulates these in order; a missing bookend leaves the message
	// half-built rather than failing loudly.
	if len(events) < 4 {
		t.Fatalf("got %d events, want the full sequence: %v", len(events), events)
	}
	if events[0] != "message_start" {
		t.Errorf("first event = %q, want message_start", events[0])
	}
	if events[len(events)-1] != "message_stop" {
		t.Errorf("last event = %q, want message_stop", events[len(events)-1])
	}
	for _, want := range []string{"content_block_start", "content_block_delta", "content_block_stop", "message_delta"} {
		if !contains(events, want) {
			t.Errorf("event %q missing from %v", want, events)
		}
	}
}

func TestAnthropicModels(t *testing.T) {
	srv := newServer(t)

	status, body := do(t, http.MethodGet, srv.URL+"/anthropic/v1/models", anthropicHeaders(), nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d: %s", status, body)
	}

	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Data) != len(knownModels["anthropic"]) {
		t.Errorf("got %d models, want %d", len(out.Data), len(knownModels["anthropic"]))
	}
}

func TestAnthropicRejectsBadKey(t *testing.T) {
	srv := newServer(t)

	headers := anthropicHeaders()
	headers["x-api-key"] = "wrong"

	status, _ := do(t, http.MethodPost, srv.URL+"/anthropic/v1/messages",
		headers, anthropicBody("claude-opus-5", 64, false))
	if status != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", status)
	}

	status, _ = do(t, http.MethodGet, srv.URL+"/anthropic/v1/models", headers, nil)
	if status != http.StatusUnauthorized {
		t.Errorf("models status = %d, want 401", status)
	}
}

func TestAnthropicRejectsUnknownModel(t *testing.T) {
	srv := newServer(t)

	status, _ := do(t, http.MethodPost, srv.URL+"/anthropic/v1/messages",
		anthropicHeaders(), anthropicBody("no-such-model", 64, false))
	if status != http.StatusNotFound {
		t.Errorf("status = %d, want 404", status)
	}
}

func TestAnthropicMalformedBody(t *testing.T) {
	srv := newServer(t)

	req, err := http.NewRequestWithContext(testutil.Context(t), http.MethodPost,
		srv.URL+"/anthropic/v1/messages", strings.NewReader("{not json"))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	for k, v := range anthropicHeaders() {
		req.Header.Set(k, v)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck // test cleanup

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

// TestAnthropicPlainStringContent covers the other content shape the API
// accepts, so the sandbox does not quietly depend on the adapter always
// sending blocks.
func TestAnthropicPlainStringContent(t *testing.T) {
	srv := newServer(t)

	body := map[string]any{
		"model":      "claude-opus-5",
		"max_tokens": 64,
		"messages": []any{map[string]any{
			"role": "user", "content": "What is the capital of France?",
		}},
	}

	status, raw := do(t, http.MethodPost, srv.URL+"/anthropic/v1/messages",
		anthropicHeaders(), body)
	if status != http.StatusOK {
		t.Fatalf("status = %d: %s", status, raw)
	}
	if !strings.Contains(string(raw), "Paris") {
		t.Errorf("body = %s, want it to contain Paris", raw)
	}
}

// ---------------------------------------------------------------------------
// OpenAI
// ---------------------------------------------------------------------------

func oaiHeaders() map[string]string {
	return map[string]string{
		"Authorization": "Bearer " + DefaultAPIKey,
		"Content-Type":  "application/json",
	}
}

func oaiBody(model string, stream bool) map[string]any {
	body := map[string]any{
		"model": model,
		"messages": []any{map[string]any{
			"role": "user", "content": "What is the capital of France?",
		}},
		"max_completion_tokens": 64,
	}
	if stream {
		body["stream"] = true
	}
	return body
}

func TestOpenAIComplete(t *testing.T) {
	srv := newServer(t)

	status, body := do(t, http.MethodPost, srv.URL+"/openai/v1/chat/completions",
		oaiHeaders(), oaiBody("gpt-5.6", false))
	if status != http.StatusOK {
		t.Fatalf("status = %d: %s", status, body)
	}

	var out struct {
		Object  string `json:"object"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if out.Object != "chat.completion" {
		t.Errorf("object = %q", out.Object)
	}
	if len(out.Choices) != 1 || out.Choices[0].Message.Content != "Paris" {
		t.Errorf("choices = %+v", out.Choices)
	}
	if out.Choices[0].FinishReason != "stop" {
		t.Errorf("finish_reason = %q", out.Choices[0].FinishReason)
	}
	if out.Usage.PromptTokens == 0 || out.Usage.CompletionTokens == 0 {
		t.Errorf("usage = %+v", out.Usage)
	}
}

// TestOpenAIStreamTerminates checks the [DONE] sentinel is present: without
// it a conforming client waits forever.
func TestOpenAIStreamTerminates(t *testing.T) {
	srv := newServer(t)

	status, body := do(t, http.MethodPost, srv.URL+"/openai/v1/chat/completions",
		oaiHeaders(), oaiBody("gpt-5.6", true))
	if status != http.StatusOK {
		t.Fatalf("status = %d: %s", status, body)
	}
	if !strings.Contains(string(body), "data: [DONE]") {
		t.Error("stream did not end with the [DONE] sentinel")
	}
	if !strings.Contains(string(body), `"usage"`) {
		t.Error("stream carried no usage chunk")
	}
}

// TestCompatMountSharesProtocol confirms the second mount behaves identically,
// since provider/openaicompat is pointed at it.
func TestCompatMountSharesProtocol(t *testing.T) {
	srv := newServer(t)

	status, body := do(t, http.MethodPost, srv.URL+"/compat/v1/chat/completions",
		oaiHeaders(), oaiBody("gpt-5.6", false))
	if status != http.StatusOK {
		t.Fatalf("status = %d: %s", status, body)
	}
	if !strings.Contains(string(body), "Paris") {
		t.Errorf("body = %s", body)
	}
}

// TestOpenAIAcceptsNoCredential is the local-runtime behaviour: Ollama and
// llama.cpp need no key, and provider/openaicompat exists to reach them.
func TestOpenAIAcceptsNoCredential(t *testing.T) {
	srv := newServer(t)

	status, body := do(t, http.MethodPost, srv.URL+"/compat/v1/chat/completions",
		map[string]string{"Content-Type": "application/json"}, oaiBody("gpt-5.6", false))
	if status != http.StatusOK {
		t.Fatalf("status = %d without a credential, want 200: %s", status, body)
	}
}

func TestOpenAIRejectsWrongCredential(t *testing.T) {
	srv := newServer(t)

	status, _ := do(t, http.MethodPost, srv.URL+"/openai/v1/chat/completions",
		map[string]string{"Authorization": "Bearer wrong"}, oaiBody("gpt-5.6", false))
	if status != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", status)
	}
}

func TestOpenAIRejectsUnknownModel(t *testing.T) {
	srv := newServer(t)

	status, _ := do(t, http.MethodPost, srv.URL+"/openai/v1/chat/completions",
		oaiHeaders(), oaiBody("no-such-model", false))
	if status != http.StatusNotFound {
		t.Errorf("status = %d, want 404", status)
	}
}

func TestOpenAIModels(t *testing.T) {
	srv := newServer(t)

	status, body := do(t, http.MethodGet, srv.URL+"/openai/v1/models", oaiHeaders(), nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d: %s", status, body)
	}
	if !strings.Contains(string(body), "gpt-5.6") {
		t.Errorf("body = %s", body)
	}
}

// TestOpenAIAcceptsBothMaxTokensSpellings matters because the two adapters
// default differently — provider/openai sends max_completion_tokens and
// provider/openaicompat sends max_tokens.
func TestOpenAIAcceptsBothMaxTokensSpellings(t *testing.T) {
	srv := newServer(t)

	for _, field := range []string{"max_tokens", "max_completion_tokens"} {
		t.Run(field, func(t *testing.T) {
			body := map[string]any{
				"model": "gpt-5.6",
				"messages": []any{map[string]any{
					"role": "user", "content": "What is the capital of France?",
				}},
				field: 64,
			}
			status, raw := do(t, http.MethodPost, srv.URL+"/openai/v1/chat/completions",
				oaiHeaders(), body)
			if status != http.StatusOK {
				t.Errorf("status = %d with %s: %s", status, field, raw)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Gemini
// ---------------------------------------------------------------------------

func geminiHeaders() map[string]string {
	return map[string]string{
		"x-goog-api-key": DefaultAPIKey,
		"Content-Type":   "application/json",
	}
}

func geminiBody() map[string]any {
	return map[string]any{
		"contents": []any{map[string]any{
			"role":  "user",
			"parts": []any{map[string]any{"text": "What is the capital of France?"}},
		}},
	}
}

func TestGeminiComplete(t *testing.T) {
	srv := newServer(t)

	status, body := do(t, http.MethodPost,
		srv.URL+"/gemini/v1beta/models/gemini-3.6-flash:generateContent",
		geminiHeaders(), geminiBody())
	if status != http.StatusOK {
		t.Fatalf("status = %d: %s", status, body)
	}

	var out struct {
		Candidates []struct {
			Content struct {
				Role  string `json:"role"`
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
			FinishReason string `json:"finishReason"`
		} `json:"candidates"`
		UsageMetadata struct {
			PromptTokenCount     int `json:"promptTokenCount"`
			CandidatesTokenCount int `json:"candidatesTokenCount"`
		} `json:"usageMetadata"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if len(out.Candidates) != 1 {
		t.Fatalf("candidates = %+v", out.Candidates)
	}
	c := out.Candidates[0]
	// Gemini names the assistant turn "model", not "assistant".
	if c.Content.Role != "model" {
		t.Errorf("role = %q, want model", c.Content.Role)
	}
	if len(c.Content.Parts) != 1 || c.Content.Parts[0].Text != "Paris" {
		t.Errorf("parts = %+v", c.Content.Parts)
	}
	if c.FinishReason != "STOP" {
		t.Errorf("finishReason = %q", c.FinishReason)
	}
	if out.UsageMetadata.PromptTokenCount == 0 || out.UsageMetadata.CandidatesTokenCount == 0 {
		t.Errorf("usageMetadata = %+v", out.UsageMetadata)
	}
}

func TestGeminiStream(t *testing.T) {
	srv := newServer(t)

	status, body := do(t, http.MethodPost,
		srv.URL+"/gemini/v1beta/models/gemini-3.6-flash:streamGenerateContent?alt=sse",
		geminiHeaders(), geminiBody())
	if status != http.StatusOK {
		t.Fatalf("status = %d: %s", status, body)
	}
	if !strings.Contains(string(body), "data: ") {
		t.Error("stream produced no data frames")
	}
	// Usage rides on the final frame only, matching the real API.
	if !strings.Contains(string(body), "usageMetadata") {
		t.Error("stream never carried usageMetadata")
	}
}

func TestGeminiModels(t *testing.T) {
	srv := newServer(t)

	status, body := do(t, http.MethodGet, srv.URL+"/gemini/v1beta/models?pageSize=1000",
		geminiHeaders(), nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d: %s", status, body)
	}
	// The resource name must be qualified; trimming it is the adapter's job.
	if !strings.Contains(string(body), `"models/gemini-3.6-flash"`) {
		t.Errorf("body = %s, want a qualified resource name", body)
	}
}

func TestGeminiRejectsBadKey(t *testing.T) {
	srv := newServer(t)

	status, _ := do(t, http.MethodPost,
		srv.URL+"/gemini/v1beta/models/gemini-3.6-flash:generateContent",
		map[string]string{"x-goog-api-key": "wrong"}, geminiBody())
	if status != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", status)
	}

	status, _ = do(t, http.MethodGet, srv.URL+"/gemini/v1beta/models",
		map[string]string{"x-goog-api-key": "wrong"}, nil)
	if status != http.StatusUnauthorized {
		t.Errorf("models status = %d, want 401", status)
	}
}

func TestGeminiRejectsUnknownModel(t *testing.T) {
	srv := newServer(t)

	status, _ := do(t, http.MethodPost,
		srv.URL+"/gemini/v1beta/models/no-such-model:generateContent",
		geminiHeaders(), geminiBody())
	if status != http.StatusNotFound {
		t.Errorf("status = %d, want 404", status)
	}
}

func TestGeminiRejectsUnknownMethod(t *testing.T) {
	srv := newServer(t)

	status, _ := do(t, http.MethodPost,
		srv.URL+"/gemini/v1beta/models/gemini-3.6-flash:embedContent",
		geminiHeaders(), geminiBody())
	if status != http.StatusNotFound {
		t.Errorf("status = %d, want 404", status)
	}
}

func TestGeminiRejectsMissingMethod(t *testing.T) {
	srv := newServer(t)

	status, _ := do(t, http.MethodPost,
		srv.URL+"/gemini/v1beta/models/gemini-3.6-flash",
		geminiHeaders(), geminiBody())
	if status != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", status)
	}
}

func TestGeminiMalformedBody(t *testing.T) {
	srv := newServer(t)

	req, err := http.NewRequestWithContext(testutil.Context(t), http.MethodPost,
		srv.URL+"/gemini/v1beta/models/gemini-3.6-flash:generateContent",
		strings.NewReader("{nope"))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	for k, v := range geminiHeaders() {
		req.Header.Set(k, v)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck // test cleanup

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

// ---------------------------------------------------------------------------
// Cross-cutting
// ---------------------------------------------------------------------------

// TestInjectedStatus walks every protocol, because each renders errors in its
// own shape and the adapters classify from those shapes.
func TestInjectedStatus(t *testing.T) {
	srv := newServer(t)

	tests := []struct {
		name    string
		method  string
		url     string
		headers map[string]string
		body    func(model string) any
	}{
		{
			"anthropic", http.MethodPost, srv.URL + "/anthropic/v1/messages", anthropicHeaders(),
			func(m string) any { return anthropicBody(m, 64, false) },
		},
		{
			"openai", http.MethodPost, srv.URL + "/openai/v1/chat/completions", oaiHeaders(),
			func(m string) any { return oaiBody(m, false) },
		},
	}

	for _, tt := range tests {
		for _, code := range []int{400, 401, 429, 500, 503} {
			t.Run(tt.name+"/"+itoa(code), func(t *testing.T) {
				status, _ := do(t, tt.method, tt.url, tt.headers,
					tt.body(StatusModelPrefix+itoa(code)))
				if status != code {
					t.Errorf("status = %d, want the injected %d", status, code)
				}
			})
		}
	}
}

func TestInjectedStatusGemini(t *testing.T) {
	srv := newServer(t)

	for _, code := range []int{429, 500} {
		status, _ := do(t, http.MethodPost,
			srv.URL+"/gemini/v1beta/models/"+StatusModelPrefix+itoa(code)+":generateContent",
			geminiHeaders(), geminiBody())
		if status != code {
			t.Errorf("status = %d, want the injected %d", status, code)
		}
	}
}

// TestRetryAfterOnRateLimit checks the hint is present, since skyl's backoff
// prefers it over the computed delay.
func TestRetryAfterOnRateLimit(t *testing.T) {
	srv := newServer(t)

	req, err := http.NewRequestWithContext(testutil.Context(t), http.MethodPost,
		srv.URL+"/openai/v1/chat/completions", strings.NewReader(`{"model":"`+
			StatusModelPrefix+`429","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+DefaultAPIKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck // test cleanup

	if got := resp.Header.Get("Retry-After"); got == "" {
		t.Error("429 carried no Retry-After header")
	}
}

func TestWithAPIKey(t *testing.T) {
	srv := newServer(t, WithAPIKey("custom-key"))

	headers := anthropicHeaders()
	headers["x-api-key"] = "custom-key"
	status, _ := do(t, http.MethodPost, srv.URL+"/anthropic/v1/messages",
		headers, anthropicBody("claude-opus-5", 64, false))
	if status != http.StatusOK {
		t.Errorf("status = %d with the configured key, want 200", status)
	}

	headers["x-api-key"] = DefaultAPIKey
	status, _ = do(t, http.MethodPost, srv.URL+"/anthropic/v1/messages",
		headers, anthropicBody("claude-opus-5", 64, false))
	if status != http.StatusUnauthorized {
		t.Errorf("status = %d with the default key, want 401 once overridden", status)
	}
}

func TestRequestCounter(t *testing.T) {
	h := New()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	if got := h.Requests(); got != 0 {
		t.Errorf("Requests() = %d before any call, want 0", got)
	}
	for range 3 {
		do(t, http.MethodGet, srv.URL+"/healthz", nil, nil)
	}
	if got := h.Requests(); got != 3 {
		t.Errorf("Requests() = %d, want 3", got)
	}
}

func TestHealthz(t *testing.T) {
	srv := newServer(t)

	status, body := do(t, http.MethodGet, srv.URL+"/healthz", nil, nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	if !strings.Contains(string(body), `"sandbox":true`) {
		t.Errorf("body = %s", body)
	}
}

// ---------------------------------------------------------------------------
// Unit-level helpers
// ---------------------------------------------------------------------------

func TestReply(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"What is the capital of France?", "Paris"},
		{"the CAPITAL OF FRANCE", "Paris"},
		{"", "Hello from the skyl sandbox."},
		{"ping", "You said: ping"},
	}

	for _, tt := range tests {
		if got := reply(tt.in); got != tt.want {
			t.Errorf("reply(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// TestChunkReassembles is the property the streaming tests rely on: the
// concatenated deltas must equal the non-streamed answer exactly, or the two
// paths disagree and one of the assertions is meaningless.
func TestChunkReassembles(t *testing.T) {
	for _, in := range []string{"Paris", "You said: hello there", "a b c d e"} {
		if got := strings.Join(chunk(in), ""); got != in {
			t.Errorf("chunk(%q) reassembles to %q", in, got)
		}
	}
	if got := chunk(""); got != nil {
		t.Errorf("chunk(\"\") = %v, want nil", got)
	}
}

func TestCountTokens(t *testing.T) {
	// Never zero: a response reporting no tokens at all reads as "usage was
	// not parsed" to every test that checks it.
	if got := countTokens(""); got != 1 {
		t.Errorf("countTokens(\"\") = %d, want 1", got)
	}
	if got := countTokens("one two three"); got != 3 {
		t.Errorf("countTokens = %d, want 3", got)
	}
}

func TestInjectedStatusParsing(t *testing.T) {
	tests := []struct {
		in     string
		want   int
		wantOK bool
	}{
		{StatusModelPrefix + "429", 429, true},
		{StatusModelPrefix + "500", 500, true},
		{StatusModelPrefix + "99", 0, false},  // below the valid range
		{StatusModelPrefix + "600", 0, false}, // above it
		{StatusModelPrefix + "abc", 0, false}, // not a number
		{"gpt-5.6", 0, false},                 // an ordinary model
	}

	for _, tt := range tests {
		got, ok := injectedStatus(tt.in)
		if ok != tt.wantOK || got != tt.want {
			t.Errorf("injectedStatus(%q) = %d, %v; want %d, %v",
				tt.in, got, ok, tt.want, tt.wantOK)
		}
	}
}

func TestKnown(t *testing.T) {
	if !known("anthropic", "claude-opus-5") {
		t.Error("claude-opus-5 should be known")
	}
	if known("anthropic", "gpt-5.6") {
		t.Error("a model from another provider's catalogue must not match")
	}
	if known("nonexistent", "anything") {
		t.Error("an unknown provider must have no models")
	}
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// itoa keeps the table tests readable without pulling strconv into a file that
// otherwise has no need for it.
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(buf[pos:])
}
