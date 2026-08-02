package skyl

import "encoding/json"

// Role identifies who produced a [Message].
//
// skyl deliberately has no system role: system prompts live on
// [Request.System] because providers place them differently — a top-level
// parameter for Anthropic, a message for OpenAI, systemInstruction for Gemini.
type Role string

// The roles skyl understands.
const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// Valid reports whether r is a role skyl understands.
func (r Role) Valid() bool {
	switch r {
	case RoleUser, RoleAssistant, RoleTool:
		return true
	default:
		return false
	}
}

// Part is one element of a message's content.
//
// The interface is deliberately closed — it has an unexported method, so only
// this package can implement it. An open interface would let callers construct
// parts that no adapter knows how to render, turning a compile-time error into
// a runtime one.
//
// The implementations are [Text], [Image], [ToolCall], and [ToolResult].
type Part interface {
	isPart()
}

// Text is a run of plain text.
type Text struct {
	Text string
}

func (Text) isPart() {}

// Image is an image supplied to the model.
//
// Provide exactly one of Data or URL. Not every provider accepts URLs, and not
// every model accepts images at all; an adapter that cannot represent an image
// returns [ErrUnsupported] rather than dropping it silently.
type Image struct {
	// MediaType is the IANA media type, for example "image/png". Required
	// when Data is set.
	MediaType string

	// Data is the raw (not base64-encoded) image content.
	Data []byte

	// URL references a remotely hosted image.
	URL string
}

func (Image) isPart() {}

// ToolCall is the model's request to invoke a tool.
type ToolCall struct {
	// ID correlates this call with its [ToolResult]. Providers generate it.
	ID string

	// Name is the tool the model wants to run.
	Name string

	// Arguments is the JSON object the model produced for the tool's
	// parameters. It is raw JSON because skyl cannot know the tool's schema.
	Arguments json.RawMessage
}

func (ToolCall) isPart() {}

// ToolResult carries the outcome of a tool invocation back to the model.
type ToolResult struct {
	// CallID must match the [ToolCall.ID] this result answers.
	CallID string

	// Content is the tool's output, rendered as text.
	Content string

	// IsError reports that the tool failed. Providers surface this to the
	// model so it can adapt rather than assume success.
	IsError bool
}

func (ToolResult) isPart() {}

// Message is one turn in a conversation: a role and its ordered content.
type Message struct {
	Role  Role
	Parts []Part
}

// Text returns every [Text] part concatenated, and ignores other parts.
//
// It is a convenience for the common case where a caller wants the model's
// prose. Use [Message.Parts] directly when tool calls or images matter.
func (m Message) Text() string {
	// Fast paths avoid allocating for the overwhelmingly common shapes.
	switch len(m.Parts) {
	case 0:
		return ""
	case 1:
		if t, ok := m.Parts[0].(Text); ok {
			return t.Text
		}
		return ""
	}

	n := 0
	for _, p := range m.Parts {
		if t, ok := p.(Text); ok {
			n += len(t.Text)
		}
	}
	if n == 0 {
		return ""
	}

	buf := make([]byte, 0, n)
	for _, p := range m.Parts {
		if t, ok := p.(Text); ok {
			buf = append(buf, t.Text...)
		}
	}
	return string(buf)
}

// ToolCalls returns every [ToolCall] part, in order.
func (m Message) ToolCalls() []ToolCall {
	var calls []ToolCall
	for _, p := range m.Parts {
		if c, ok := p.(ToolCall); ok {
			calls = append(calls, c)
		}
	}
	return calls
}

// UserText returns a user message containing a single run of text.
func UserText(text string) Message {
	return Message{Role: RoleUser, Parts: []Part{Text{Text: text}}}
}

// AssistantText returns an assistant message containing a single run of text.
//
// Use it to replay prior turns; skyl does not support prefilling an assistant
// turn to steer the next response, because several current models reject it.
func AssistantText(text string) Message {
	return Message{Role: RoleAssistant, Parts: []Part{Text{Text: text}}}
}

// UserImage returns a user message carrying an image and an optional caption.
func UserImage(mediaType string, data []byte, caption string) Message {
	parts := []Part{Image{MediaType: mediaType, Data: data}}
	if caption != "" {
		parts = append(parts, Text{Text: caption})
	}
	return Message{Role: RoleUser, Parts: parts}
}

// ToolResultMessage returns a tool message answering the call identified by
// callID.
//
// Append it after the assistant message that requested the call. When several
// tools were called in one turn, append one message per call before the next
// [Client.Complete].
func ToolResultMessage(callID, content string) Message {
	return Message{
		Role:  RoleTool,
		Parts: []Part{ToolResult{CallID: callID, Content: content}},
	}
}

// ToolErrorMessage is [ToolResultMessage] for a tool that failed.
func ToolErrorMessage(callID, content string) Message {
	return Message{
		Role:  RoleTool,
		Parts: []Part{ToolResult{CallID: callID, Content: content, IsError: true}},
	}
}
