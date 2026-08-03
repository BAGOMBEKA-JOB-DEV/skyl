package anthropic_test

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
	"github.com/BAGOMBEKA-JOB-DEV/skyl/internal/providertest"
	"github.com/BAGOMBEKA-JOB-DEV/skyl/provider/anthropic"
)

const testKey = "sk-ant-test-key"

const successBody = `{
	"id": "msg_1",
	"type": "message",
	"role": "assistant",
	"model": "served-model-0613",
	"content": [{"type": "text", "text": "hi there"}],
	"stop_reason": "end_turn",
	"stop_sequence": null,
	"usage": {"input_tokens": 9, "output_tokens": 3}
}`

const errorBody = `{"type": "error", "error": {"type": "invalid_request_error", "message": "something went wrong"}}`

// TestContract runs the shared adapter contract. Streaming is covered
// separately below, because Anthropic's SSE stream is a sequence of named
// events rather than the bare `data:` frames the shared suite emits.
func TestContract(t *testing.T) {
	t.Parallel()

	providertest.Suite{
		Name:   "anthropic",
		APIKey: testKey,
		New: func(baseURL string) skyl.Provider {
			return anthropic.New(testKey, anthropic.WithBaseURL(baseURL))
		},
		SuccessBody: successBody,
		WantText:    "hi there",
		WantModel:   "served-model-0613",
		ErrorBody:   errorBody,
		SkipStream:  true,
	}.Run(t)
}

func newTestProvider(t *testing.T, h http.HandlerFunc) *anthropic.Provider {
	t.Helper()
	// Wrap the handler so every response declares JSON, matching what a real
	// provider sends; the Anthropic SDK refuses to decode a body without it.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if w.Header().Get("Content-Type") == "" {
			w.Header().Set("Content-Type", "application/json")
		}
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	return anthropic.New(testKey, anthropic.WithBaseURL(srv.URL))
}

func basicRequest() *skyl.Request {
	return &skyl.Request{
		Model:     "claude-opus-5",
		MaxTokens: 100,
		Messages:  []skyl.Message{skyl.UserText("hello")},
	}
}

func TestCompleteMapsRequest(t *testing.T) {
	t.Parallel()

	var captured map[string]any
	p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &captured)

		if got := r.Header.Get("X-Api-Key"); got != testKey {
			t.Errorf("x-api-key = %q, want the configured key", got)
		}
		_, _ = io.WriteString(w, successBody)
	})

	req := basicRequest()
	req.System = "be terse"
	req.Stop = []string{"END"}

	resp, err := p.Complete(context.Background(), req)
	if err != nil {
		t.Fatalf("Complete() error = %v", err)
	}

	// Anthropic takes the system prompt as a top-level field, not a message.
	sys, ok := captured["system"].([]any)
	if !ok || len(sys) == 0 {
		t.Fatalf("system = %v, want a top-level system block", captured["system"])
	}
	if got := sys[0].(map[string]any)["text"]; got != "be terse" {
		t.Errorf("system text = %v, want %q", got, "be terse")
	}
	if captured["model"] != "claude-opus-5" {
		t.Errorf("model = %v, want it passed through untouched", captured["model"])
	}
	if captured["max_tokens"] != float64(100) {
		t.Errorf("max_tokens = %v, want 100", captured["max_tokens"])
	}
	if resp.StopReason != skyl.StopEndTurn {
		t.Errorf("StopReason = %q, want %q", resp.StopReason, skyl.StopEndTurn)
	}
	if resp.Usage.InputTokens != 9 || resp.Usage.OutputTokens != 3 {
		t.Errorf("Usage = %+v, want 9/3", resp.Usage)
	}
}

// The API requires max_tokens, so skyl must supply a default rather than fail
// a request every other provider would accept.
func TestCompleteSuppliesDefaultMaxTokens(t *testing.T) {
	t.Parallel()

	var captured map[string]any
	p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &captured)
		_, _ = io.WriteString(w, successBody)
	})

	req := basicRequest()
	req.MaxTokens = 0

	if _, err := p.Complete(context.Background(), req); err != nil {
		t.Fatalf("Complete() error = %v", err)
	}
	if got, _ := captured["max_tokens"].(float64); got <= 0 {
		t.Errorf("max_tokens = %v, want a positive default", captured["max_tokens"])
	}
}

