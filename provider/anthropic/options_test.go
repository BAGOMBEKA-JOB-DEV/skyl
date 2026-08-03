// Tests for the Anthropic adapter's request mapping and options.
//
// anthropic_test.go covers the happy path and the shared contract. This file
// covers the branches those miss: every tool-choice mode, thinking, sampling
// parameters, both image forms, the full stop-reason table, and the options
// that alter transport. All of it is asserted against the JSON actually sent,
// because the SDK's param types are unions whose zero values encode nothing —
// a mapping that silently produced an empty union would still compile and
// still return a valid response.
package anthropic_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/BAGOMBEKA-JOB-DEV/skyl"
	"github.com/BAGOMBEKA-JOB-DEV/skyl/provider/anthropic"
)

// captureRequest runs one Complete against a fake server and returns the
// decoded request body.
func captureRequest(t *testing.T, req *skyl.Request) map[string]any {
	t.Helper()

	var captured map[string]any
	p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &captured); err != nil {
			t.Errorf("request body is not valid JSON: %v", err)
		}
		_, _ = io.WriteString(w, successBody)
	})

	if _, err := p.Complete(context.Background(), req); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	return captured
}

func TestSamplingParameters(t *testing.T) {
	t.Parallel()

	temp, topP := 0.25, 0.9
	req := basicRequest()
	req.Temperature = &temp
	req.TopP = &topP
	req.Stop = []string{"END", "STOP"}

	got := captureRequest(t, req)

	if got["temperature"] != 0.25 {
		t.Errorf("temperature = %v, want 0.25", got["temperature"])
	}
	if got["top_p"] != 0.9 {
		t.Errorf("top_p = %v, want 0.9", got["top_p"])
	}
	seqs, ok := got["stop_sequences"].([]any)
	if !ok || len(seqs) != 2 || seqs[0] != "END" {
		t.Errorf("stop_sequences = %v, want [END STOP]", got["stop_sequences"])
	}
}

// TestSamplingParametersOmitted checks the pointer fields stay absent when
// unset, rather than being sent as zero. Temperature 0 is a meaningful value,
// so sending it uninvited would change the model's behaviour.
func TestSamplingParametersOmitted(t *testing.T) {
	t.Parallel()

	got := captureRequest(t, basicRequest())

	for _, k := range []string{"temperature", "top_p", "stop_sequences", "tools", "tool_choice", "thinking"} {
		if _, present := got[k]; present {
			t.Errorf("%s was sent despite being unset: %v", k, got[k])
		}
	}
}

func TestToolMapping(t *testing.T) {
	t.Parallel()

	req := basicRequest()
	req.Tools = []skyl.Tool{{
		Name:        "get_weather",
		Description: "Look up the weather",
		Parameters: map[string]any{
			"type":       "object",
			"properties": map[string]any{"city": map[string]any{"type": "string"}},
			"required":   []string{"city"},
		},
	}}

	got := captureRequest(t, req)

	tools, ok := got["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("tools = %v, want one tool", got["tools"])
	}
	tool := tools[0].(map[string]any)
	if tool["name"] != "get_weather" {
		t.Errorf("tool name = %v", tool["name"])
	}
	if tool["description"] != "Look up the weather" {
		t.Errorf("tool description = %v", tool["description"])
	}

	schema, ok := tool["input_schema"].(map[string]any)
	if !ok {
		t.Fatalf("input_schema = %v", tool["input_schema"])
	}
	if _, ok := schema["properties"].(map[string]any)["city"]; !ok {
		t.Errorf("input_schema.properties missing city: %v", schema["properties"])
	}
	req0, ok := schema["required"].([]any)
	if !ok || len(req0) != 1 || req0[0] != "city" {
		t.Errorf("input_schema.required = %v, want [city]", schema["required"])
	}
}

