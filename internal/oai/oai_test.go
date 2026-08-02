package oai

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/BAGOMBEKA-JOB-DEV/skyl"
)

// newTestClient wires a Client to an httptest server. No test in this file
// touches the network.
func newTestClient(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return New(Config{Name: "test", BaseURL: srv.URL, APIKey: "test-key"})
}

func basicRequest() *skyl.Request {
	return &skyl.Request{
		Model:     "some-model",
		MaxTokens: 100,
		Messages:  []skyl.Message{skyl.UserText("hello")},
	}
}

func TestCompleteSuccess(t *testing.T) {
	t.Parallel()

	var captured map[string]any
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &captured)

		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("Authorization = %q, want bearer test-key", got)
		}
		if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			t.Errorf("path = %q, want it to end with /chat/completions", r.URL.Path)
		}

		_, _ = io.WriteString(w, `{
			"id": "chatcmpl-1",
			"model": "some-model-0613",
			"choices": [{"message": {"role": "assistant", "content": "hi there"},
			             "finish_reason": "stop"}],
			"usage": {"prompt_tokens": 9, "completion_tokens": 3,
			          "prompt_tokens_details": {"cached_tokens": 4}}
		}`)
	})

	resp, err := c.Complete(context.Background(), basicRequest())
	if err != nil {
		t.Fatalf("Complete() error = %v", err)
	}

	if resp.Text() != "hi there" {
		t.Errorf("Text() = %q, want %q", resp.Text(), "hi there")
	}
	if resp.ID != "chatcmpl-1" {
		t.Errorf("ID = %q, want chatcmpl-1", resp.ID)
	}
	// The model must come from the response, not be echoed from the request:
	// providers can and do serve a different model than the one asked for.
	if resp.Model != "some-model-0613" {
		t.Errorf("Model = %q, want the model from the response", resp.Model)
	}
	if resp.Provider != "test" {
		t.Errorf("Provider = %q, want test", resp.Provider)
	}
	if resp.StopReason != skyl.StopEndTurn {
		t.Errorf("StopReason = %q, want %q", resp.StopReason, skyl.StopEndTurn)
	}
	if resp.Usage.InputTokens != 9 || resp.Usage.OutputTokens != 3 || resp.Usage.CacheReadTokens != 4 {
		t.Errorf("Usage = %+v, want 9/3/4", resp.Usage)
	}
	if len(resp.Raw) == 0 {
		t.Error("Raw is empty; it is the caller's escape hatch and must always be populated")
	}
	if captured["model"] != "some-model" {
		t.Errorf("request model = %v, want it passed through untouched", captured["model"])
	}
}

func TestCompleteMapsSystemPromptAndOptions(t *testing.T) {
	t.Parallel()

	var captured map[string]any
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &captured)
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`)
	})

	temp := 0.3
	req := basicRequest()
	req.System = "be terse"
	req.Temperature = &temp
	req.Stop = []string{"END"}
	req.ProviderOptions = map[string]any{"top_k": 40, "model": "overridden"}

	if _, err := c.Complete(context.Background(), req); err != nil {
		t.Fatalf("Complete() error = %v", err)
	}

	msgs, ok := captured["messages"].([]any)
	if !ok || len(msgs) != 2 {
		t.Fatalf("messages = %v, want a system message prepended", captured["messages"])
	}
	first := msgs[0].(map[string]any)
	if first["role"] != "system" || first["content"] != "be terse" {
		t.Errorf("first message = %v, want the system prompt", first)
	}
	if captured["temperature"] != 0.3 {
		t.Errorf("temperature = %v, want 0.3", captured["temperature"])
	}
	if captured["max_tokens"] != float64(100) {
		t.Errorf("max_tokens = %v, want 100", captured["max_tokens"])
	}
	// ProviderOptions is an escape hatch: caller values must win.
	if captured["top_k"] != float64(40) {
		t.Errorf("top_k = %v, want ProviderOptions to be merged", captured["top_k"])
	}
	if captured["model"] != "overridden" {
		t.Errorf("model = %v, want ProviderOptions to override skyl's value", captured["model"])
	}
}

func TestCompleteToolCalls(t *testing.T) {
	t.Parallel()

	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{
			"choices": [{"message": {"role": "assistant", "content": null,
			  "tool_calls": [{"id": "call_1", "type": "function",
			    "function": {"name": "get_weather", "arguments": "{\"city\":\"Kampala\"}"}}]},
			  "finish_reason": "tool_calls"}]
		}`)
	})

	resp, err := c.Complete(context.Background(), basicRequest())
	if err != nil {
		t.Fatalf("Complete() error = %v", err)
	}
	if resp.StopReason != skyl.StopToolUse {
		t.Errorf("StopReason = %q, want %q", resp.StopReason, skyl.StopToolUse)
	}

	calls := resp.ToolCalls()
	if len(calls) != 1 {
		t.Fatalf("got %d tool calls, want 1", len(calls))
	}
	if calls[0].ID != "call_1" || calls[0].Name != "get_weather" {
		t.Errorf("call = %+v, want call_1/get_weather", calls[0])
	}
	var args map[string]string
	if err := json.Unmarshal(calls[0].Arguments, &args); err != nil {
		t.Fatalf("arguments are not valid JSON: %v", err)
	}
	if args["city"] != "Kampala" {
		t.Errorf("arguments = %v, want city=Kampala", args)
	}
}

