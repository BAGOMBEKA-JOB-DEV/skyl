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

// contractStreamSSE is an Anthropic stream whose text deltas concatenate to
// "ab", which is what the shared contract asserts.
//
// It is spelled out here rather than assembled from StreamFrames because
// Anthropic's stream is a sequence of *named* events: the `event:` line is
// load-bearing, and a bare `data:` frame is not a valid Anthropic stream.
const contractStreamSSE = `event: message_start
data: {"type":"message_start","message":{"id":"msg_c","type":"message","role":"assistant","model":"served-model-0613","content":[],"stop_reason":null,"usage":{"input_tokens":1,"output_tokens":0}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"a"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"b"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}

event: message_stop
data: {"type":"message_stop"}

`

// TestContract runs the shared adapter contract, streaming included.
//
// Streaming used to be skipped here because Anthropic's SSE is named events
// rather than the bare `data:` frames the suite emits. That exempted this
// adapter — the one with by far the largest dependency surface — from three
// checks, including the only goroutine-leak assertion it had. Supplying the
// vendor's own framing via StreamRawSSE costs a fixture and buys all three.
func TestContract(t *testing.T) {
	t.Parallel()

	providertest.Suite{
		Name:   "anthropic",
		APIKey: testKey,
		New: func(baseURL string) skyl.Provider {
			return anthropic.New(testKey, anthropic.WithBaseURL(baseURL))
		},
		SuccessBody:  successBody,
		WantText:     "hi there",
		WantModel:    "served-model-0613",
		ErrorBody:    errorBody,
		StreamRawSSE: contractStreamSSE,
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
	// Stop sequences were previously set by this test and never asserted, so
	// the mapping counted as covered without being checked.
	stop, ok := captured["stop_sequences"].([]any)
	if !ok || len(stop) != 1 || stop[0] != "END" {
		t.Errorf("stop_sequences = %v, want [END]", captured["stop_sequences"])
	}
	if resp.StopReason != skyl.StopEndTurn {
		t.Errorf("StopReason = %q, want %q", resp.StopReason, skyl.StopEndTurn)
	}
	if resp.Usage.InputTokens != 9 || resp.Usage.OutputTokens != 3 {
		t.Errorf("Usage = %+v, want 9/3", resp.Usage)
	}
}

// Anthropic reports input_tokens EXCLUDING its two cache counters, while every
// other adapter reports an input figure that already contains them. skyl.Usage
// defines InputTokens as the total including cache, so this adapter must add
// them — otherwise an identical cached conversation reports a different
// billable input depending on which provider served it.
func TestCompleteNormalisesCacheTokensIntoInput(t *testing.T) {
	t.Parallel()

	const cachedBody = `{
		"id": "msg_1",
		"type": "message",
		"role": "assistant",
		"model": "claude-opus-5",
		"content": [{"type": "text", "text": "hi"}],
		"stop_reason": "end_turn",
		"usage": {
			"input_tokens": 100,
			"output_tokens": 5,
			"cache_read_input_tokens": 800,
			"cache_creation_input_tokens": 50
		}
	}`

	p := newTestProvider(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, cachedBody)
	})

	resp, err := p.Complete(context.Background(), basicRequest())
	if err != nil {
		t.Fatalf("Complete() error = %v", err)
	}

	if got := resp.Usage.InputTokens; got != 950 {
		t.Errorf("InputTokens = %d, want 950 (100 uncached + 800 read + 50 written)", got)
	}
	if got := resp.Usage.CacheReadTokens; got != 800 {
		t.Errorf("CacheReadTokens = %d, want 800", got)
	}
	if got := resp.Usage.CacheWriteTokens; got != 50 {
		t.Errorf("CacheWriteTokens = %d, want 50", got)
	}
	if got := resp.Usage.TotalTokens(); got != 955 {
		t.Errorf("TotalTokens() = %d, want 955 (950 in + 5 out, cache not double-counted)", got)
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

// Named for what it actually exercises: a tool call whose arguments are not
// valid JSON. The adapter's other rejection path — an unknown Part type — is
// unreachable from outside skyl, because Part is a closed interface
// (message.go), so only a new Part type added to skyl itself could reach it.
func TestMalformedToolArgumentsAreRejectedNotDropped(t *testing.T) {
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

// The SDK's accumulator only records a stop reason once message_delta arrives,
// so a stream cut short before it reached EOF looking exactly like a complete
// one — the caller kept a truncated answer with a nil error.
func TestStreamTruncationIsReported(t *testing.T) {
	t.Parallel()

	frames := []string{
		`event: message_start
data: {"type":"message_start","message":{"id":"msg_4","type":"message","role":"assistant","model":"claude-opus-5","content":[],"stop_reason":null,"usage":{"input_tokens":5,"output_tokens":0}}}`,
		`event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Par"}}`,
		// The connection ends here: no message_delta, no message_stop.
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

	var text strings.Builder
	for stream.Next() {
		if ev := stream.Event(); ev.Type == skyl.EventTextDelta {
			text.WriteString(ev.Text)
		}
	}

	err = stream.Err()
	if err == nil {
		t.Fatal("Err() = nil, want a truncation error")
	}
	if !errors.Is(err, skyl.ErrServer) {
		t.Errorf("Err() = %v, want it to classify as ErrServer", err)
	}
	if !strings.Contains(err.Error(), "truncated") {
		t.Errorf("Err() = %q, want it to say the response is truncated", err)
	}
	if got := text.String(); got != "Par" {
		t.Errorf("text = %q, want the partial text %q", got, "Par")
	}
}

// docs/rules.md §6.5 requires every adapter to honour ProviderOptions. This
// one did not: buildParams builds a typed SDK struct and simply never read the
// map, so the documented escape hatch — the only route to cache_control, top_k
// and beta features — silently did nothing on Anthropic.
func TestProviderOptionsAreHonoured(t *testing.T) {
	t.Parallel()

	var captured map[string]any
	p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &captured)
		_, _ = io.WriteString(w, successBody)
	})

	req := basicRequest()
	req.ProviderOptions = map[string]any{
		"top_k": 40,
		// The escape hatch must be able to override what skyl chose, not just
		// add to it.
		"max_tokens": 4321,
		"metadata":   map[string]any{"user_id": "u-7"},
	}

	if _, err := p.Complete(context.Background(), req); err != nil {
		t.Fatalf("Complete() error = %v", err)
	}

	if got := captured["top_k"]; got != float64(40) {
		t.Errorf("top_k = %v, want ProviderOptions merged into the payload", got)
	}
	if got := captured["max_tokens"]; got != float64(4321) {
		t.Errorf("max_tokens = %v, want ProviderOptions to override skyl's value", got)
	}
	meta, ok := captured["metadata"].(map[string]any)
	if !ok || meta["user_id"] != "u-7" {
		t.Errorf("metadata = %v, want the nested object preserved", captured["metadata"])
	}
	// Fields skyl set and the caller did not touch must survive.
	if got := captured["model"]; got != "claude-opus-5" {
		t.Errorf("model = %v, want skyl's value left intact", got)
	}
}