// TestToolWithoutParameters covers the nil-schema branch: a tool that takes no
// arguments must still produce a valid schema rather than a null the API
// rejects.
func TestToolWithoutParameters(t *testing.T) {
	t.Parallel()

	req := basicRequest()
	req.Tools = []skyl.Tool{{Name: "ping"}}

	got := captureRequest(t, req)

	tools := got["tools"].([]any)
	tool := tools[0].(map[string]any)
	if tool["name"] != "ping" {
		t.Errorf("tool name = %v", tool["name"])
	}
	if _, ok := tool["input_schema"]; !ok {
		t.Error("input_schema absent; the API requires it")
	}
	if _, ok := tool["description"]; ok {
		t.Errorf("empty description was sent: %v", tool["description"])
	}
}

// TestToolRequiredFromJSON covers a schema loaded from JSON rather than
// written as a Go literal: json.Unmarshal produces []any, and treating only
// []string as valid silently dropped the required list.
func TestToolRequiredFromJSON(t *testing.T) {
	t.Parallel()

	var params map[string]any
	if err := json.Unmarshal([]byte(`{
		"type": "object",
		"properties": {"city": {"type": "string"}},
		"required": ["city"]
	}`), &params); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	req := basicRequest()
	req.Tools = []skyl.Tool{{Name: "get_weather", Parameters: params}}

	got := captureRequest(t, req)

	tool := got["tools"].([]any)[0].(map[string]any)
	schema := tool["input_schema"].(map[string]any)
	required, ok := schema["required"].([]any)
	if !ok || len(required) != 1 || required[0] != "city" {
		t.Errorf("input_schema.required = %v, want [city]", schema["required"])
	}
}

// TestToolChoiceModes walks every mode. Each maps onto a different SDK union
// member, so one broken case is invisible to a test that only checks "auto".
func TestToolChoiceModes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		mode     skyl.ToolChoiceMode
		name     string
		wantType string
	}{
		{skyl.ToolChoiceAuto, "", "auto"},
		{skyl.ToolChoiceNone, "", "none"},
		{skyl.ToolChoiceRequired, "", "any"},
		{skyl.ToolChoiceSpecific, "get_weather", "tool"},
	}

	for _, tt := range tests {
		t.Run(string(tt.mode), func(t *testing.T) {
			t.Parallel()

			req := basicRequest()
			req.Tools = []skyl.Tool{{Name: "get_weather"}}
			req.ToolChoice = &skyl.ToolChoice{Mode: tt.mode, Name: tt.name}

			got := captureRequest(t, req)

			tc, ok := got["tool_choice"].(map[string]any)
			if !ok {
				t.Fatalf("tool_choice = %v, want an object", got["tool_choice"])
			}
			if tc["type"] != tt.wantType {
				t.Errorf("tool_choice.type = %v, want %q", tc["type"], tt.wantType)
			}
			if tt.wantType == "tool" && tc["name"] != "get_weather" {
				t.Errorf("tool_choice.name = %v, want get_weather", tc["name"])
			}
		})
	}
}

func TestThinking(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		enabled  bool
		wantType string
	}{
		{"enabled", true, "adaptive"},
		{"disabled", false, "disabled"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			req := basicRequest()
			req.Thinking = &skyl.Thinking{Enabled: tt.enabled}

			got := captureRequest(t, req)

			th, ok := got["thinking"].(map[string]any)
			if !ok {
				t.Fatalf("thinking = %v, want an object", got["thinking"])
			}
			if th["type"] != tt.wantType {
				t.Errorf("thinking.type = %v, want %q", th["type"], tt.wantType)
			}
		})
	}
}

