// Package gateway exposes skyl over HTTP using go-chi.
//
// It is a separate Go module so that importing the skyl library never pulls in
// a router. The core library is an HTTP *client*; chi routes inbound requests.
// See docs/adr/0003-gateway-as-separate-module.md.
//
// Use the gateway when non-Go services need model access, when API keys should
// live in exactly one place, or when the estate needs a single audited egress
// point for model traffic. A Go service calling a model should import the
// library directly — an extra network hop buys nothing.
package gateway

import (
	"encoding/base64"
	"encoding/json"
	"fmt"

	"github.com/BAGOMBEKA-JOB-DEV/skyl"
)

// ChatRequest is the JSON body of POST /v1/chat and /v1/chat/stream.
type ChatRequest struct {
	// Provider names a registered provider. Empty uses the configured
	// default.
	Provider string `json:"provider,omitempty"`

	// Model is passed to the provider untouched.
	Model string `json:"model"`

	// System is the system prompt.
	System string `json:"system,omitempty"`

	// Messages is the conversation. Required.
	Messages []ChatMessage `json:"messages"`

	MaxTokens   int      `json:"max_tokens,omitempty"`
	Temperature *float64 `json:"temperature,omitempty"`
	TopP        *float64 `json:"top_p,omitempty"`
	Stop        []string `json:"stop,omitempty"`

	Tools []ChatTool `json:"tools,omitempty"`

	// ToolChoice constrains tool use: "auto", "none", "required", or an object
	// naming one tool.
	ToolChoice *ChatToolChoice `json:"tool_choice,omitempty"`

	// Thinking requests reasoning. A pointer because nil ("provider default")
	// and an explicit off are different instructions.
	Thinking *ChatThinking `json:"thinking,omitempty"`

	// ProviderOptions is forwarded verbatim as skyl's escape hatch.
	ProviderOptions map[string]any `json:"provider_options,omitempty"`
}

// ChatMessage is one conversation turn over the wire.
//
// Content is a list of parts rather than a string because a turn is not always
// text. An assistant turn that called a tool has to be sent back verbatim on
// the next request — providers reject a tool result that does not follow the
// call it answers — and a flat string cannot carry that.
//
// The convenience of a plain string is kept: a message with only text may set
// Text instead of Content, and exactly one of the two must be present.
type ChatMessage struct {
	// Role is "user", "assistant", or "tool".
	Role string `json:"role"`

	// Text is shorthand for a single text part. Mutually exclusive with
	// Content.
	Text string `json:"text,omitempty"`

	// Content is the ordered parts of the turn. Order matters: a caption after
	// an image reads differently from one before it.
	Content []ChatPart `json:"content,omitempty"`
}

// Part kinds. The set is closed because skyl.Part is closed — these are its
// four implementations and no more can exist outside the skyl package.
const (
	PartText       = "text"
	PartImage      = "image"
	PartToolCall   = "tool_call"
	PartToolResult = "tool_result"
)

// ChatPart is one element of a message, discriminated by Type.
type ChatPart struct {
	Type string `json:"type"`

	// Text is set when Type is "text".
	Text string `json:"text,omitempty"`

	// Image fields, set when Type is "image". Supply exactly one of Data or
	// URL; Data is standard base64 and requires MediaType.
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`

	// Tool-call fields, set when Type is "tool_call".
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Arguments json.RawMessage `json:"arguments,omitempty"`

	// Tool-result fields, set when Type is "tool_result". Content carries the
	// tool's output; IsError reports that the tool failed, which the model
	// needs so it can adapt rather than assume success.
	ToolCallID string `json:"tool_call_id,omitempty"`
	Content    string `json:"content,omitempty"`
	IsError    bool   `json:"is_error,omitempty"`
}

// ChatTool describes a tool the model may call.
type ChatTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

// ChatToolChoice constrains the model's use of tools.
type ChatToolChoice struct {
	// Mode is "auto", "none", "required", or "tool".
	Mode string `json:"mode"`

	// Name is the tool to force. Required when Mode is "tool".
	Name string `json:"name,omitempty"`
}

// ChatThinking requests that the model reason before answering.
type ChatThinking struct {
	Enabled bool `json:"enabled"`

	// Effort is "low", "medium", "high", or "max". Empty means the provider's
	// default.
	Effort string `json:"effort,omitempty"`
}

// ChatResponse is the JSON body returned by POST /v1/chat.
type ChatResponse struct {
	ID       string `json:"id,omitempty"`
	Provider string `json:"provider"`
	Model    string `json:"model"`

	// Text is every text part concatenated, for callers that want only prose.
	Text string `json:"text"`

	// Message is the assistant's turn in full, in the same shape a request
	// takes. Append it to the conversation and send it back — that is what
	// makes a tool-calling loop possible.
	Message ChatMessage `json:"message"`

	StopReason string          `json:"stop_reason"`
	ToolCalls  []ChatToolCall  `json:"tool_calls,omitempty"`
	Usage      ChatUsage       `json:"usage"`
	Raw        json.RawMessage `json:"raw,omitempty"`
}

// ChatToolCall is a tool invocation the model requested.
type ChatToolCall struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// ChatUsage reports token consumption.
//
// InputTokens is the total input including anything served from or written to
// a cache; the cache figures break it down rather than adding to it.
type ChatUsage struct {
	InputTokens      int `json:"input_tokens"`
	OutputTokens     int `json:"output_tokens"`
	CacheReadTokens  int `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens int `json:"cache_write_tokens,omitempty"`
}

