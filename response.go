package skyl

import "encoding/json"

// StopReason explains why the model stopped generating.
type StopReason string

// The stop reasons skyl understands. Adapters map provider-specific values
// onto these and fall back to [StopUnknown] rather than inventing a new one.
const (
	// StopEndTurn means the model finished naturally.
	StopEndTurn StopReason = "end_turn"

	// StopMaxTokens means the output hit [Request.MaxTokens]. The response is
	// truncated — treat it as incomplete.
	StopMaxTokens StopReason = "max_tokens"

	// StopToolUse means the model wants a tool run. Execute the calls and
	// send the results back.
	StopToolUse StopReason = "tool_use"

	// StopStopSequence means a sequence from [Request.Stop] was produced.
	StopStopSequence StopReason = "stop_sequence"

	// StopRefusal means the model or its safety classifiers declined. The
	// content may be empty or partial; do not retry the same request.
	StopRefusal StopReason = "refusal"

	// StopUnknown means the provider reported something skyl does not model.
	// Read [Response.Raw] for the original value.
	StopUnknown StopReason = "unknown"
)

// Usage reports token consumption for a request.
//
// Providers do not all report every field; zero means "not reported", not
// "zero tokens".
//
// # Inclusion semantics
//
// Providers disagree about whether cached tokens are part of the input count.
// OpenAI and Gemini report a cache figure that is a subset of the prompt count;
// Anthropic reports cache figures that are disjoint from its input count.
// Summing the fields blindly therefore over-reports on some providers and not
// on others, which is exactly the kind of difference a caller should not have
// to know about.
//
// skyl normalises to one rule, and every adapter obeys it:
//
//   - InputTokens is the total input, cached tokens included.
//   - CacheReadTokens and CacheWriteTokens are a breakdown OF InputTokens,
//     not an addition to it.
//
// So InputTokens is what to bill, and CacheReadTokens is how much of it was
// discounted.
type Usage struct {
	// InputTokens is every token of input, including any served from or
	// written to a cache.
	InputTokens int

	// OutputTokens is every token the model generated.
	OutputTokens int

	// CacheReadTokens were served from a prompt cache, usually at a large
	// discount. They are part of InputTokens, not additional to it.
	CacheReadTokens int

	// CacheWriteTokens were written to a prompt cache, usually at a premium.
	// They are part of InputTokens, not additional to it.
	CacheWriteTokens int
}

// TotalTokens returns every token the provider reported.
//
// Cache figures are a breakdown of InputTokens, so they are deliberately not
// added again — see the inclusion semantics on [Usage].
func (u Usage) TotalTokens() int {
	return u.InputTokens + u.OutputTokens
}

// Add returns the sum of two usage records, for accumulating across a
// multi-turn exchange.
func (u Usage) Add(other Usage) Usage {
	return Usage{
		InputTokens:      u.InputTokens + other.InputTokens,
		OutputTokens:     u.OutputTokens + other.OutputTokens,
		CacheReadTokens:  u.CacheReadTokens + other.CacheReadTokens,
		CacheWriteTokens: u.CacheWriteTokens + other.CacheWriteTokens,
	}
}

// Response is a provider-agnostic model reply.
type Response struct {
	// ID is the provider's identifier for this response, when it supplies one.
	ID string

	// Provider is the adapter that produced this response, for example
	// "anthropic".
	Provider string

	// Model is the model that actually served the request, read from the
	// response rather than echoed from the request — providers can and do
	// serve a different model than the one asked for.
	Model string

	// Message is the assistant's turn. Append it to your conversation before
	// sending tool results.
	Message Message

	// StopReason explains why generation ended.
	StopReason StopReason

	// Usage reports token consumption.
	Usage Usage

	// Raw is the provider's untouched response body.
	//
	// It is always populated. Use it to read anything skyl does not model, so
	// that skyl's abstraction is never the reason you cannot ship.
	Raw json.RawMessage
}

// Text returns the response's text content.
//
// It is shorthand for Response.Message.Text().
func (r *Response) Text() string {
	if r == nil {
		return ""
	}
	return r.Message.Text()
}

// ToolCalls returns the tool calls the model requested, in order.
//
// It returns nil when the model asked for none, so a plain range is safe.
func (r *Response) ToolCalls() []ToolCall {
	if r == nil {
		return nil
	}
	return r.Message.ToolCalls()
}

// ModelInfo describes a model a provider offers.
//
// It is what [Provider.Models] returns. Fields beyond ID are best-effort:
// providers differ in what their model endpoints disclose, and zero means "not
// reported".
type ModelInfo struct {
	// ID is the identifier to put in [Request.Model].
	ID string

	// Provider is the adapter that offers it.
	Provider string

	// DisplayName is a human-readable name, when the provider supplies one.
	DisplayName string

	// ContextWindow is the maximum input size in tokens.
	ContextWindow int

	// MaxOutputTokens is the maximum response size in tokens.
	MaxOutputTokens int

	// Raw is the provider's untouched entry for this model.
	Raw json.RawMessage
}