func TestCompleteErrorClassification(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		status     int
		body       string
		retryAfter string
		wantKind   error
		wantMsg    string
	}{
		{"unauthorized", 401, `{"error":{"message":"bad key"}}`, "", skyl.ErrAuth, "bad key"},
		{"forbidden", 403, `{}`, "", skyl.ErrAuth, ""},
		{"not found", 404, `{"error":{"message":"no such model"}}`, "", skyl.ErrNotFound, "no such model"},
		{"rate limited", 429, `{"error":{"message":"slow down"}}`, "7", skyl.ErrRateLimit, "slow down"},
		{"bad request", 400, `{"error":{"message":"bad input"}}`, "", skyl.ErrBadRequest, "bad input"},
		{"server error", 500, `{}`, "", skyl.ErrServer, ""},
		{"overloaded", 529, `{}`, "", skyl.ErrServer, ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
				if tc.retryAfter != "" {
					w.Header().Set("Retry-After", tc.retryAfter)
				}
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			})

			_, err := c.Complete(context.Background(), basicRequest())
			if err == nil {
				t.Fatal("Complete() error = nil, want a failure")
			}
			if !errors.Is(err, tc.wantKind) {
				t.Errorf("err = %v, want it to classify as %v", err, tc.wantKind)
			}
			if tc.wantMsg != "" && !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("err = %q, want it to include the provider message %q", err, tc.wantMsg)
			}

			var e *skyl.Error
			if !errors.As(err, &e) {
				t.Fatal("errors.As did not recover *skyl.Error")
			}
			if e.StatusCode != tc.status {
				t.Errorf("StatusCode = %d, want %d", e.StatusCode, tc.status)
			}
			if tc.retryAfter == "7" && e.RetryAfter == 0 {
				t.Error("RetryAfter = 0, want the header to be parsed")
			}
			if strings.Contains(err.Error(), "test-key") {
				t.Error("the API key leaked into the error message")
			}
		})
	}
}

func TestCompleteRejectsMalformedBody(t *testing.T) {
	t.Parallel()

	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `not json at all`)
	})

	_, err := c.Complete(context.Background(), basicRequest())
	if !errors.Is(err, skyl.ErrServer) {
		t.Errorf("err = %v, want ErrServer for an unparseable body", err)
	}
}

func TestCompleteRejectsEmptyChoices(t *testing.T) {
	t.Parallel()

	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"choices":[]}`)
	})

	if _, err := c.Complete(context.Background(), basicRequest()); err == nil {
		t.Error("Complete() error = nil, want a failure when there are no choices")
	}
}

func TestCompleteHandlesErrorInBodyWith200(t *testing.T) {
	t.Parallel()

	// Some compatible hosts return 200 with an error object.
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"error":{"message":"quota exhausted"}}`)
	})

	_, err := c.Complete(context.Background(), basicRequest())
	if err == nil {
		t.Fatal("Complete() error = nil, want the in-body error to be surfaced")
	}
	if !strings.Contains(err.Error(), "quota exhausted") {
		t.Errorf("err = %q, want the provider message", err)
	}
}