// ModelsResponse is the JSON body of GET /v1/models.
type ModelsResponse struct {
	Provider string      `json:"provider"`
	Models   []ModelItem `json:"models"`
}

// ModelItem describes one model.
type ModelItem struct {
	ID              string `json:"id"`
	DisplayName     string `json:"display_name,omitempty"`
	ContextWindow   int    `json:"context_window,omitempty"`
	MaxOutputTokens int    `json:"max_output_tokens,omitempty"`
}

// ErrorResponse is the JSON body of any non-2xx reply.
type ErrorResponse struct {
	Error string `json:"error"`

	// Kind is the skyl classification — "rate_limit", "auth", and so on —
	// so clients can branch without parsing prose.
	Kind string `json:"kind,omitempty"`
}

// toSkylRequest converts a wire request into a skyl request.
func (r *ChatRequest) toSkylRequest() (*skyl.Request, error) {
	msgs := make([]skyl.Message, 0, len(r.Messages))
	for i, m := range r.Messages {
		msg, err := m.toSkylMessage(i)
		if err != nil {
			return nil, err
		}
		msgs = append(msgs, msg)
	}

	tools := make([]skyl.Tool, 0, len(r.Tools))
	for _, t := range r.Tools {
		tools = append(tools, skyl.Tool{
			Name:        t.Name,
			Description: t.Description,
			Parameters:  t.Parameters,
		})
	}

	out := &skyl.Request{
		Model:           r.Model,
		System:          r.System,
		Messages:        msgs,
		MaxTokens:       r.MaxTokens,
		Temperature:     r.Temperature,
		TopP:            r.TopP,
		Stop:            r.Stop,
		Tools:           tools,
		ProviderOptions: r.ProviderOptions,
	}

	if tc := r.ToolChoice; tc != nil {
		mode := skyl.ToolChoiceMode(tc.Mode)
		switch mode {
		case skyl.ToolChoiceAuto, skyl.ToolChoiceNone,
			skyl.ToolChoiceRequired, skyl.ToolChoiceSpecific:
		default:
			return nil, &badRequestError{fmt.Sprintf(
				"tool_choice: unknown mode %q; want auto, none, required, or tool", tc.Mode)}
		}
		out.ToolChoice = &skyl.ToolChoice{Mode: mode, Name: tc.Name}
	}

	if th := r.Thinking; th != nil {
		out.Thinking = &skyl.Thinking{
			Enabled: th.Enabled,
			Effort:  skyl.Effort(th.Effort),
		}
	}

	return out, nil
}

// toSkylMessage converts one wire turn, rejecting shapes skyl cannot express.
func (m ChatMessage) toSkylMessage(idx int) (skyl.Message, error) {
	role := skyl.Role(m.Role)
	if !role.Valid() {
		return skyl.Message{}, &badRequestError{fmt.Sprintf(
			"message %d: unknown role %q", idx, m.Role)}
	}

	if m.Text != "" && len(m.Content) > 0 {
		return skyl.Message{}, &badRequestError{fmt.Sprintf(
			"message %d: set text or content, not both", idx)}
	}
	if m.Text != "" {
		return skyl.Message{Role: role, Parts: []skyl.Part{skyl.Text{Text: m.Text}}}, nil
	}

	parts := make([]skyl.Part, 0, len(m.Content))
	for j, p := range m.Content {
		part, err := p.toSkylPart(idx, j)
		if err != nil {
			return skyl.Message{}, err
		}
		parts = append(parts, part)
	}
	return skyl.Message{Role: role, Parts: parts}, nil
}