func TestProviderOptionsApplyToStreaming(t *testing.T) {
	t.Parallel()

	var captured map[string]any
	p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &captured)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	})

	req := basicRequest()
	req.ProviderOptions = map[string]any{"top_k": 7}

	stream, err := p.Stream(context.Background(), req)
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	for stream.Next() { //nolint:revive // draining is the point
	}
	_ = stream.Close()

	if got := captured["top_k"]; got != float64(7) {
		t.Errorf("top_k = %v, want ProviderOptions honoured on the streaming path too", got)
	}
}

// Reading only "properties" and "required" discarded the rest of the schema, so
// a tool defined with $defs/$ref reached Anthropic with dangling references
// while reaching OpenAI intact — the same skyl.Tool meaning two different
// things depending on the provider.
func TestToolSchemaIsPassedThroughWhole(t *testing.T) {
	t.Parallel()

	var captured map[string]any
	p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &captured)
		_, _ = io.WriteString(w, successBody)
	})

	req := basicRequest()
	req.Tools = []skyl.Tool{{
		Name:        "lookup",
		Description: "Look something up",
		Parameters: map[string]any{
			"type":                 "object",
			"properties":           map[string]any{"where": map[string]any{"$ref": "#/$defs/Place"}},
			"required":             []string{"where"},
			"additionalProperties": false,
			"$defs": map[string]any{
				"Place": map[string]any{"type": "string", "description": "a place"},
			},
		},
	}}

	if _, err := p.Complete(context.Background(), req); err != nil {
		t.Fatalf("Complete() error = %v", err)
	}

	tools := captured["tools"].([]any)
	schema := tools[0].(map[string]any)["input_schema"].(map[string]any)

	if _, ok := schema["$defs"]; !ok {
		t.Errorf("input_schema = %v, want $defs preserved — a $ref without it is dangling", schema)
	}
	if got, ok := schema["additionalProperties"]; !ok || got != false {
		t.Errorf("additionalProperties = %v, want false preserved", got)
	}
	if _, ok := schema["properties"]; !ok {
		t.Error("properties was lost")
	}
	req0 := schema["required"].([]any)
	if len(req0) != 1 || req0[0] != "where" {
		t.Errorf("required = %v, want [where]", req0)
	}
	// The struct always emits its own object type; a duplicate would be
	// invalid JSON Schema.
	if got := schema["type"]; got != "object" {
		t.Errorf("type = %v, want exactly one \"object\"", got)
	}
}
