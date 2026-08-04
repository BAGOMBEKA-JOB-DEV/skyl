//go:build sandbox

// Tool calling, end to end over a real socket, for every adapter.
//
//	go test -tags=sandbox ./provider/
//
// This suite exists because tool calling was the least validated path in the
// library. The entire Tools and ToolChoice mapping blocks in internal/oai and
// provider/gemini were never executed by any test, request-side ToolCall
// mapping was dead in all three adapters, and the sandbox could not help
// because none of its request structs decoded a tools field at all.
//
// The same caveat as every sandbox suite applies: this proves the round trip is
// self-consistent and survives real HTTP, not that the field names match what a
// provider actually sends. Only -tags=integration settles that.
package provider_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/BAGOMBEKA-JOB-DEV/skyl"
	"github.com/BAGOMBEKA-JOB-DEV/skyl/internal/sandbox"
	"github.com/BAGOMBEKA-JOB-DEV/skyl/internal/testutil"
	"github.com/BAGOMBEKA-JOB-DEV/skyl/provider/gemini"
	"github.com/BAGOMBEKA-JOB-DEV/skyl/provider/openai"
	"github.com/BAGOMBEKA-JOB-DEV/skyl/provider/openaicompat"
)

// weatherTool is the declaration every case in this file sends. The sandbox
// calls it when the prompt mentions the weather.
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

// toolTargets returns one adapter per protocol, each pointed at the sandbox.
func toolTargets(t *testing.T, base string) map[string]skyl.Provider {
	t.Helper()
	return map[string]skyl.Provider{
		"openai": openai.New(sandbox.DefaultAPIKey,
			openai.WithBaseURL(base+"/openai/v1")),
		"gemini": gemini.New(sandbox.DefaultAPIKey,
			gemini.WithBaseURL(base+"/gemini/v1beta")),
		"openaicompat": openaicompat.New(
			openaicompat.WithBaseURL(base+"/compat/v1"),
			openaicompat.WithAPIKey(sandbox.DefaultAPIKey),
			openaicompat.WithName("compat")),
	}
}

func toolModel(provider string) string {
	if provider == "gemini" {
		return "gemini-3.6-flash"
	}
	return "gpt-5.6"
}

// TestSandboxToolLoop runs the whole cycle: ask a question that needs a tool,
// receive the call, run the tool, send the result back, and get an answer that
// quotes it.
//
// The final assertion is the load-bearing one. The sandbox echoes the tool's
// own output, so an adapter that transmits the result under the wrong field —
// or drops it — produces an answer that does not contain it. A canned reply
// would have proved nothing.
func TestSandboxToolLoop(t *testing.T) {
	base := newSandbox(t)

	for name, p := range toolTargets(t, base) {
		t.Run(name, func(t *testing.T) {
			ctx := testutil.Context(t)
			client := skyl.New(p)
			model := toolModel(name)

			// Turn one: the model asks for the tool.
			first, err := client.Complete(ctx, &skyl.Request{
				Model:     model,
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
				t.Fatalf("got %d tool calls, want 1 (stop reason %q, text %q)",
					len(calls), first.StopReason, first.Text())
			}
			call := calls[0]
			if call.Name != "get_weather" {
				t.Errorf("call name = %q, want get_weather", call.Name)
			}
			if call.ID == "" {
				t.Error("call ID is empty; it is what correlates the result")
			}

			// Arguments must be whole, valid JSON even though the sandbox sent
			// them as fragments — that is the accumulator's whole job.
			var args map[string]any
			if err := json.Unmarshal(call.Arguments, &args); err != nil {
				t.Fatalf("arguments are not valid JSON: %v (%s)", err, call.Arguments)
			}
			if args["city"] != "Kampala" || args["units"] != "metric" {
				t.Errorf("arguments = %v, want city=Kampala units=metric", args)
			}

			// Turn two: run the tool and send the result back.
			const toolOutput = "22C and sunny"
			second, err := client.Complete(ctx, &skyl.Request{
				Model:     model,
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
				t.Errorf("second turn text = %q, want it to contain the tool output %q — "+
					"the result did not survive the round trip", second.Text(), toolOutput)
			}
		})
	}
}

// TestSandboxToolCallStreaming proves the arguments are reassembled from
// fragments. The sandbox deliberately splits them mid-token, so each frame on
// its own is invalid JSON.
func TestSandboxToolCallStreaming(t *testing.T) {
	base := newSandbox(t)

	for name, p := range toolTargets(t, base) {
		t.Run(name, func(t *testing.T) {
			ctx := testutil.Context(t)

			stream, err := skyl.New(p).Stream(ctx, &skyl.Request{
				Model:     toolModel(name),
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
				t.Fatalf("streamed arguments are not valid JSON: %v (%s)",
					err, calls[0].Arguments)
			}
			if args["city"] != "Kampala" {
				t.Errorf("streamed arguments = %v, want city=Kampala", args)
			}
		})
	}
}

// TestSandboxToolChoiceIsHonoured checks the two modes with observable
// consequences. An adapter that drops tool_choice looks correct until a caller
// forbids tool use and gets a tool call anyway.
func TestSandboxToolChoiceIsHonoured(t *testing.T) {
	base := newSandbox(t)

	for name, p := range toolTargets(t, base) {
		t.Run(name, func(t *testing.T) {
			ctx := testutil.Context(t)
			client := skyl.New(p)
			model := toolModel(name)

			t.Run("none forbids a call", func(t *testing.T) {
				resp, err := client.Complete(ctx, &skyl.Request{
					Model:      model,
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
					Model:      model,
					MaxTokens:  128,
					Tools:      []skyl.Tool{weatherTool()},
					ToolChoice: &skyl.ToolChoice{Mode: skyl.ToolChoiceRequired},
					// A prompt with no trigger word: only the forced mode can
					// produce a call here.
					Messages: []skyl.Message{skyl.UserText("Say hello.")},
				})
				if err != nil {
					t.Fatalf("Complete() error = %v", err)
				}
				if got := resp.ToolCalls(); len(got) != 1 {
					t.Errorf("got %d tool calls with ToolChoiceRequired, want 1", len(got))
				}
			})
		})
	}
}

// TestSandboxToolsAreNotCalledUnprompted guards the other direction: declaring
// a tool must not force one. Without this, an adapter that always requested a
// tool call would pass every test above.
func TestSandboxToolsAreNotCalledUnprompted(t *testing.T) {
	base := newSandbox(t)

	for name, p := range toolTargets(t, base) {
		t.Run(name, func(t *testing.T) {
			resp, err := skyl.New(p).Complete(testutil.Context(t), &skyl.Request{
				Model:     toolModel(name),
				MaxTokens: 128,
				Tools:     []skyl.Tool{weatherTool()},
				Messages:  []skyl.Message{skyl.UserText("What is the capital of France?")},
			})
			if err != nil {
				t.Fatalf("Complete() error = %v", err)
			}
			if got := resp.ToolCalls(); len(got) != 0 {
				t.Errorf("got %d tool calls for a question needing none", len(got))
			}
			if !strings.Contains(resp.Text(), "Paris") {
				t.Errorf("Text() = %q, want the ordinary answer", resp.Text())
			}
		})
	}
}
