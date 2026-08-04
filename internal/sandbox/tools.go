package sandbox

import "strings"

// Tool calling, shared by all three protocols.
//
// The wire shapes differ completely — OpenAI nests a function object inside a
// tools array, Anthropic puts name and input_schema at the top level, Gemini
// wraps functionDeclarations inside a tools array of its own — but the
// *decision* is the same everywhere, so it is made once here and rendered three
// ways.
//
// This exists because tool calling was the least validated path in the library:
// the entire Tools and ToolChoice mapping blocks in internal/oai and
// provider/gemini were never executed by any test, and the sandbox could not
// help because none of its request structs even decoded a tools field.

// The tool call the sandbox makes. Fixed values, because a deterministic
// sandbox is the point — a test asserting on them is asserting the adapter
// carried them faithfully, not that the sandbox invented something.
const (
	toolCallID = "call_sandbox_1"

	// Deliberately more than one field and long enough to be split across
	// several streaming frames. A single short fragment would let a broken
	// argument accumulator pass.
	toolCallArgs = `{"city":"Kampala","units":"metric"}`
)

// toolTrigger is the word that makes the sandbox reach for a tool.
//
// A trigger rather than "always call when tools are declared", because both
// behaviours have to be reachable: a request that declares tools and does not
// need one must still come back as ordinary text.
const toolTrigger = "weather"

// Tool-choice modes, normalised across the three protocols. Each handler maps
// its own vocabulary onto these — OpenAI's "required", Anthropic's "any" and
// Gemini's "ANY" all mean the same thing.
const (
	choiceAuto     = "auto"
	choiceNone     = "none"
	choiceRequired = "required"
)

// toolCall is the sandbox's decision to invoke a tool.
type toolCall struct {
	ID   string
	Name string
	Args string // raw JSON
}

// decideToolCall reports which tool to call this turn, or nil for a text reply.
//
// choice is a normalised tool-choice mode; forcedName is set when the caller
// demanded one specific tool. Honouring choice is not decoration: an adapter
// that fails to send "none" gets a tool call the caller explicitly forbade, and
// one that fails to send "required" gets prose where a call was mandatory.
// Both are silent failures against a real provider.
func decideToolCall(prompt string, toolNames []string, choice, forcedName string) *toolCall {
	if len(toolNames) == 0 || choice == choiceNone {
		return nil
	}

	if forcedName != "" {
		for _, name := range toolNames {
			if name == forcedName {
				return &toolCall{ID: toolCallID, Name: name, Args: toolCallArgs}
			}
		}
		// A forced tool that was not declared is the caller's error, and a real
		// provider rejects it. The sandbox declines to call anything, which
		// surfaces as a text reply where the test expected a call.
		return nil
	}

	name := toolNames[0]
	for _, n := range toolNames {
		if strings.Contains(strings.ToLower(n), toolTrigger) {
			name = n
			break
		}
	}

	if choice == choiceRequired || strings.Contains(strings.ToLower(prompt), toolTrigger) {
		return &toolCall{ID: toolCallID, Name: name, Args: toolCallArgs}
	}
	return nil
}

// replyToToolResult produces the second-turn answer, once the caller has run
// the tool and sent its output back.
//
// It echoes the tool's own output rather than returning a canned sentence. That
// is what makes the round trip meaningful: the assertion proves the adapter
// actually transmitted the result content through its own message format, which
// no amount of asserting on a fixed string would show.
func replyToToolResult(result string) string {
	return "The tool said: " + result
}

// argChunks splits tool-call arguments into streaming fragments.
//
// Providers stream partial JSON, so an adapter has to buffer until the object
// is whole. Emitting the arguments in one frame would let an adapter that never
// accumulates anything pass — the very bug this is here to catch. The split
// points are deliberately mid-token, producing fragments that are not valid
// JSON on their own.
func argChunks(args string) []string {
	const parts = 3
	if len(args) < parts {
		return []string{args}
	}
	size := len(args) / parts
	out := make([]string, 0, parts)
	for i := 0; i < parts; i++ {
		start := i * size
		end := start + size
		if i == parts-1 {
			end = len(args)
		}
		out = append(out, args[start:end])
	}
	return out
}
