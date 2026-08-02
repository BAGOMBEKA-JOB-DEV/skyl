// Package anthropic adapts the Anthropic Claude API.
//
// It reaches Claude Opus 5, Fable 5, Sonnet 5, Haiku 4.5, and the 4.x family.
// Model IDs are passed through untouched, so a model released after your skyl
// build works immediately — see docs/adr/0004-model-ids-are-pass-through.md.
//
// # A separate module
//
// This adapter is its own Go module because it depends on the official
// anthropic-sdk-go, which brings a dozen transitive dependencies. Keeping it
// out of the core module means a user who only wants OpenAI does not inherit
// them. Install it explicitly:
//
//	go get github.com/BAGOMBEKA-JOB-DEV/skyl/provider/anthropic
package anthropic

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	sdk "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/packages/ssestream"

	"github.com/BAGOMBEKA-JOB-DEV/skyl"
)

const providerName = "anthropic"

// defaultMaxTokens is used when a request leaves MaxTokens unset. The
// Anthropic API requires the field, so skyl must supply something rather than
// fail a request that every other provider would accept.
const defaultMaxTokens = 4096

// Provider adapts the Anthropic API. It is safe for concurrent use.
type Provider struct {
	client sdk.Client
}

// Option configures a [Provider].
type Option func(*[]option.RequestOption)

// WithBaseURL overrides the API root — for a proxy or a compatible gateway.
func WithBaseURL(url string) Option {
	return func(o *[]option.RequestOption) { *o = append(*o, option.WithBaseURL(url)) }
}

// WithHTTPClient supplies the HTTP client, for custom transports, proxies,
// instrumentation, or a private trust store.
func WithHTTPClient(hc *http.Client) Option {
	return func(o *[]option.RequestOption) { *o = append(*o, option.WithHTTPClient(hc)) }
}

// WithHeader adds a header to every request, for beta flags and the like.
func WithHeader(key, value string) Option {
	return func(o *[]option.RequestOption) { *o = append(*o, option.WithHeader(key, value)) }
}

// New returns a Provider authenticating with apiKey.
//
//	p := anthropic.New(os.Getenv("ANTHROPIC_API_KEY"))
//
// The underlying SDK retries on its own; skyl's [skyl.Client] retries too, so
// this adapter disables the SDK's retries to keep backoff in one place.
func New(apiKey string, opts ...Option) *Provider {
	reqOpts := []option.RequestOption{
		option.WithAPIKey(apiKey),
		option.WithMaxRetries(0),
	}
	for _, opt := range opts {
		opt(&reqOpts)
	}
	return &Provider{client: sdk.NewClient(reqOpts...)}
}

// Name returns "anthropic".
func (p *Provider) Name() string { return providerName }

// ---------------------------------------------------------------------------
// Request mapping
// ---------------------------------------------------------------------------

func buildParams(req *skyl.Request) (sdk.MessageNewParams, error) {
	maxTokens := int64(req.MaxTokens)
	if maxTokens <= 0 {
		maxTokens = defaultMaxTokens
	}

	params := sdk.MessageNewParams{
		Model:     sdk.Model(req.Model),
		MaxTokens: maxTokens,
	}

	if req.System != "" {
		params.System = []sdk.TextBlockParam{{Text: req.System}}
	}
	if req.Temperature != nil {
		params.Temperature = sdk.Float(*req.Temperature)
	}
	if req.TopP != nil {
		params.TopP = sdk.Float(*req.TopP)
	}
	if len(req.Stop) > 0 {
		params.StopSequences = req.Stop
	}

	msgs := make([]sdk.MessageParam, 0, len(req.Messages))
	for i, m := range req.Messages {
		converted, err := convertMessage(i, m)
		if err != nil {
			return params, err
		}
		msgs = append(msgs, converted)
	}
	params.Messages = msgs

	if len(req.Tools) > 0 {
		tools := make([]sdk.ToolUnionParam, 0, len(req.Tools))
		for _, t := range req.Tools {
			schema := sdk.ToolInputSchemaParam{}
			if t.Parameters != nil {
				if props, ok := t.Parameters["properties"]; ok {
					schema.Properties = props
				}
				if req, ok := t.Parameters["required"].([]string); ok {
					schema.Required = req
				}
			}
			tool := sdk.ToolParam{Name: t.Name, InputSchema: schema}
			if t.Description != "" {
				tool.Description = sdk.String(t.Description)
			}
			tools = append(tools, sdk.ToolUnionParam{OfTool: &tool})
		}
		params.Tools = tools
	}

	if tc := req.ToolChoice; tc != nil {
		switch tc.Mode {
		case skyl.ToolChoiceAuto:
			params.ToolChoice = sdk.ToolChoiceUnionParam{OfAuto: &sdk.ToolChoiceAutoParam{}}
		case skyl.ToolChoiceNone:
			params.ToolChoice = sdk.ToolChoiceUnionParam{OfNone: &sdk.ToolChoiceNoneParam{}}
		case skyl.ToolChoiceRequired:
			params.ToolChoice = sdk.ToolChoiceUnionParam{OfAny: &sdk.ToolChoiceAnyParam{}}
		case skyl.ToolChoiceSpecific:
			params.ToolChoice = sdk.ToolChoiceUnionParam{
				OfTool: &sdk.ToolChoiceToolParam{Name: tc.Name},
			}
		}
	}

	if th := req.Thinking; th != nil {
		if th.Enabled {
			params.Thinking = sdk.ThinkingConfigParamUnion{
				OfAdaptive: &sdk.ThinkingConfigAdaptiveParam{},
			}
		} else {
			params.Thinking = sdk.ThinkingConfigParamUnion{
				OfDisabled: &sdk.ThinkingConfigDisabledParam{},
			}
		}
	}

	return params, nil
}

