//go:build sandbox

// Tool calling over a real socket, for the Anthropic adapter.
//
//	cd provider/anthropic && go test -tags=sandbox ./...
//
// The sibling suite in provider/sandbox_tools_test.go covers the other three
// adapters; this one lives here because the adapter is its own module. Worth
// running despite the shared coverage: Anthropic's shape differs the most —
// tool input arrives as a decoded object rather than a JSON string, results
// come back as blocks inside a user turn rather than under a tool role, and
// the streamed call is a second content block at index 1.
package anthropic_test

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/BAGOMBEKA-JOB-DEV/skyl"
	"github.com/BAGOMBEKA-JOB-DEV/skyl/internal/sandbox"
	"github.com/BAGOMBEKA-JOB-DEV/skyl/internal/testutil"
	"github.com/BAGOMBEKA-JOB-DEV/skyl/provider/anthropic"
)

func sandboxProvider(t *testing.T) skyl.Provider {
	t.Helper()
	srv := httptest.NewServer(sandbox.New())
	t.Cleanup(srv.Close)
	return anthropic.New(sandbox.DefaultAPIKey,
		anthropic.WithBaseURL(srv.URL+"/anthropic"))
}

func weatherTool() skyl.Tool {
	return skyl.Tool{
		Name:        "get_weather",
		Description: "Look up the current weather for a city.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"city":  map[string]any{"type": "string"},
				"units": map[string]any{"type": "string"},
			},
			"required": []string{"city"},
		},
	}
}

// TestSandboxAnthropicToolLoop runs the full cycle. The closing assertion is
// the one that matters: the sandbox echoes the tool's own output, so an adapter
// that mislabels or drops the result produces an answer without it.
func TestSandboxAnthropicToolLoop(t *testing.T) {
	ctx := testutil.Context(t)
	client := skyl.New(sandboxProvider(t))

	first, err := client.Complete(ctx, &skyl.Request{
		Model:     "claude-haiku-4-5",
		MaxTokens: 128,
		Tools:     []skyl.Tool{weatherTool()},
		Messages:  []skyl.Message{skyl.UserText("What is the weather in Kampala?")},
	})
	if err != nil {
		t.Fatalf("first turn: %v", err)
	}
	if first.StopReason != skyl.StopToolUse {
		t.Errorf("StopReason = %q, want %q", first.StopReason, skyl.StopToolUse)
	}

	calls := first.ToolCalls()
	if len(calls) != 1 {
		t.Fatalf("got %d tool calls, want 1 (text %q)", len(calls), first.Text())
	}
	call := calls[0]
	if call.Name != "get_weather" || call.ID == "" {
		t.Errorf("call = %+v, want a named call with an ID", call)
	}

	var args map[string]any
	if err := json.Unmarshal(call.Arguments, &args); err != nil {
		t.Fatalf("arguments are not valid JSON: %v (%s)", err, call.Arguments)
	}
	if args["city"] != "Kampala" {
		t.Errorf("arguments = %v, want city=Kampala", args)
	}

	const toolOutput = "22C and sunny"
	second, err := client.Complete(ctx, &skyl.Request{
		Model:     "claude-haiku-4-5",
		MaxTokens: 128,
		Tools:     []skyl.Tool{weatherTool()},
		Messages: []skyl.Message{
			skyl.UserText("What is the weather in Kampala?"),
			{Role: skyl.RoleAssistant, Parts: []skyl.Part{call}},
			skyl.ToolResultMessage(call.ID, toolOutput),
		},
	})
	if err != nil {
		t.Fatalf("second turn: %v", err)
	}
	if !strings.Contains(second.Text(), toolOutput) {
		t.Errorf("second turn text = %q, want it to contain %q — the result did not "+
			"survive the round trip", second.Text(), toolOutput)
	}
}

// The streamed call is a second content block at index 1, with its arguments
// arriving as input_json_delta fragments that are not valid JSON alone.
func TestSandboxAnthropicToolCallStreaming(t *testing.T) {
	stream, err := skyl.New(sandboxProvider(t)).Stream(testutil.Context(t), &skyl.Request{
		Model:     "claude-haiku-4-5",
		MaxTokens: 128,
		Tools:     []skyl.Tool{weatherTool()},
		Messages:  []skyl.Message{skyl.UserText("What is the weather in Kampala?")},
	})
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	defer stream.Close() //nolint:errcheck // test cleanup

	var (
		calls []skyl.ToolCall
		last  skyl.StreamEvent
	)
	for stream.Next() {
		ev := stream.Event()
		last = ev
		if ev.Type == skyl.EventToolCall && ev.ToolCall != nil {
			calls = append(calls, *ev.ToolCall)
		}
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("Err() = %v", err)
	}
	if last.Type != skyl.EventDone {
		t.Errorf("final event = %q, want %q", last.Type, skyl.EventDone)
	}
	if len(calls) != 1 {
		t.Fatalf("got %d streamed tool calls, want 1", len(calls))
	}
	var args map[string]any
	if err := json.Unmarshal(calls[0].Arguments, &args); err != nil {
		t.Fatalf("streamed arguments are not valid JSON: %v (%s)", err, calls[0].Arguments)
	}
	if args["city"] != "Kampala" {
		t.Errorf("streamed arguments = %v, want city=Kampala", args)
	}
}

func TestSandboxAnthropicToolChoice(t *testing.T) {
	ctx := testutil.Context(t)
	client := skyl.New(sandboxProvider(t))

	t.Run("none forbids a call", func(t *testing.T) {
		resp, err := client.Complete(ctx, &skyl.Request{
			Model:      "claude-haiku-4-5",
			MaxTokens:  128,
			Tools:      []skyl.Tool{weatherTool()},
			ToolChoice: &skyl.ToolChoice{Mode: skyl.ToolChoiceNone},
			Messages:   []skyl.Message{skyl.UserText("What is the weather in Kampala?")},
		})
		if err != nil {
			t.Fatalf("Complete() error = %v", err)
		}
		if got := resp.ToolCalls(); len(got) != 0 {
			t.Errorf("got %d tool calls with ToolChoiceNone, want 0", len(got))
		}
	})

	t.Run("required forces a call", func(t *testing.T) {
		resp, err := client.Complete(ctx, &skyl.Request{
			Model:      "claude-haiku-4-5",
			MaxTokens:  128,
			Tools:      []skyl.Tool{weatherTool()},
			ToolChoice: &skyl.ToolChoice{Mode: skyl.ToolChoiceRequired},
			Messages:   []skyl.Message{skyl.UserText("Say hello.")},
		})
		if err != nil {
			t.Fatalf("Complete() error = %v", err)
		}
		if got := resp.ToolCalls(); len(got) != 1 {
			t.Errorf("got %d tool calls with ToolChoiceRequired, want 1", len(got))
		}
	})
}