// TestImageMapping covers both forms. A URL image must be sent by reference
// rather than being base64'd into an empty payload.
func TestImageMapping(t *testing.T) {
	t.Parallel()

	t.Run("url", func(t *testing.T) {
		t.Parallel()

		req := basicRequest()
		req.Messages = []skyl.Message{{
			Role:  skyl.RoleUser,
			Parts: []skyl.Part{skyl.Image{URL: "https://example.com/cat.png"}},
		}}

		got := captureRequest(t, req)
		src := firstBlockSource(t, got)

		if src["type"] != "url" {
			t.Errorf("image source type = %v, want url", src["type"])
		}
		if src["url"] != "https://example.com/cat.png" {
			t.Errorf("image url = %v", src["url"])
		}
	})

	t.Run("base64", func(t *testing.T) {
		t.Parallel()

		req := basicRequest()
		req.Messages = []skyl.Message{{
			Role: skyl.RoleUser,
			Parts: []skyl.Part{skyl.Image{
				MediaType: "image/png",
				Data:      []byte{0x89, 0x50, 0x4e, 0x47},
			}},
		}}

		got := captureRequest(t, req)
		src := firstBlockSource(t, got)

		if src["type"] != "base64" {
			t.Errorf("image source type = %v, want base64", src["type"])
		}
		if src["media_type"] != "image/png" {
			t.Errorf("media_type = %v", src["media_type"])
		}
		if src["data"] != "iVBORw==" {
			t.Errorf("data = %v, want the base64 of the PNG magic bytes", src["data"])
		}
	})
}

// firstBlockSource digs out messages[0].content[0].source.
func firstBlockSource(t *testing.T, body map[string]any) map[string]any {
	t.Helper()

	msgs, ok := body["messages"].([]any)
	if !ok || len(msgs) == 0 {
		t.Fatalf("messages = %v", body["messages"])
	}
	content, ok := msgs[0].(map[string]any)["content"].([]any)
	if !ok || len(content) == 0 {
		t.Fatalf("content = %v", msgs[0])
	}
	src, ok := content[0].(map[string]any)["source"].(map[string]any)
	if !ok {
		t.Fatalf("source = %v", content[0])
	}
	return src
}

// TestUnknownRoleIsRejected checks an unmappable role fails loudly instead of
// being silently sent as a user turn.
func TestUnknownRoleIsRejected(t *testing.T) {
	t.Parallel()

	p := anthropic.New(testKey, anthropic.WithBaseURL("http://example.invalid"))
	_, err := p.Complete(context.Background(), &skyl.Request{
		Model:     "claude-opus-5",
		MaxTokens: 10,
		Messages: []skyl.Message{{
			Role:  skyl.Role("wizard"),
			Parts: []skyl.Part{skyl.Text{Text: "hi"}},
		}},
	})
	if err == nil {
		t.Fatal("expected an error for an unknown role")
	}
	if !errors.Is(err, skyl.ErrUnsupported) {
		t.Errorf("error = %v, want it classified as unsupported", err)
	}
}

// TestMalformedToolCallArgumentsRejected checks invalid JSON in a tool call is
// caught before the request leaves, rather than producing a confusing 400.
func TestMalformedToolCallArgumentsRejected(t *testing.T) {
	t.Parallel()

	p := anthropic.New(testKey, anthropic.WithBaseURL("http://example.invalid"))
	_, err := p.Complete(context.Background(), &skyl.Request{
		Model:     "claude-opus-5",
		MaxTokens: 10,
		Messages: []skyl.Message{{
			Role: skyl.RoleAssistant,
			Parts: []skyl.Part{skyl.ToolCall{
				ID: "call_1", Name: "f", Arguments: json.RawMessage(`{not json`),
			}},
		}},
	})
	if err == nil {
		t.Fatal("expected an error for malformed tool-call arguments")
	}
	if !errors.Is(err, skyl.ErrUnsupported) {
		t.Errorf("error = %v, want it classified as unsupported", err)
	}
}