func convertMessage(idx int, m skyl.Message) (sdk.MessageParam, error) {
	blocks := make([]sdk.ContentBlockParamUnion, 0, len(m.Parts))

	for _, part := range m.Parts {
		switch v := part.(type) {
		case skyl.Text:
			blocks = append(blocks, sdk.NewTextBlock(v.Text))

		case skyl.Image:
			if v.URL != "" && len(v.Data) == 0 {
				blocks = append(blocks, sdk.NewImageBlock(sdk.URLImageSourceParam{URL: v.URL}))
				continue
			}
			blocks = append(blocks, sdk.NewImageBlockBase64(
				v.MediaType, base64.StdEncoding.EncodeToString(v.Data)))

		case skyl.ToolCall:
			var input any
			if len(v.Arguments) > 0 {
				if err := json.Unmarshal(v.Arguments, &input); err != nil {
					return sdk.MessageParam{}, skyl.Unsupportedf(providerName,
						"message %d: tool call arguments are not valid JSON: %v", idx, err)
				}
			} else {
				input = map[string]any{}
			}
			blocks = append(blocks, sdk.NewToolUseBlock(v.ID, input, v.Name))

		case skyl.ToolResult:
			blocks = append(blocks, sdk.NewToolResultBlock(v.CallID, v.Content, v.IsError))

		default:
			return sdk.MessageParam{}, skyl.Unsupportedf(providerName,
				"message %d: cannot represent part of type %T", idx, part)
		}
	}

	switch m.Role {
	case skyl.RoleAssistant:
		return sdk.NewAssistantMessage(blocks...), nil
	case skyl.RoleUser, skyl.RoleTool:
		// Anthropic has no tool role: results are user-turn content blocks.
		return sdk.NewUserMessage(blocks...), nil
	default:
		return sdk.MessageParam{}, skyl.Unsupportedf(providerName,
			"message %d: unknown role %q", idx, m.Role)
	}
}

// ---------------------------------------------------------------------------
// Response mapping
// ---------------------------------------------------------------------------

func mapStopReason(s sdk.StopReason) skyl.StopReason {
	switch s {
	case sdk.StopReasonEndTurn:
		return skyl.StopEndTurn
	case sdk.StopReasonMaxTokens, sdk.StopReasonModelContextWindowExceeded:
		return skyl.StopMaxTokens
	case sdk.StopReasonToolUse:
		return skyl.StopToolUse
	case sdk.StopReasonStopSequence:
		return skyl.StopStopSequence
	case sdk.StopReasonRefusal:
		return skyl.StopRefusal
	default:
		return skyl.StopUnknown
	}
}

func partsFromMessage(msg *sdk.Message) []skyl.Part {
	var parts []skyl.Part
	for _, block := range msg.Content {
		switch b := block.AsAny().(type) {
		case sdk.TextBlock:
			if b.Text != "" {
				parts = append(parts, skyl.Text{Text: b.Text})
			}
		case sdk.ToolUseBlock:
			args := json.RawMessage(b.JSON.Input.Raw())
			if len(args) == 0 {
				args = json.RawMessage("{}")
			}
			parts = append(parts, skyl.ToolCall{ID: b.ID, Name: b.Name, Arguments: args})
		}
	}
	return parts
}

func usageFrom(u sdk.Usage) skyl.Usage {
	return skyl.Usage{
		InputTokens:      int(u.InputTokens),
		OutputTokens:     int(u.OutputTokens),
		CacheReadTokens:  int(u.CacheReadInputTokens),
		CacheWriteTokens: int(u.CacheCreationInputTokens),
	}
}

// Complete runs a request to completion.
func (p *Provider) Complete(ctx context.Context, req *skyl.Request) (*skyl.Response, error) {
	params, err := buildParams(req)
	if err != nil {
		return nil, err
	}

	msg, err := p.client.Messages.New(ctx, params)
	if err != nil {
		return nil, translateError(err)
	}

	model := string(msg.Model)
	if model == "" {
		model = req.Model
	}

	return &skyl.Response{
		ID:         msg.ID,
		Provider:   providerName,
		Model:      model,
		Message:    skyl.Message{Role: skyl.RoleAssistant, Parts: partsFromMessage(msg)},
		StopReason: mapStopReason(msg.StopReason),
		Usage:      usageFrom(msg.Usage),
		Raw:        json.RawMessage(msg.RawJSON()),
	}, nil
}

