package skyl

import "fmt"

// Tool describes a function the model may ask to invoke.
type Tool struct {
	// Name is the identifier the model uses to call the tool.
	Name string

	// Description tells the model when to use the tool. Be prescriptive about
	// *when* to call it, not only what it does — trigger conditions measurably
	// improve tool selection.
	Description string

	// Parameters is a JSON Schema object describing the tool's input.
	Parameters map[string]any
}

// ToolChoiceMode controls whether and how the model may call tools.
type ToolChoiceMode string

// The tool-choice modes skyl understands.
const (
	// ToolChoiceAuto lets the model decide. This is the default.
	ToolChoiceAuto ToolChoiceMode = "auto"

	// ToolChoiceNone forbids tool calls for this request.
	ToolChoiceNone ToolChoiceMode = "none"

	// ToolChoiceRequired forces at least one tool call.
	ToolChoiceRequired ToolChoiceMode = "required"

	// ToolChoiceSpecific forces a named tool. Set [ToolChoice.Name].
	ToolChoiceSpecific ToolChoiceMode = "tool"
)

// ToolChoice constrains the model's use of tools.
type ToolChoice struct {
	Mode ToolChoiceMode

	// Name is the tool to force. Required when Mode is [ToolChoiceSpecific],
	// ignored otherwise.
	Name string
}

// Effort hints how much reasoning the model should spend.
//
// Providers interpret it differently and some ignore it. It is a hint, not a
// contract.
type Effort string

// The effort levels skyl understands, in increasing order.
const (
	EffortLow    Effort = "low"
	EffortMedium Effort = "medium"
	EffortHigh   Effort = "high"
	EffortMax    Effort = "max"
)

// Thinking requests that the model reason before answering.
//
// Support varies: some models always think, some never do, and some accept an
// effort hint. Adapters map this onto whatever the provider offers and ignore
// it where the provider has no equivalent.
type Thinking struct {
	// Enabled requests reasoning. A zero Thinking value is not the same as a
	// nil *Thinking: nil means "provider default", &Thinking{} means
	// "explicitly off".
	Enabled bool

	// Effort hints at depth. Empty means the provider's default.
	Effort Effort
}

// Request is a provider-agnostic model call.
//
// The same Request can be sent to any provider. Fields a provider does not
// support are ignored rather than rejected, except where ignoring them would
// silently lose data — see [ErrUnsupported].
type Request struct {
	// Model is the provider's model identifier, passed through untouched.
	//
	// skyl never validates this against a list of known models, so a model
	// released after your skyl build works immediately. A typo therefore
	// surfaces as the provider's own not-found error rather than a local one.
	Model string

	// System is the system prompt. Adapters place it where the provider
	// expects — a top-level field, a leading message, or systemInstruction.
	System string

	// Messages is the conversation so far. It must not be empty.
	//
	// skyl does not police the ordering of roles: providers disagree about
	// what is legal — a leading assistant turn, two user turns in a row — and
	// rejecting a shape one vendor accepts would be skyl deciding something it
	// has no business deciding. An ordering a provider dislikes comes back as
	// that provider's own error.
	Messages []Message

	// MaxTokens caps the response length. Zero means the provider's default,
	// which for some providers is an error — set it explicitly.
	MaxTokens int

	// Temperature and TopP are sampling controls, nil for the provider
	// default.
	//
	// A non-nil value is always sent. Several current reasoning models reject
	// these outright, and skyl does not second-guess that: silently dropping a
	// field you set would be worse than the provider's own error, because you
	// would have no way to tell it had happened. Leave them nil unless you
	// mean them.
	Temperature *float64
	TopP        *float64

	// Stop are sequences that end generation.
	Stop []string

	// Tools the model may call.
	Tools []Tool

	// ToolChoice constrains tool use. Nil means [ToolChoiceAuto].
	ToolChoice *ToolChoice

	// Thinking requests reasoning. Nil means the provider's default.
	Thinking *Thinking

	// ProviderOptions is an escape hatch: arbitrary vendor-specific fields
	// merged into the outbound payload, overriding anything skyl set.
	//
	// Use it to reach a feature skyl does not model. skyl does not validate
	// the contents — that is the point.
	ProviderOptions map[string]any
}

// Validate reports whether the request is well-formed.
//
// [Client] calls it before dispatching, so a malformed request fails locally
// rather than costing a round trip. Adapters may impose further requirements.
func (r *Request) Validate() error {
	if r == nil {
		return fmt.Errorf("%w: nil request", ErrBadRequest)
	}
	if r.Model == "" {
		return fmt.Errorf("%w: model is required", ErrBadRequest)
	}
	if len(r.Messages) == 0 {
		return fmt.Errorf("%w: at least one message is required", ErrBadRequest)
	}
	if r.MaxTokens < 0 {
		return fmt.Errorf("%w: max tokens must not be negative", ErrBadRequest)
	}

	for i, m := range r.Messages {
		if !m.Role.Valid() {
			return fmt.Errorf("%w: message %d has invalid role %q", ErrBadRequest, i, m.Role)
		}
		if len(m.Parts) == 0 {
			return fmt.Errorf("%w: message %d has no parts", ErrBadRequest, i)
		}
		for j, p := range m.Parts {
			if err := validatePart(i, j, p); err != nil {
				return err
			}
		}
	}

	for i, t := range r.Tools {
		if t.Name == "" {
			return fmt.Errorf("%w: tool %d has no name", ErrBadRequest, i)
		}
	}

	if r.ToolChoice != nil {
		switch r.ToolChoice.Mode {
		case ToolChoiceAuto, ToolChoiceNone, ToolChoiceRequired:
		case ToolChoiceSpecific:
			if r.ToolChoice.Name == "" {
				return fmt.Errorf("%w: tool choice %q requires a name", ErrBadRequest, ToolChoiceSpecific)
			}
		default:
			return fmt.Errorf("%w: unknown tool choice mode %q", ErrBadRequest, r.ToolChoice.Mode)
		}
	}

	return nil
}

func validatePart(msgIdx, partIdx int, p Part) error {
	switch v := p.(type) {
	case Text:
		return nil
	case Image:
		if len(v.Data) == 0 && v.URL == "" {
			return fmt.Errorf("%w: message %d part %d: image needs data or a URL",
				ErrBadRequest, msgIdx, partIdx)
		}
		if len(v.Data) > 0 && v.MediaType == "" {
			return fmt.Errorf("%w: message %d part %d: image data needs a media type",
				ErrBadRequest, msgIdx, partIdx)
		}
		return nil
	case ToolCall:
		if v.ID == "" || v.Name == "" {
			return fmt.Errorf("%w: message %d part %d: tool call needs an ID and a name",
				ErrBadRequest, msgIdx, partIdx)
		}
		return nil
	case ToolResult:
		if v.CallID == "" {
			return fmt.Errorf("%w: message %d part %d: tool result needs a call ID",
				ErrBadRequest, msgIdx, partIdx)
		}
		return nil
	case nil:
		return fmt.Errorf("%w: message %d part %d is nil", ErrBadRequest, msgIdx, partIdx)
	default:
		return fmt.Errorf("%w: message %d part %d has unknown type %T",
			ErrBadRequest, msgIdx, partIdx, p)
	}
}