// TestStopReasonMapping walks the full table. Anthropic's vocabulary differs
// from skyl's, so each entry is a distinct translation that can break alone.
func TestStopReasonMapping(t *testing.T) {
	t.Parallel()

	tests := []struct {
		wire string
		want skyl.StopReason
	}{
		{"end_turn", skyl.StopEndTurn},
		{"max_tokens", skyl.StopMaxTokens},
		{"model_context_window_exceeded", skyl.StopMaxTokens},
		{"tool_use", skyl.StopToolUse},
		{"stop_sequence", skyl.StopStopSequence},
		{"refusal", skyl.StopRefusal},
		{"something_new", skyl.StopUnknown},
	}

	for _, tt := range tests {
		t.Run(tt.wire, func(t *testing.T) {
			t.Parallel()

			body := `{
				"id": "msg_1", "type": "message", "role": "assistant",
				"model": "claude-opus-5",
				"content": [{"type": "text", "text": "ok"}],
				"stop_reason": "` + tt.wire + `",
				"usage": {"input_tokens": 1, "output_tokens": 1}
			}`

			p := newTestProvider(t, func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, body)
			})

			resp, err := p.Complete(context.Background(), basicRequest())
			if err != nil {
				t.Fatalf("Complete: %v", err)
			}
			if resp.StopReason != tt.want {
				t.Errorf("stop_reason %q mapped to %q, want %q",
					tt.wire, resp.StopReason, tt.want)
			}
		})
	}
}

func TestModels(t *testing.T) {
	t.Parallel()

	p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("path = %q, want /v1/models", r.URL.Path)
		}
		_, _ = io.WriteString(w, `{
			"data": [
				{"id": "claude-opus-5", "type": "model", "display_name": "Claude Opus 5",
				 "max_input_tokens": 400000, "max_tokens": 64000},
				{"id": "claude-haiku-4-5", "type": "model", "display_name": "Claude Haiku 4.5"}
			],
			"has_more": false
		}`)
	})

	models, err := p.Models(context.Background())
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	if len(models) != 2 {
		t.Fatalf("got %d models, want 2: %+v", len(models), models)
	}
	if models[0].ID != "claude-opus-5" {
		t.Errorf("models[0].ID = %q", models[0].ID)
	}
	if models[0].DisplayName != "Claude Opus 5" {
		t.Errorf("models[0].DisplayName = %q", models[0].DisplayName)
	}
	if models[0].ContextWindow != 400000 || models[0].MaxOutputTokens != 64000 {
		t.Errorf("models[0] limits = %d/%d, want 400000/64000",
			models[0].ContextWindow, models[0].MaxOutputTokens)
	}
	if models[0].Provider != "anthropic" {
		t.Errorf("models[0].Provider = %q", models[0].Provider)
	}
}

func TestModelsError(t *testing.T) {
	t.Parallel()

	p := newTestProvider(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, errorBody)
	})

	models, err := p.Models(context.Background())
	if err == nil {
		t.Fatalf("expected an error, got %d models", len(models))
	}

	var e *skyl.Error
	if !errors.As(err, &e) {
		t.Fatalf("error is not *skyl.Error: %T", err)
	}
	if e.StatusCode != http.StatusUnauthorized {
		t.Errorf("StatusCode = %d, want %d", e.StatusCode, http.StatusUnauthorized)
	}
}

// TestWithHeader checks the option reaches the wire — it exists for beta
// flags, which silently do nothing if the header is dropped.
func TestWithHeader(t *testing.T) {
	t.Parallel()

	var got http.Header
	srv := newRecordingServer(t, &got)

	p := anthropic.New(testKey,
		anthropic.WithBaseURL(srv),
		anthropic.WithHeader("anthropic-beta", "context-1m-2025-08-07"),
	)
	if _, err := p.Complete(context.Background(), basicRequest()); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	if v := got.Get("anthropic-beta"); v != "context-1m-2025-08-07" {
		t.Errorf("anthropic-beta = %q", v)
	}
}

func TestWithHTTPClient(t *testing.T) {
	t.Parallel()

	var got http.Header
	srv := newRecordingServer(t, &got)

	used := false
	hc := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		used = true
		return http.DefaultTransport.RoundTrip(r)
	})}

	p := anthropic.New(testKey, anthropic.WithBaseURL(srv), anthropic.WithHTTPClient(hc))
	if _, err := p.Complete(context.Background(), basicRequest()); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	if !used {
		t.Error("WithHTTPClient client was not used")
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// newRecordingServer starts a server that records the request headers and
// replies with a valid completion, returning its base URL.
func newRecordingServer(t *testing.T, into *http.Header) string {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*into = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, successBody)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// streamFrom serves frames as an SSE stream and returns every event skyl
// produced, so a test can assert on the whole sequence rather than a total.
func streamFrom(t *testing.T, frames []string) ([]skyl.StreamEvent, error) {
	t.Helper()

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
		return nil, err
	}
	defer stream.Close() //nolint:errcheck // test cleanup

	var events []skyl.StreamEvent
	for stream.Next() {
		events = append(events, stream.Event())
	}
	return events, stream.Err()
}

