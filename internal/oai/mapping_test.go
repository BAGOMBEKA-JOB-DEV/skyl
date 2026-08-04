package oai

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/BAGOMBEKA-JOB-DEV/skyl"
)

// Request-mapping coverage for the blocks that no test executed before.
//
// The Tools and ToolChoice blocks in particular were entirely dead: skyl's
// headline feature is that one Request works everywhere, and the code that
// renders a tool onto this wire format had never run. provider/anthropic's
// options_test.go is the model this file follows.

// captureRequest sends req and returns the decoded outbound payload.
func captureRequest(t *testing.T, req *skyl.Request) map[string]any {
	t.Helper()

	var captured map[string]any
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &captured); err != nil {
			t.Errorf("outbound body is not JSON: %v", err)
		}
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`)
	})

	if _, err := c.Complete(context.Background(), req); err != nil {
		t.Fatalf("Complete() error = %v", err)
	}
	return captured
}

func TestToolsAreMapped(t *testing.T) {
	t.Parallel()

	req := basicRequest()
	req.Tools = []skyl.Tool{{
		Name:        "get_weather",
		Description: "Look up the weather.",
		Parameters: map[string]any{
			"type":       "object",
			"properties": map[string]any{"city": map[string]any{"type": "string"}},
			"required":   []string{"city"},
		},
	}}

	captured := captureRequest(t, req)

	tools, ok := captured["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("tools = %v, want one declaration", captured["tools"])
	}
	tool := tools[0].(map[string]any)
	if tool["type"] != "function" {
		t.Errorf("tool type = %v, want function", tool["type"])
	}
	fn := tool["function"].(map[string]any)
	if fn["name"] != "get_weather" {
		t.Errorf("name = %v, want get_weather", fn["name"])
	}
	if fn["description"] != "Look up the weather." {
		t.Errorf("description = %v, want it carried", fn["description"])
	}
	// The schema must arrive whole: a dropped $ref or required list silently
	// relaxes validation on the provider's side.
	params := fn["parameters"].(map[string]any)
	if params["type"] != "object" {
		t.Errorf("parameters.type = %v, want object", params["type"])
	}
	if _, ok := params["properties"]; !ok {
		t.Error("parameters.properties was dropped")
	}
	if _, ok := params["required"]; !ok {
		t.Error("parameters.required was dropped")
	}
}

// A tool taking no arguments still needs a schema: the API rejects a
// declaration without one, so skyl substitutes an empty object.
func TestToolWithoutParametersGetsEmptySchema(t *testing.T) {
	t.Parallel()

	req := basicRequest()
	req.Tools = []skyl.Tool{{Name: "ping"}}

	captured := captureRequest(t, req)
	tools := captured["tools"].([]any)
	fn := tools[0].(map[string]any)["function"].(map[string]any)

	params, ok := fn["parameters"].(map[string]any)
	if !ok {
		t.Fatalf("parameters = %v, want an empty object schema", fn["parameters"])
	}
	if params["type"] != "object" {
		t.Errorf("parameters.type = %v, want object", params["type"])
	}
}

func TestToolChoiceModes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		choice skyl.ToolChoice
		want   any
	}{
		{"auto", skyl.ToolChoice{Mode: skyl.ToolChoiceAuto}, "auto"},
		{"none", skyl.ToolChoice{Mode: skyl.ToolChoiceNone}, "none"},
		{"required", skyl.ToolChoice{Mode: skyl.ToolChoiceRequired}, "required"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			req := basicRequest()
			req.Tools = []skyl.Tool{{Name: "ping"}}
			req.ToolChoice = &tc.choice

			if got := captureRequest(t, req)["tool_choice"]; got != tc.want {
				t.Errorf("tool_choice = %v, want %v", got, tc.want)
			}
		})
	}

	t.Run("specific names the function", func(t *testing.T) {
		t.Parallel()

		req := basicRequest()
		req.Tools = []skyl.Tool{{Name: "ping"}}
		req.ToolChoice = &skyl.ToolChoice{Mode: skyl.ToolChoiceSpecific, Name: "ping"}

		choice, ok := captureRequest(t, req)["tool_choice"].(map[string]any)
		if !ok {
			t.Fatalf("tool_choice = %v, want an object naming the tool", choice)
		}
		if choice["type"] != "function" {
			t.Errorf("type = %v, want function", choice["type"])
		}
		fn := choice["function"].(map[string]any)
		if fn["name"] != "ping" {
			t.Errorf("name = %v, want ping", fn["name"])
		}
	})
}

func TestTopPIsMapped(t *testing.T) {
	t.Parallel()

	topP := 0.9
	req := basicRequest()
	req.TopP = &topP

	if got := captureRequest(t, req)["top_p"]; got != 0.9 {
		t.Errorf("top_p = %v, want 0.9", got)
	}
}

func TestThinkingMapsToReasoningEffort(t *testing.T) {
	t.Parallel()

	req := basicRequest()
	req.Thinking = &skyl.Thinking{Enabled: true, Effort: skyl.EffortHigh}

	if got := captureRequest(t, req)["reasoning_effort"]; got != "high" {
		t.Errorf("reasoning_effort = %v, want high", got)
	}

	// Without an effort there is nothing to say, so the field is omitted
	// rather than guessed at.
	plain := basicRequest()
	plain.Thinking = &skyl.Thinking{Enabled: true}
	if _, ok := captureRequest(t, plain)["reasoning_effort"]; ok {
		t.Error("reasoning_effort was sent for a Thinking with no effort")
	}
}

// Images were mapped but never exercised. Both shapes matter: a URL rides
// through untouched, while raw bytes become a data: URI.
func TestImageMapping(t *testing.T) {
	t.Parallel()

	t.Run("url is passed through", func(t *testing.T) {
		t.Parallel()

		req := basicRequest()
		req.Messages = []skyl.Message{{
			Role:  skyl.RoleUser,
			Parts: []skyl.Part{skyl.Image{URL: "https://example.com/cat.png"}},
		}}

		blocks := userContentBlocks(t, captureRequest(t, req))
		img := findBlock(t, blocks, "image_url")
		url := img["image_url"].(map[string]any)["url"]
		if url != "https://example.com/cat.png" {
			t.Errorf("url = %v, want it passed through untouched", url)
		}
	})

	t.Run("raw data becomes a data uri", func(t *testing.T) {
		t.Parallel()

		req := basicRequest()
		req.Messages = []skyl.Message{{
			Role: skyl.RoleUser,
			Parts: []skyl.Part{
				skyl.Image{MediaType: "image/png", Data: []byte{0x89, 0x50}},
				skyl.Text{Text: "what is this?"},
			},
		}}

		blocks := userContentBlocks(t, captureRequest(t, req))
		img := findBlock(t, blocks, "image_url")
		url, _ := img["image_url"].(map[string]any)["url"].(string)
		// base64 of {0x89,0x50} is "iVA=" — two bytes pad to four characters.
		if url != "data:image/png;base64,iVA=" {
			t.Errorf("url = %q, want a base64 data URI carrying the media type", url)
		}
		// The caption must survive alongside the image.
		if findBlock(t, blocks, "text")["text"] != "what is this?" {
			t.Error("the text part was dropped when an image was present")
		}
	})
}

// A tool call replayed in an assistant turn is how a caller continues a
// conversation. The mapping existed and was never executed.
func TestAssistantToolCallIsMapped(t *testing.T) {
	t.Parallel()

	req := basicRequest()
	req.Messages = []skyl.Message{
		skyl.UserText("weather?"),
		{Role: skyl.RoleAssistant, Parts: []skyl.Part{skyl.ToolCall{
			ID: "call_1", Name: "get_weather", Arguments: json.RawMessage(`{"city":"Kampala"}`),
		}}},
		skyl.ToolResultMessage("call_1", "22C"),
	}

	msgs := captureRequest(t, req)["messages"].([]any)
	assistant := msgs[1].(map[string]any)
	if assistant["role"] != "assistant" {
		t.Fatalf("role = %v, want assistant", assistant["role"])
	}

	calls, ok := assistant["tool_calls"].([]any)
	if !ok || len(calls) != 1 {
		t.Fatalf("tool_calls = %v, want one", assistant["tool_calls"])
	}
	call := calls[0].(map[string]any)
	if call["id"] != "call_1" || call["type"] != "function" {
		t.Errorf("call = %v, want id call_1 and type function", call)
	}
	fn := call["function"].(map[string]any)
	if fn["name"] != "get_weather" {
		t.Errorf("name = %v, want get_weather", fn["name"])
	}
	// Arguments ride as a JSON *string* in this format, not as an object.
	if args, ok := fn["arguments"].(string); !ok || args != `{"city":"Kampala"}` {
		t.Errorf("arguments = %v (%T), want the raw JSON string", fn["arguments"], fn["arguments"])
	}
}

// A tool call with no arguments must still send valid JSON: an empty string
// would be rejected as malformed.
func TestAssistantToolCallWithoutArguments(t *testing.T) {
	t.Parallel()

	req := basicRequest()
	req.Messages = []skyl.Message{
		skyl.UserText("ping"),
		{Role: skyl.RoleAssistant, Parts: []skyl.Part{
			skyl.ToolCall{ID: "call_1", Name: "ping"},
		}},
	}

	msgs := captureRequest(t, req)["messages"].([]any)
	fn := msgs[1].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)
	if fn["arguments"] != "{}" {
		t.Errorf("arguments = %v, want an empty JSON object", fn["arguments"])
	}
}

// A failed tool must be reported as failed, or the model assumes success and
// carries on with an answer built on nothing.
func TestToolErrorIsMarked(t *testing.T) {
	t.Parallel()

	req := basicRequest()
	req.Messages = []skyl.Message{
		skyl.UserText("weather?"),
		skyl.ToolErrorMessage("call_1", "upstream timed out"),
	}

	msgs := captureRequest(t, req)["messages"].([]any)
	last := msgs[len(msgs)-1].(map[string]any)
	content, _ := last["content"].(string)
	if content == "upstream timed out" {
		t.Error("the failure was reported as an ordinary result")
	}
	if content != "error: upstream timed out" {
		t.Errorf("content = %q, want it marked as an error", content)
	}
}

// userContentBlocks pulls the block list off the first message.
func userContentBlocks(t *testing.T, captured map[string]any) []any {
	t.Helper()
	msgs, ok := captured["messages"].([]any)
	if !ok || len(msgs) == 0 {
		t.Fatalf("messages = %v", captured["messages"])
	}
	blocks, ok := msgs[0].(map[string]any)["content"].([]any)
	if !ok {
		t.Fatalf("content = %v, want a block list", msgs[0])
	}
	return blocks
}

func findBlock(t *testing.T, blocks []any, kind string) map[string]any {
	t.Helper()
	for _, raw := range blocks {
		b, ok := raw.(map[string]any)
		if ok && b["type"] == kind {
			return b
		}
	}
	t.Fatalf("no %q block in %v", kind, blocks)
	return nil
}