func TestToolCallRoundTrip(t *testing.T) {
	t.Parallel()

	p := newTestProvider(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{
			"id": "msg_2", "type": "message", "role": "assistant",
			"model": "claude-opus-5",
			"content": [{"type": "tool_use", "id": "toolu_1", "name": "get_weather",
			             "input": {"city": "Kampala"}}],
			"stop_reason": "tool_use",
			"usage": {"input_tokens": 5, "output_tokens": 2}
		}`)
	})

	resp, err := p.Complete(context.Background(), basicRequest())
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
	if calls[0].ID != "toolu_1" || calls[0].Name != "get_weather" {
		t.Errorf("call = %+v, want toolu_1/get_weather", calls[0])
	}

	var args map[string]string
	if err := json.Unmarshal(calls[0].Arguments, &args); err != nil {
		t.Fatalf("arguments are not valid JSON: %v (%s)", err, calls[0].Arguments)
	}
	if args["city"] != "Kampala" {
		t.Errorf("arguments = %v, want city=Kampala", args)
	}
}

// Anthropic has no tool role: results are user-turn content blocks.
func TestToolResultBecomesUserContent(t *testing.T) {
	t.Parallel()

	var captured map[string]any
	p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &captured)
		_, _ = io.WriteString(w, successBody)
	})

	req := basicRequest()
	req.Messages = append(req.Messages, skyl.ToolResultMessage("toolu_1", "22C and sunny"))

	if _, err := p.Complete(context.Background(), req); err != nil {
		t.Fatalf("Complete() error = %v", err)
	}

	msgs := captured["messages"].([]any)
	last := msgs[len(msgs)-1].(map[string]any)
	if last["role"] != "user" {
		t.Errorf("role = %v, want user — Anthropic has no tool role", last["role"])
	}
	blocks := last["content"].([]any)
	if blocks[0].(map[string]any)["type"] != "tool_result" {
		t.Errorf("block = %v, want a tool_result", blocks[0])
	}
}

func TestUnsupportedPartIsRejectedNotDropped(t *testing.T) {
	t.Parallel()

	p := newTestProvider(t, func(http.ResponseWriter, *http.Request) {
		t.Error("provider was called; an unrepresentable request must fail locally")
	})

	req := basicRequest()
	req.Messages = []skyl.Message{{
		Role: skyl.RoleAssistant,
		Parts: []skyl.Part{skyl.ToolCall{
			ID: "t1", Name: "x", Arguments: json.RawMessage(`not json`),
		}},
	}}

	_, err := p.Complete(context.Background(), req)
	if !errors.Is(err, skyl.ErrUnsupported) {
		t.Errorf("err = %v, want ErrUnsupported rather than silently dropping the part", err)
	}
}

// Anthropic streams named SSE events, so this is exercised here rather than
// through the shared contract suite.
func TestStream(t *testing.T) {
	t.Parallel()

	frames := []string{
		`event: message_start
data: {"type":"message_start","message":{"id":"msg_3","type":"message","role":"assistant","model":"claude-opus-5","content":[],"stop_reason":null,"usage":{"input_tokens":5,"output_tokens":0}}}`,
		`event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}`,
		`event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":", world"}}`,
		`event: content_block_stop
data: {"type":"content_block_stop","index":0}`,
		`event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":7}}`,
		`event: message_stop
data: {"type":"message_stop"}`,
	}

	p := newTestProvider(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, f := range frames {
			_, _ = io.WriteString(w, f+"\n\n")
			if fl, ok := w.(http.Flusher); ok {
				fl.Flush()
			}
		}
	})

	stream, err := p.Stream(context.Background(), basicRequest())
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	defer stream.Close() //nolint:errcheck // test cleanup

	var (
		text strings.Builder
		last skyl.StreamEvent
	)
	for stream.Next() {
		ev := stream.Event()
		last = ev
		if ev.Type == skyl.EventTextDelta {
			text.WriteString(ev.Text)
		}
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("Err() = %v, want nil", err)
	}
	if text.String() != "Hello, world" {
		t.Errorf("streamed text = %q, want %q", text.String(), "Hello, world")
	}
	if last.Type != skyl.EventDone {
		t.Errorf("final event = %q, want %q", last.Type, skyl.EventDone)
	}
	if last.StopReason != skyl.StopEndTurn {
		t.Errorf("StopReason = %q, want %q", last.StopReason, skyl.StopEndTurn)
	}
}

func TestStreamHandshakeErrorIsClassified(t *testing.T) {
	t.Parallel()

	p := newTestProvider(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, errorBody)
	})

	_, err := p.Stream(context.Background(), basicRequest())
	if !errors.Is(err, skyl.ErrRateLimit) {
		t.Errorf("err = %v, want ErrRateLimit", err)
	}
	if err != nil && strings.Contains(err.Error(), testKey) {
		t.Error("the API key leaked into the error")
	}
}

func TestName(t *testing.T) {
	t.Parallel()

	if got := anthropic.New("k").Name(); got != "anthropic" {
		t.Errorf("Name() = %q, want anthropic", got)
	}
}