const streamStart = `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-5","content":[],"stop_reason":null,"usage":{"input_tokens":5,"output_tokens":0}}}`

// TestStreamThinkingDeltas checks reasoning output arrives as its own event
// type. Folding it into text deltas would silently splice the model's private
// reasoning into the user-visible answer.
func TestStreamThinkingDeltas(t *testing.T) {
	t.Parallel()

	events, err := streamFrom(t, []string{
		streamStart,
		`event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}`,
		`event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"let me think"}}`,
		`event: content_block_stop
data: {"type":"content_block_stop","index":0}`,
		`event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`,
		`event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"the answer"}}`,
		`event: content_block_stop
data: {"type":"content_block_stop","index":1}`,
		`event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":9}}`,
		`event: message_stop
data: {"type":"message_stop"}`,
	})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}

	var thinking, text string
	for _, ev := range events {
		switch ev.Type {
		case skyl.EventThinkingDelta:
			thinking += ev.Text
		case skyl.EventTextDelta:
			text += ev.Text
		}
	}

	if thinking != "let me think" {
		t.Errorf("thinking = %q, want %q", thinking, "let me think")
	}
	if text != "the answer" {
		t.Errorf("text = %q, want %q", text, "the answer")
	}
}

// TestStreamToolCall checks a tool call is withheld until its JSON arguments
// are whole. Emitting a half-parsed call would hand the caller arguments that
// cannot be unmarshalled.
func TestStreamToolCall(t *testing.T) {
	t.Parallel()

	events, err := streamFrom(t, []string{
		streamStart,
		`event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{}}}`,
		`event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"city\":"}}`,
		`event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"\"Kampala\"}"}}`,
		`event: content_block_stop
data: {"type":"content_block_stop","index":0}`,
		`event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":12}}`,
		`event: message_stop
data: {"type":"message_stop"}`,
	})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}

	var calls []skyl.ToolCall
	var last skyl.StreamEvent
	for _, ev := range events {
		last = ev
		if ev.Type == skyl.EventToolCall && ev.ToolCall != nil {
			calls = append(calls, *ev.ToolCall)
		}
	}

	if len(calls) != 1 {
		t.Fatalf("got %d tool calls, want 1: %+v", len(calls), calls)
	}
	if calls[0].ID != "toolu_1" || calls[0].Name != "get_weather" {
		t.Errorf("tool call = %+v", calls[0])
	}

	// The arguments must be complete, parseable JSON — not a fragment.
	var args struct {
		City string `json:"city"`
	}
	if err := json.Unmarshal(calls[0].Arguments, &args); err != nil {
		t.Fatalf("tool call arguments are not valid JSON (%s): %v", calls[0].Arguments, err)
	}
	if args.City != "Kampala" {
		t.Errorf("city = %q, want Kampala", args.City)
	}

	if last.Type != skyl.EventDone {
		t.Errorf("final event = %q, want %q", last.Type, skyl.EventDone)
	}
	if last.StopReason != skyl.StopToolUse {
		t.Errorf("StopReason = %q, want %q", last.StopReason, skyl.StopToolUse)
	}
}

// TestStreamCloseIsIdempotent checks the documented contract: callers close
// from a defer, which can run after the loop already drained the stream.
func TestStreamCloseIsIdempotent(t *testing.T) {
	t.Parallel()

	p := newTestProvider(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, streamStart+"\n\n")
		_, _ = io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	})

	stream, err := p.Stream(context.Background(), basicRequest())
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	for stream.Next() { //nolint:revive // draining
	}
	if err := stream.Close(); err != nil {
		t.Errorf("first Close: %v", err)
	}
	if err := stream.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}