func TestUnsupportedPartsAreRejectedNotDropped(t *testing.T) {
	t.Parallel()

	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("provider was called; an unrepresentable request must fail locally")
	})

	req := basicRequest()
	req.Messages = []skyl.Message{{
		Role:  skyl.RoleAssistant,
		Parts: []skyl.Part{skyl.Image{MediaType: "image/png", Data: []byte{1}}},
	}}

	_, err := c.Complete(context.Background(), req)
	if !errors.Is(err, skyl.ErrUnsupported) {
		t.Errorf("err = %v, want ErrUnsupported — silently dropping data is the worst failure mode", err)
	}
}

func TestConvertToolResultMessage(t *testing.T) {
	t.Parallel()

	var captured map[string]any
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &captured)
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`)
	})

	req := basicRequest()
	req.Messages = append(req.Messages, skyl.ToolResultMessage("call_1", "22C and sunny"))

	if _, err := c.Complete(context.Background(), req); err != nil {
		t.Fatalf("Complete() error = %v", err)
	}

	msgs := captured["messages"].([]any)
	last := msgs[len(msgs)-1].(map[string]any)
	if last["role"] != "tool" {
		t.Errorf("role = %v, want tool", last["role"])
	}
	if last["tool_call_id"] != "call_1" {
		t.Errorf("tool_call_id = %v, want call_1", last["tool_call_id"])
	}
}

func TestModels(t *testing.T) {
	t.Parallel()

	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/models") {
			t.Errorf("path = %q, want /models", r.URL.Path)
		}
		_, _ = io.WriteString(w, `{"data":[
			{"id":"model-a","context_length":128000},
			{"id":"model-b"},
			{"no_id":"skipped"}
		]}`)
	})

	models, err := c.Models(context.Background())
	if err != nil {
		t.Fatalf("Models() error = %v", err)
	}
	if len(models) != 2 {
		t.Fatalf("got %d models, want 2 (the entry without an ID is skipped)", len(models))
	}
	if models[0].ID != "model-a" || models[0].ContextWindow != 128000 {
		t.Errorf("model[0] = %+v, want model-a with a 128k window", models[0])
	}
	if models[0].Provider != "test" {
		t.Errorf("Provider = %q, want test", models[0].Provider)
	}
}

func TestModelsError(t *testing.T) {
	t.Parallel()

	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})

	if _, err := c.Models(context.Background()); !errors.Is(err, skyl.ErrAuth) {
		t.Errorf("err = %v, want ErrAuth", err)
	}
}

// ---------------------------------------------------------------------------
// Streaming
// ---------------------------------------------------------------------------

func sseHandler(frames ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, f := range frames {
			_, _ = io.WriteString(w, "data: "+f+"\n\n")
			if fl, ok := w.(http.Flusher); ok {
				fl.Flush()
			}
		}
	}
}

func drain(t *testing.T, s skyl.Stream) (text string, events []skyl.StreamEvent) {
	t.Helper()
	var b strings.Builder
	for s.Next() {
		ev := s.Event()
		events = append(events, ev)
		if ev.Type == skyl.EventTextDelta {
			b.WriteString(ev.Text)
		}
	}
	return b.String(), events
}

func TestStreamText(t *testing.T) {
	t.Parallel()

	c := newTestClient(t, sseHandler(
		`{"choices":[{"delta":{"content":"Hello"}}]}`,
		`{"choices":[{"delta":{"content":", world"}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":2}}`,
		`[DONE]`,
	))

	s, err := c.Stream(context.Background(), basicRequest())
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	defer s.Close() //nolint:errcheck // test cleanup

	text, events := drain(t, s)
	if err := s.Err(); err != nil {
		t.Fatalf("Err() = %v, want nil", err)
	}
	if text != "Hello, world" {
		t.Errorf("streamed text = %q, want %q", text, "Hello, world")
	}

	last := events[len(events)-1]
	if last.Type != skyl.EventDone {
		t.Fatalf("final event = %q, want %q", last.Type, skyl.EventDone)
	}
	if last.StopReason != skyl.StopEndTurn {
		t.Errorf("StopReason = %q, want %q", last.StopReason, skyl.StopEndTurn)
	}
	if last.Usage == nil || last.Usage.OutputTokens != 2 {
		t.Errorf("Usage = %+v, want output_tokens 2", last.Usage)
	}
}