// Models lists the models available to this API key.
func (p *Provider) Models(ctx context.Context) ([]skyl.ModelInfo, error) {
	iter := p.client.Models.ListAutoPaging(ctx, sdk.ModelListParams{})

	var out []skyl.ModelInfo
	for iter.Next() {
		m := iter.Current()
		out = append(out, skyl.ModelInfo{
			ID:              m.ID,
			Provider:        providerName,
			DisplayName:     m.DisplayName,
			ContextWindow:   int(m.MaxInputTokens),
			MaxOutputTokens: int(m.MaxTokens),
			Raw:             json.RawMessage(m.RawJSON()),
		})
	}
	if err := iter.Err(); err != nil {
		return nil, translateError(err)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Errors
// ---------------------------------------------------------------------------

// translateError maps an SDK error onto skyl's classification, so callers can
// branch with errors.Is regardless of which provider produced the failure.
func translateError(err error) error {
	if err == nil {
		return nil
	}

	var apiErr *sdk.Error
	if !errors.As(err, &apiErr) {
		// No HTTP response: a dial failure, a reset, a cancelled context.
		return &skyl.Error{Provider: providerName, Message: err.Error()}
	}

	e := skyl.NewError(
		providerName,
		apiErr.StatusCode,
		skyl.ClassifyStatus(apiErr.StatusCode),
		apiErr.Error(),
		nil,
	)
	if apiErr.Response != nil {
		e.RetryAfter = skyl.ParseRetryAfter(apiErr.Response.Header.Get("Retry-After"))
	}
	return e
}

// ---------------------------------------------------------------------------
// Streaming
// ---------------------------------------------------------------------------

// Stream runs a request, returning events as the model produces them.
func (p *Provider) Stream(ctx context.Context, req *skyl.Request) (skyl.Stream, error) {
	params, err := buildParams(req)
	if err != nil {
		return nil, err
	}

	sdkStream := p.client.Messages.NewStreaming(ctx, params)
	// NewStreaming defers the request, so a handshake failure surfaces on the
	// first read rather than here. Check for it now so that skyl's Client can
	// retry a failed handshake instead of handing back a dead stream.
	if err := sdkStream.Err(); err != nil {
		_ = sdkStream.Close()
		return nil, translateError(err)
	}

	return &stream{sdk: sdkStream}, nil
}

// stream adapts the SDK's event stream to skyl.Stream.
//
// It accumulates into an sdk.Message as events arrive: text is emitted live,
// while tool calls are held back until their JSON arguments are whole, because
// a half-parsed tool call is not actionable.
type stream struct {
	sdk *ssestream.Stream[sdk.MessageStreamEventUnion]

	acc sdk.Message
	ev  skyl.StreamEvent
	err error

	queued  []skyl.StreamEvent
	flushed bool
	done    bool
	closed  bool
}

func (s *stream) Next() bool {
	if s.done || s.err != nil {
		return false
	}
	if len(s.queued) > 0 {
		s.ev = s.queued[0]
		s.queued = s.queued[1:]
		return true
	}

	for s.sdk.Next() {
		event := s.sdk.Current()
		if err := s.acc.Accumulate(event); err != nil {
			s.err = &skyl.Error{Provider: providerName, Message: err.Error()}
			return false
		}

		if delta, ok := event.AsAny().(sdk.ContentBlockDeltaEvent); ok {
			switch d := delta.Delta.AsAny().(type) {
			case sdk.TextDelta:
				if d.Text != "" {
					s.ev = skyl.StreamEvent{Type: skyl.EventTextDelta, Text: d.Text}
					return true
				}
			case sdk.ThinkingDelta:
				if d.Thinking != "" {
					s.ev = skyl.StreamEvent{Type: skyl.EventThinkingDelta, Text: d.Thinking}
					return true
				}
			}
		}
	}

	if err := s.sdk.Err(); err != nil {
		s.err = translateError(err)
		return false
	}

	return s.finish()
}

// finish emits buffered tool calls, then the terminal event.
func (s *stream) finish() bool {
	if !s.flushed {
		s.flushed = true
		for _, part := range partsFromMessage(&s.acc) {
			if call, ok := part.(skyl.ToolCall); ok {
				c := call
				s.queued = append(s.queued, skyl.StreamEvent{
					Type: skyl.EventToolCall, ToolCall: &c,
				})
			}
		}
		usage := usageFrom(s.acc.Usage)
		s.queued = append(s.queued, skyl.StreamEvent{
			Type:       skyl.EventDone,
			Usage:      &usage,
			StopReason: mapStopReason(s.acc.StopReason),
		})
	}

	if len(s.queued) > 0 {
		s.ev = s.queued[0]
		s.queued = s.queued[1:]
		return true
	}

	s.done = true
	return false
}

func (s *stream) Event() skyl.StreamEvent { return s.ev }

func (s *stream) Err() error { return s.err }

func (s *stream) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	if err := s.sdk.Close(); err != nil {
		return fmt.Errorf("skyl: closing anthropic stream: %w", err)
	}
	return nil
}

// Verify at compile time that Provider satisfies the interface.
var _ skyl.Provider = (*Provider)(nil)
