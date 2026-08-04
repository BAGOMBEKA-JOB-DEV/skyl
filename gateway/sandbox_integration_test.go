//go:build sandbox

// End-to-end: an HTTP client talks to the gateway, which talks to the
// sandbox standing in for a provider.
//
//	cd gateway && go test -tags=sandbox ./...
//
// Every other gateway test injects a stub provider directly, which proves the
// routing and the JSON shapes but stops at the seam. This runs the real chain
// — chi router, auth middleware, skyl.Client, adapter, HTTP transport, SSE
// framing — across two real sockets, which is the arrangement an operator
// actually deploys.
package gateway

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/BAGOMBEKA-JOB-DEV/skyl"
	"github.com/BAGOMBEKA-JOB-DEV/skyl/internal/sandbox"
	"github.com/BAGOMBEKA-JOB-DEV/skyl/provider/openai"
)

// newSandboxGateway wires a gateway to a sandbox-backed provider and returns
// the gateway's URL.
func newSandboxGateway(t *testing.T) string {
	t.Helper()

	box := httptest.NewServer(sandbox.New())
	t.Cleanup(box.Close)

	srv, err := NewServer(Config{
		Providers: map[string]*skyl.Client{
			"openai": skyl.New(openai.New(
				sandbox.DefaultAPIKey,
				openai.WithBaseURL(box.URL+"/openai/v1"),
			)),
		},
		AuthToken: testToken,
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	gw := httptest.NewServer(srv)
	t.Cleanup(gw.Close)
	return gw.URL
}

func post(t *testing.T, url, token string, body any) *http.Response {
	t.Helper()

	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func TestSandboxGatewayChat(t *testing.T) {
	base := newSandboxGateway(t)

	resp := post(t, base+"/v1/chat", testToken, map[string]any{
		"provider": "openai",
		"model":    "gpt-5.6",
		"messages": []any{map[string]any{"role": "user", "text": "What is the capital of France?"}},
	})

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, body)
	}

	var out struct {
		Provider string `json:"provider"`
		Model    string `json:"model"`
		Text     string `json:"text"`
		Usage    struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
		StopReason string `json:"stop_reason"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if out.Text != "Paris" {
		t.Errorf("text = %q, want %q", out.Text, "Paris")
	}
	if out.Provider != "openai" {
		t.Errorf("provider = %q", out.Provider)
	}
	if out.Model != "gpt-5.6" {
		t.Errorf("model = %q", out.Model)
	}
	if out.Usage.InputTokens == 0 || out.Usage.OutputTokens == 0 {
		t.Errorf("usage = %+v, want non-zero on both sides", out.Usage)
	}
	if out.StopReason == "" {
		t.Error("stop_reason is empty")
	}
}

// TestSandboxGatewayStream checks SSE survives both hops. The gateway reads a
// stream from the provider and writes a new one to its own client, so a
// framing mistake in either direction shows up here and nowhere else.
func TestSandboxGatewayStream(t *testing.T) {
	base := newSandboxGateway(t)

	resp := post(t, base+"/v1/chat/stream", testToken, map[string]any{
		"provider": "openai",
		"model":    "gpt-5.6",
		"messages": []any{map[string]any{"role": "user", "text": "What is the capital of France?"}},
	})

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}

	var (
		text    strings.Builder
		sawDone bool
		frames  int
		scanner = bufio.NewScanner(resp.Body)
	)
	for scanner.Scan() {
		line := scanner.Text()
		payload, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue
		}
		frames++

		var ev struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal([]byte(payload), &ev); err != nil {
			t.Fatalf("frame %d is not valid JSON (%s): %v", frames, payload, err)
		}
		switch ev.Type {
		case "text_delta":
			text.WriteString(ev.Text)
		case "done":
			sawDone = true
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("reading stream: %v", err)
	}

	if text.String() != "Paris" {
		t.Errorf("streamed text = %q, want %q", text.String(), "Paris")
	}
	if !sawDone {
		t.Error("stream never carried a terminal event")
	}
}

// TestSandboxGatewayUpstreamError checks a provider failure reaches the client
// as a classified gateway error rather than a 500 that hides the cause.
func TestSandboxGatewayUpstreamError(t *testing.T) {
	base := newSandboxGateway(t)

	resp := post(t, base+"/v1/chat", testToken, map[string]any{
		"provider": "openai",
		"model":    sandbox.StatusModelPrefix + "429",
		"messages": []any{map[string]any{"role": "user", "text": "hi"}},
	})

	if resp.StatusCode != http.StatusTooManyRequests {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 429: %s", resp.StatusCode, body)
	}
}

// TestSandboxGatewayRequiresAuth confirms the token is enforced on the real
// chain, not only against a stub.
func TestSandboxGatewayRequiresAuth(t *testing.T) {
	base := newSandboxGateway(t)

	resp := post(t, base+"/v1/chat", "", map[string]any{
		"provider": "openai",
		"model":    "gpt-5.6",
		"messages": []any{map[string]any{"role": "user", "text": "hi"}},
	})

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 without a token", resp.StatusCode)
	}
}

// TestSandboxGatewayToolLoop is the test the gateway most needed and did not
// have.
//
// The wire format used to be unable to express an assistant turn's tool calls,
// so a caller received one in the response and had no way to send it back —
// and a provider rejects a tool result that does not follow the call it
// answers. Nothing caught it, because no gateway test ever attempted the
// second turn. This one does, over two real sockets.
func TestSandboxGatewayToolLoop(t *testing.T) {
	base := newSandboxGateway(t)

	tool := map[string]any{
		"name":        "get_weather",
		"description": "Look up the current weather for a city.",
		"parameters": map[string]any{
			"type":       "object",
			"properties": map[string]any{"city": map[string]any{"type": "string"}},
		},
	}
	question := map[string]any{"role": "user", "text": "What is the weather in Kampala?"}

	// Turn one: the model asks for the tool.
	resp := post(t, base+"/v1/chat", testToken, map[string]any{
		"model":      "gpt-5.6",
		"max_tokens": 128,
		"tools":      []any{tool},
		"messages":   []any{question},
	})
	defer resp.Body.Close() //nolint:errcheck // test cleanup

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("first turn = %d (%s)", resp.StatusCode, body)
	}

	var first ChatResponse
	if err := json.NewDecoder(resp.Body).Decode(&first); err != nil {
		t.Fatalf("decoding first turn: %v", err)
	}
	if first.StopReason != string(skyl.StopToolUse) {
		t.Errorf("stop_reason = %q, want %q", first.StopReason, skyl.StopToolUse)
	}
	if len(first.ToolCalls) != 1 {
		t.Fatalf("got %d tool calls, want 1 (text %q)", len(first.ToolCalls), first.Text)
	}

	// The assistant turn must come back in a shape that can be replayed. This
	// is the field that did not exist.
	if len(first.Message.Content) == 0 {
		t.Fatal("Message.Content is empty; the assistant turn cannot be replayed")
	}

	// Turn two: run the tool and send the result back, including the assistant
	// turn verbatim.
	second := post(t, base+"/v1/chat", testToken, map[string]any{
		"model":      "gpt-5.6",
		"max_tokens": 128,
		"tools":      []any{tool},
		"messages": []any{
			question,
			first.Message,
			map[string]any{"role": "tool", "content": []any{map[string]any{
				"type":         "tool_result",
				"tool_call_id": first.ToolCalls[0].ID,
				"content":      "22C and sunny",
			}}},
		},
	})
	defer second.Body.Close() //nolint:errcheck // test cleanup

	if second.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(second.Body)
		t.Fatalf("second turn = %d (%s)", second.StatusCode, body)
	}

	var final ChatResponse
	if err := json.NewDecoder(second.Body).Decode(&final); err != nil {
		t.Fatalf("decoding second turn: %v", err)
	}
	// The sandbox echoes the tool's own output, so this proves the result
	// survived both hops rather than that a canned string came back.
	if !strings.Contains(final.Text, "22C and sunny") {
		t.Errorf("second turn text = %q, want it to contain the tool output", final.Text)
	}
}