func TestStreamToolCallsAreEmittedWhole(t *testing.T) {
	t.Parallel()

	// Arguments arrive as partial JSON across several frames; a half-parsed
	// tool call is not actionable, so exactly one whole call must be emitted.
	c := newTestClient(t, sseHandler(
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"get_weather","arguments":"{\"ci"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"ty\":\"Kampala\"}"}}]}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		`[DONE]`,
	))

	s, err := c.Stream(context.Background(), basicRequest())
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	defer s.Close() //nolint:errcheck // test cleanup

	_, events := drain(t, s)
	if err := s.Err(); err != nil {
		t.Fatalf("Err() = %v", err)
	}

	var calls []skyl.ToolCall
	for _, ev := range events {
		if ev.Type == skyl.EventToolCall && ev.ToolCall != nil {
			calls = append(calls, *ev.ToolCall)
		}
	}
	if len(calls) != 1 {
		t.Fatalf("emitted %d tool calls, want exactly 1", len(calls))
	}
	if calls[0].Name != "get_weather" || calls[0].ID != "call_1" {
		t.Errorf("call = %+v, want call_1/get_weather", calls[0])
	}

	var args map[string]string
	if err := json.Unmarshal(calls[0].Arguments, &args); err != nil {
		t.Fatalf("reassembled arguments are not valid JSON: %v (%s)", err, calls[0].Arguments)
	}
	if args["city"] != "Kampala" {
		t.Errorf("arguments = %v, want city=Kampala", args)
	}
}

func TestStreamHandshakeErrorIsClassified(t *testing.T) {
	t.Parallel()

	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"message":"slow down"}}`)
	})

	_, err := c.Stream(context.Background(), basicRequest())
	if !errors.Is(err, skyl.ErrRateLimit) {
		t.Errorf("err = %v, want ErrRateLimit", err)
	}
}

func TestStreamSkipsUnparseableFrames(t *testing.T) {
	t.Parallel()

	// Providers interleave keep-alives and vendor-specific records; those must
	// not kill an otherwise healthy stream.
	c := newTestClient(t, sseHandler(
		`{"choices":[{"delta":{"content":"a"}}]}`,
		`this is not json`,
		`{"choices":[{"delta":{"content":"b"}}]}`,
		`[DONE]`,
	))

	s, err := c.Stream(context.Background(), basicRequest())
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	defer s.Close() //nolint:errcheck // test cleanup

	text, _ := drain(t, s)
	if err := s.Err(); err != nil {
		t.Fatalf("Err() = %v, want nil", err)
	}
	if text != "ab" {
		t.Errorf("text = %q, want %q", text, "ab")
	}
}

func TestStreamCloseIsIdempotentAndSafeEarly(t *testing.T) {
	t.Parallel()

	c := newTestClient(t, sseHandler(
		`{"choices":[{"delta":{"content":"one"}}]}`,
		`{"choices":[{"delta":{"content":"two"}}]}`,
		`[DONE]`,
	))

	s, err := c.Stream(context.Background(), basicRequest())
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}

	// Abandon after a single event, as a caller returning early would.
	if !s.Next() {
		t.Fatal("Next() = false, want at least one event")
	}
	if err := s.Close(); err != nil {
		t.Errorf("first Close() = %v, want nil", err)
	}
	if err := s.Close(); err != nil {
		t.Errorf("second Close() = %v, want nil — Close must be idempotent", err)
	}
}

func TestStreamCancellationStopsPromptly(t *testing.T) {
	t.Parallel()

	c := newTestClient(t, sseHandler(
		`{"choices":[{"delta":{"content":"a"}}]}`,
		`{"choices":[{"delta":{"content":"b"}}]}`,
		`[DONE]`,
	))

	ctx, cancel := context.WithCancel(context.Background())
	s, err := c.Stream(ctx, basicRequest())
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	defer s.Close() //nolint:errcheck // test cleanup

	cancel()
	// Draining a cancelled stream must terminate rather than hang; the -race
	// build and the test timeout together catch a leaked reader.
	for s.Next() {
	}
}
