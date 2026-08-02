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
	"encoding/json"

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

	// ProviderOptions is forwarded verbatim as skyl's escape hatch.
	ProviderOptions map[string]any `json:"provider_options,omitempty"`
}

// ChatMessage is one conversation turn over the wire.
type ChatMessage struct {
	// Role is "user", "assistant", or "tool".
	Role string `json:"role"`

	// Content is the message text.
	Content string `json:"content"`

	// ToolCallID correlates a "tool" message with the call it answers.
	ToolCallID string `json:"tool_call_id,omitempty"`
}

// ChatTool describes a tool the model may call.
type ChatTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

// ChatResponse is the JSON body returned by POST /v1/chat.
type ChatResponse struct {
	ID         string          `json:"id,omitempty"`
	Provider   string          `json:"provider"`
	Model      string          `json:"model"`
	Text       string          `json:"text"`
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
		role := skyl.Role(m.Role)
		switch role {
		case skyl.RoleUser:
			msgs = append(msgs, skyl.UserText(m.Content))
		case skyl.RoleAssistant:
			msgs = append(msgs, skyl.AssistantText(m.Content))
		case skyl.RoleTool:
			if m.ToolCallID == "" {
				return nil, &badRequestError{"message " + itoa(i) + ": tool messages need a tool_call_id"}
			}
			msgs = append(msgs, skyl.ToolResultMessage(m.ToolCallID, m.Content))
		default:
			return nil, &badRequestError{"message " + itoa(i) + ": unknown role " + m.Role}
		}
	}

	tools := make([]skyl.Tool, 0, len(r.Tools))
	for _, t := range r.Tools {
		tools = append(tools, skyl.Tool{
			Name:        t.Name,
			Description: t.Description,
			Parameters:  t.Parameters,
		})
	}

	return &skyl.Request{
		Model:           r.Model,
		System:          r.System,
		Messages:        msgs,
		MaxTokens:       r.MaxTokens,
		Temperature:     r.Temperature,
		TopP:            r.TopP,
		Stop:            r.Stop,
		Tools:           tools,
		ProviderOptions: r.ProviderOptions,
	}, nil
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

// itoa avoids pulling strconv in for one call site.
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(buf[pos:])
}