// toSkylPart converts one part.
//
// The validation here mirrors skyl.Request.Validate rather than deferring to
// it, so the error names the wire field the caller actually sent.
func (p ChatPart) toSkylPart(msgIdx, partIdx int) (skyl.Part, error) {
	where := fmt.Sprintf("message %d part %d", msgIdx, partIdx)

	switch p.Type {
	case PartText:
		return skyl.Text{Text: p.Text}, nil

	case PartImage:
		if p.Data == "" && p.URL == "" {
			return nil, &badRequestError{where + ": image needs data or url"}
		}
		if p.Data != "" {
			if p.MediaType == "" {
				return nil, &badRequestError{where + ": image data needs a media_type"}
			}
			raw, err := base64.StdEncoding.DecodeString(p.Data)
			if err != nil {
				return nil, &badRequestError{where + ": image data is not valid base64"}
			}
			return skyl.Image{MediaType: p.MediaType, Data: raw}, nil
		}
		return skyl.Image{MediaType: p.MediaType, URL: p.URL}, nil

	case PartToolCall:
		if p.ID == "" || p.Name == "" {
			return nil, &badRequestError{where + ": tool_call needs an id and a name"}
		}
		return skyl.ToolCall{ID: p.ID, Name: p.Name, Arguments: p.Arguments}, nil

	case PartToolResult:
		if p.ToolCallID == "" {
			return nil, &badRequestError{where + ": tool_result needs a tool_call_id"}
		}
		return skyl.ToolResult{
			CallID:  p.ToolCallID,
			Content: p.Content,
			IsError: p.IsError,
		}, nil

	case "":
		return nil, &badRequestError{where + ": part has no type"}

	default:
		return nil, &badRequestError{fmt.Sprintf(
			"%s: unknown part type %q; want text, image, tool_call, or tool_result",
			where, p.Type)}
	}
}

// fromSkylMessage renders an assistant turn in the shape a request takes, so a
// caller can append it to the conversation and send it straight back.
func fromSkylMessage(m skyl.Message) ChatMessage {
	out := ChatMessage{Role: string(m.Role), Content: make([]ChatPart, 0, len(m.Parts))}
	for _, part := range m.Parts {
		switch v := part.(type) {
		case skyl.Text:
			out.Content = append(out.Content, ChatPart{Type: PartText, Text: v.Text})
		case skyl.Image:
			p := ChatPart{Type: PartImage, MediaType: v.MediaType, URL: v.URL}
			if len(v.Data) > 0 {
				p.Data = base64.StdEncoding.EncodeToString(v.Data)
			}
			out.Content = append(out.Content, p)
		case skyl.ToolCall:
			out.Content = append(out.Content, ChatPart{
				Type: PartToolCall, ID: v.ID, Name: v.Name, Arguments: v.Arguments,
			})
		case skyl.ToolResult:
			out.Content = append(out.Content, ChatPart{
				Type: PartToolResult, ToolCallID: v.CallID,
				Content: v.Content, IsError: v.IsError,
			})
		}
	}
	return out
}

// toChatResponse converts a skyl response into a wire response.
//
// includeRaw is off by default: the raw provider body can echo request content
// back to a caller who should not necessarily see it.
func toChatResponse(resp *skyl.Response, includeRaw bool) ChatResponse {
	out := ChatResponse{
		ID:         resp.ID,
		Provider:   resp.Provider,
		Model:      resp.Model,
		Text:       resp.Text(),
		Message:    fromSkylMessage(resp.Message),
		StopReason: string(resp.StopReason),
		Usage: ChatUsage{
			InputTokens:      resp.Usage.InputTokens,
			OutputTokens:     resp.Usage.OutputTokens,
			CacheReadTokens:  resp.Usage.CacheReadTokens,
			CacheWriteTokens: resp.Usage.CacheWriteTokens,
		},
	}
	for _, c := range resp.ToolCalls() {
		out.ToolCalls = append(out.ToolCalls, ChatToolCall{
			ID: c.ID, Name: c.Name, Arguments: c.Arguments,
		})
	}
	if includeRaw {
		out.Raw = resp.Raw
	}
	return out
}

// badRequestError marks a client-side problem so the handler can answer 400.
type badRequestError struct{ msg string }

func (e *badRequestError) Error() string { return e.msg }
