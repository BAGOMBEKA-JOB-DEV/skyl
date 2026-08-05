// Package gemini adapts the Google Gemini API.
//
// It reaches Gemini 3.6 Flash, 3.5/3.1 Flash-Lite, and 3 Pro. Model IDs are
// passed through untouched, so a model released after your skyl build works
// immediately — see docs/adr/0004-model-ids-are-pass-through.md.
//
// # Tool call identity
//
// Gemini does not assign IDs to function calls; it correlates a response with
// a call by function name. This adapter therefore sets [skyl.ToolCall.ID] to
// the function name, so replaying a [skyl.ToolResult] round-trips correctly
// with no special handling by the caller.
package gemini

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/BAGOMBEKA-JOB-DEV/skyl"
	"github.com/BAGOMBEKA-JOB-DEV/skyl/internal/httpx"
	"github.com/BAGOMBEKA-JOB-DEV/skyl/internal/sse"
)

// DefaultBaseURL is the Gemini API root.
const DefaultBaseURL = "https://generativelanguage.googleapis.com/v1beta"

const providerName = "gemini"

// Provider adapts the Gemini API. It is safe for concurrent use.
type Provider struct {
	apiKey  string
	baseURL string
	hc      *http.Client
}

// Option configures a [Provider].
type Option func(*Provider)

// WithBaseURL overrides the API root — for a proxy or a regional endpoint.
func WithBaseURL(u string) Option {
	return func(p *Provider) { p.baseURL = strings.TrimRight(u, "/") }
}

// WithHTTPClient supplies the HTTP client, for custom transports, proxies,
// instrumentation, or a private trust store.
func WithHTTPClient(hc *http.Client) Option {
	return func(p *Provider) { p.hc = hc }
}

// New returns a Provider authenticating with apiKey.
//
//	p := gemini.New(os.Getenv("GEMINI_API_KEY"))
func New(apiKey string, opts ...Option) *Provider {
	p := &Provider{apiKey: apiKey, baseURL: DefaultBaseURL, hc: httpx.DefaultClient()}
	for _, opt := range opts {
		opt(p)
	}
	if p.hc == nil {
		p.hc = httpx.DefaultClient()
	}
	return p
}

// Name returns "gemini".
func (p *Provider) Name() string { return providerName }

func (p *Provider) headers() map[string]string {
	h := map[string]string{}
	if p.apiKey != "" {
		// Header auth keeps the key out of URLs, and therefore out of proxy
		// and server access logs.
		h["x-goog-api-key"] = p.apiKey
	}
	return h
}

// ---------------------------------------------------------------------------
// Wire types
// ---------------------------------------------------------------------------

type wirePart struct {
	Text       string `json:"text,omitempty"`
	InlineData *struct {
		MimeType string `json:"mimeType"`
		Data     string `json:"data"`
	} `json:"inlineData,omitempty"`
	FunctionCall *struct {
		Name string          `json:"name"`
		Args json.RawMessage `json:"args"`
	} `json:"functionCall,omitempty"`
	FunctionResponse *struct {
		Name     string          `json:"name"`
		Response json.RawMessage `json:"response"`
	} `json:"functionResponse,omitempty"`
}

type wireContent struct {
	Role  string     `json:"role,omitempty"`
	Parts []wirePart `json:"parts"`
}

type wireResponse struct {
	Candidates []struct {
		Content      wireContent `json:"content"`
		FinishReason string      `json:"finishReason"`
	} `json:"candidates"`
	UsageMetadata struct {
		PromptTokenCount        int `json:"promptTokenCount"`
		CandidatesTokenCount    int `json:"candidatesTokenCount"`
		CachedContentTokenCount int `json:"cachedContentTokenCount"`
	} `json:"usageMetadata"`
	ModelVersion   string `json:"modelVersion"`
	ResponseID     string `json:"responseId"`
	PromptFeedback *struct {
		BlockReason string `json:"blockReason"`
	} `json:"promptFeedback,omitempty"`
	Error *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Status  string `json:"status"`
	} `json:"error,omitempty"`
}

// ---------------------------------------------------------------------------
// Request mapping
// ---------------------------------------------------------------------------

func (p *Provider) buildPayload(req *skyl.Request) (map[string]any, error) {
	contents := make([]wireContent, 0, len(req.Messages))

	for i, m := range req.Messages {
		c, err := convertMessage(i, m)
		if err != nil {
			return nil, err
		}
		contents = append(contents, c)
	}

	payload := map[string]any{"contents": contents}

	if req.System != "" {
		payload["systemInstruction"] = wireContent{
			Parts: []wirePart{{Text: req.System}},
		}
	}

	gen := map[string]any{}
	if req.MaxTokens > 0 {
		gen["maxOutputTokens"] = req.MaxTokens
	}
	if req.Temperature != nil {
		gen["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		gen["topP"] = *req.TopP
	}
	if len(req.Stop) > 0 {
		gen["stopSequences"] = req.Stop
	}
	if th := req.Thinking; th != nil {
		// Gemini expresses reasoning as a token budget inside
		// generationConfig. A nil Thinking leaves the model's default alone;
		// an explicit &Thinking{Enabled: false} means off, and a budget of
		// zero is how Gemini spells that — without this, disabling reasoning
		// silently did nothing and the caller kept paying for it.
		tc := map[string]any{}
		if th.Enabled {
			if budget, ok := thinkingBudget(th.Effort); ok {
				tc["thinkingBudget"] = budget
			} else {
				// No effort given: -1 lets the model decide how much to spend.
				tc["thinkingBudget"] = -1
			}
		} else {
			tc["thinkingBudget"] = 0
		}
		gen["thinkingConfig"] = tc
	}
	if rf := req.ResponseFormat; rf != nil {
		// Gemini needs both: the mime type switches the model into JSON mode,
		// and the schema constrains its shape. Sending the schema alone is
		// accepted and then ignored, which is the worst of the two outcomes.
		//
		// ResponseFormat.Name has nowhere to go here; Gemini's schema is
		// anonymous. The field's doc comment says the other providers ignore it.
		gen["responseMimeType"] = "application/json"
		gen["responseSchema"] = rf.Schema
	}
	if len(gen) > 0 {
		payload["generationConfig"] = gen
	}

	if len(req.Tools) > 0 {
		decls := make([]map[string]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			d := map[string]any{"name": t.Name}
			if t.Description != "" {
				d["description"] = t.Description
			}
			if t.Parameters != nil {
				d["parameters"] = t.Parameters
			}
			decls = append(decls, d)
		}
		payload["tools"] = []map[string]any{{"functionDeclarations": decls}}
	}

	if tc := req.ToolChoice; tc != nil {
		fcc := map[string]any{}
		switch tc.Mode {
		case skyl.ToolChoiceAuto:
			fcc["mode"] = "AUTO"
		case skyl.ToolChoiceNone:
			fcc["mode"] = "NONE"
		case skyl.ToolChoiceRequired:
			fcc["mode"] = "ANY"
		case skyl.ToolChoiceSpecific:
			fcc["mode"] = "ANY"
			fcc["allowedFunctionNames"] = []string{tc.Name}
		}
		if len(fcc) > 0 {
			payload["toolConfig"] = map[string]any{"functionCallingConfig": fcc}
		}
	}

	return httpx.Merge(payload, req.ProviderOptions), nil
}

// thinkingBudget maps skyl's effort hint onto a Gemini thinking budget.
//
// The numbers are a coarse translation of an intentionally coarse hint —
// [skyl.Effort] is documented as advisory, not a contract. A caller who needs
// an exact budget sets thinkingConfig through ProviderOptions, which is merged
// last and wins.
func thinkingBudget(e skyl.Effort) (int, bool) {
	switch e {
	case skyl.EffortLow:
		return 1024, true
	case skyl.EffortMedium:
		return 8192, true
	case skyl.EffortHigh:
		return 16384, true
	case skyl.EffortMax:
		return 24576, true
	default:
		return 0, false
	}
}

func convertMessage(idx int, m skyl.Message) (wireContent, error) {
	// Gemini names the assistant role "model" and has no tool role: function
	// results are user-role content carrying functionResponse parts.
	role := "user"
	if m.Role == skyl.RoleAssistant {
		role = "model"
	}

	parts := make([]wirePart, 0, len(m.Parts))
	for _, part := range m.Parts {
		switch v := part.(type) {
		case skyl.Text:
			parts = append(parts, wirePart{Text: v.Text})

		case skyl.Image:
			if v.URL != "" && len(v.Data) == 0 {
				return wireContent{}, skyl.Unsupportedf(providerName,
					"message %d: Gemini requires inline image data, not a URL", idx)
			}
			wp := wirePart{}
			wp.InlineData = &struct {
				MimeType string `json:"mimeType"`
				Data     string `json:"data"`
			}{MimeType: v.MediaType, Data: base64.StdEncoding.EncodeToString(v.Data)}
			parts = append(parts, wp)

		case skyl.ToolCall:
			args := v.Arguments
			if len(args) == 0 {
				args = json.RawMessage("{}")
			}
			wp := wirePart{}
			wp.FunctionCall = &struct {
				Name string          `json:"name"`
				Args json.RawMessage `json:"args"`
			}{Name: v.Name, Args: args}
			parts = append(parts, wp)

		case skyl.ToolResult:
			// Gemini correlates by name; this adapter sets ToolCall.ID to the
			// function name so the round trip works without caller effort.
			payload, err := json.Marshal(map[string]any{"content": v.Content})
			if err != nil {
				return wireContent{}, fmt.Errorf("skyl: encoding tool result: %w", err)
			}
			wp := wirePart{}
			wp.FunctionResponse = &struct {
				Name     string          `json:"name"`
				Response json.RawMessage `json:"response"`
			}{Name: v.CallID, Response: payload}
			parts = append(parts, wp)

		default:
			return wireContent{}, skyl.Unsupportedf(providerName,
				"message %d: cannot represent part of type %T", idx, part)
		}
	}

	return wireContent{Role: role, Parts: parts}, nil
}

// ---------------------------------------------------------------------------
// Response mapping
// ---------------------------------------------------------------------------

func mapFinishReason(s string, hasCalls bool) skyl.StopReason {
	if hasCalls {
		return skyl.StopToolUse
	}
	switch s {
	case "STOP":
		return skyl.StopEndTurn
	case "MAX_TOKENS":
		return skyl.StopMaxTokens
	case "SAFETY", "RECITATION", "BLOCKLIST", "PROHIBITED_CONTENT", "SPII":
		return skyl.StopRefusal
	default:
		return skyl.StopUnknown
	}
}

func partsFromCandidate(c wireContent) ([]skyl.Part, bool) {
	var (
		parts    []skyl.Part
		hasCalls bool
	)
	for _, wp := range c.Parts {
		switch {
		case wp.FunctionCall != nil:
			hasCalls = true
			args := wp.FunctionCall.Args
			if len(args) == 0 {
				args = json.RawMessage("{}")
			}
			parts = append(parts, skyl.ToolCall{
				ID:        wp.FunctionCall.Name, // Gemini has no call IDs.
				Name:      wp.FunctionCall.Name,
				Arguments: args,
			})
		case wp.Text != "":
			parts = append(parts, skyl.Text{Text: wp.Text})
		}
	}
	return parts, hasCalls
}

// Complete runs a request to completion.
func (p *Provider) Complete(ctx context.Context, req *skyl.Request) (*skyl.Response, error) {
	payload, err := p.buildPayload(req)
	if err != nil {
		return nil, err
	}

	endpoint := fmt.Sprintf("%s/models/%s:generateContent", p.baseURL, url.PathEscape(req.Model))
	httpResp, err := httpx.PostJSON(ctx, p.hc, endpoint, p.headers(), payload)
	if err != nil {
		return nil, p.transportError(err)
	}
	defer httpResp.Body.Close() //nolint:errcheck // response body close on read path

	if httpResp.StatusCode >= 300 {
		return nil, p.httpError(httpResp)
	}

	raw, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return nil, p.transportError(err)
	}

	var wire wireResponse
	if err := json.Unmarshal(raw, &wire); err != nil {
		return nil, skyl.NewError(providerName, httpResp.StatusCode, skyl.ErrServer,
			"malformed response body", raw)
	}
	if wire.Error != nil {
		return nil, skyl.NewError(providerName, httpResp.StatusCode, skyl.ErrServer,
			wire.Error.Message, raw)
	}
	if len(wire.Candidates) == 0 {
		// A prompt blocked before generation yields no candidates at all.
		msg := "response contained no candidates"
		kind := skyl.ErrServer
		if wire.PromptFeedback != nil && wire.PromptFeedback.BlockReason != "" {
			msg = "prompt blocked: " + wire.PromptFeedback.BlockReason
			kind = skyl.ErrRefusal
		}
		return nil, skyl.NewError(providerName, httpResp.StatusCode, kind, msg, raw)
	}

	cand := wire.Candidates[0]
	parts, hasCalls := partsFromCandidate(cand.Content)

	model := wire.ModelVersion
	if model == "" {
		model = req.Model
	}

	return &skyl.Response{
		ID:         wire.ResponseID,
		Provider:   providerName,
		Model:      model,
		Message:    skyl.Message{Role: skyl.RoleAssistant, Parts: parts},
		StopReason: mapFinishReason(cand.FinishReason, hasCalls),
		// cachedContentTokenCount is already part of promptTokenCount, which is
		// the inclusion rule skyl.Usage defines — so these map across directly.
		Usage: skyl.Usage{
			InputTokens:     wire.UsageMetadata.PromptTokenCount,
			OutputTokens:    wire.UsageMetadata.CandidatesTokenCount,
			CacheReadTokens: wire.UsageMetadata.CachedContentTokenCount,
		},
		Raw: raw,
	}, nil
}

// Models lists the models available to this API key.
func (p *Provider) Models(ctx context.Context) ([]skyl.ModelInfo, error) {
	httpResp, err := httpx.Get(ctx, p.hc, p.baseURL+"/models?pageSize=1000", p.headers())
	if err != nil {
		return nil, p.transportError(err)
	}
	defer httpResp.Body.Close() //nolint:errcheck // response body close on read path

	if httpResp.StatusCode >= 300 {
		return nil, p.httpError(httpResp)
	}

	body, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return nil, p.transportError(err)
	}

	var wire struct {
		Models []json.RawMessage `json:"models"`
	}
	if err := json.Unmarshal(body, &wire); err != nil {
		return nil, skyl.NewError(providerName, httpResp.StatusCode, skyl.ErrServer,
			"malformed models response", body)
	}

	out := make([]skyl.ModelInfo, 0, len(wire.Models))
	for _, entry := range wire.Models {
		var m struct {
			Name             string `json:"name"`
			DisplayName      string `json:"displayName"`
			InputTokenLimit  int    `json:"inputTokenLimit"`
			OutputTokenLimit int    `json:"outputTokenLimit"`
		}
		if err := json.Unmarshal(entry, &m); err != nil || m.Name == "" {
			continue
		}
		out = append(out, skyl.ModelInfo{
			// The API returns "models/gemini-3.6-flash"; callers need the
			// bare ID for Request.Model.
			ID:              strings.TrimPrefix(m.Name, "models/"),
			Provider:        providerName,
			DisplayName:     m.DisplayName,
			ContextWindow:   m.InputTokenLimit,
			MaxOutputTokens: m.OutputTokenLimit,
			Raw:             entry,
		})
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Errors
// ---------------------------------------------------------------------------

// transportError wraps a failure that never produced an HTTP response, keeping
// the cause reachable through errors.Is.
func (p *Provider) transportError(err error) error {
	return (&skyl.Error{Provider: providerName, Message: err.Error()}).WithCause(err)
}

func (p *Provider) httpError(resp *http.Response) error {
	body := httpx.ReadErrorBody(resp.Body)
	kind := skyl.ClassifyStatus(resp.StatusCode)

	msg := ""
	var wire wireResponse
	if err := json.Unmarshal(body, &wire); err == nil && wire.Error != nil {
		msg = wire.Error.Message
	}

	e := skyl.NewError(providerName, resp.StatusCode, kind, msg, body)
	e.RetryAfter = skyl.ParseRetryAfter(resp.Header.Get("Retry-After"))
	return e
}

// ---------------------------------------------------------------------------
// Streaming
// ---------------------------------------------------------------------------

// Stream runs a request, returning events as the model produces them.
func (p *Provider) Stream(ctx context.Context, req *skyl.Request) (skyl.Stream, error) {
	payload, err := p.buildPayload(req)
	if err != nil {
		return nil, err
	}

	endpoint := fmt.Sprintf("%s/models/%s:streamGenerateContent?alt=sse",
		p.baseURL, url.PathEscape(req.Model))

	headers := p.headers()
	headers["Accept"] = "text/event-stream"

	httpResp, err := httpx.PostJSON(ctx, p.hc, endpoint, headers, payload)
	if err != nil {
		return nil, p.transportError(err)
	}
	if httpResp.StatusCode >= 300 {
		defer httpResp.Body.Close() //nolint:errcheck // error path
		return nil, p.httpError(httpResp)
	}

	return &stream{
		body:   httpResp.Body,
		reader: sse.NewReader(httpResp.Body),
	}, nil
}

type stream struct {
	body   io.ReadCloser
	reader *sse.Reader

	ev  skyl.StreamEvent
	err error

	queued     []skyl.StreamEvent
	usage      skyl.Usage
	stopReason skyl.StopReason
	flushed    bool
	done       bool
	closed     bool

	// sawTerminal records that a candidate carried a finishReason. Gemini has
	// no [DONE] sentinel, so that is the only promise the response is whole —
	// without it, a connection dropped mid-generation reaches EOF looking
	// exactly like a completed stream.
	sawTerminal bool
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

	for s.reader.Next() {
		data := s.reader.Event().Data
		if len(data) == 0 {
			continue
		}

		var chunk wireResponse
		if err := json.Unmarshal(data, &chunk); err != nil {
			continue
		}
		if chunk.Error != nil {
			s.err = skyl.NewError(providerName, 0, skyl.ErrServer, chunk.Error.Message, data)
			return false
		}
		if chunk.UsageMetadata.PromptTokenCount > 0 || chunk.UsageMetadata.CandidatesTokenCount > 0 {
			s.usage = skyl.Usage{
				InputTokens:     chunk.UsageMetadata.PromptTokenCount,
				OutputTokens:    chunk.UsageMetadata.CandidatesTokenCount,
				CacheReadTokens: chunk.UsageMetadata.CachedContentTokenCount,
			}
		}
		if len(chunk.Candidates) == 0 {
			continue
		}

		cand := chunk.Candidates[0]
		parts, hasCalls := partsFromCandidate(cand.Content)
		if cand.FinishReason != "" {
			s.stopReason = mapFinishReason(cand.FinishReason, hasCalls)
			s.sawTerminal = true
		}

		for _, part := range parts {
			switch v := part.(type) {
			case skyl.Text:
				s.queued = append(s.queued, skyl.StreamEvent{
					Type: skyl.EventTextDelta, Text: v.Text, Raw: data,
				})
			case skyl.ToolCall:
				call := v
				s.queued = append(s.queued, skyl.StreamEvent{
					Type: skyl.EventToolCall, ToolCall: &call, Raw: data,
				})
			}
		}

		if len(s.queued) > 0 {
			s.ev = s.queued[0]
			s.queued = s.queued[1:]
			return true
		}
	}

	if err := s.reader.Err(); err != nil {
		s.err = (&skyl.Error{Provider: providerName, Message: err.Error()}).WithCause(err)
		return false
	}

	// A stream that ends without any finishReason was cut short; reporting
	// success would hand back a partial answer that looks complete.
	if !s.sawTerminal && !s.flushed {
		s.err = skyl.NewError(providerName, 0, skyl.ErrServer,
			"stream ended without a terminal event; the response is truncated", nil)
		return false
	}

	if !s.flushed {
		s.flushed = true
		reason := s.stopReason
		if reason == "" {
			reason = skyl.StopUnknown
		}
		usage := s.usage
		s.ev = skyl.StreamEvent{Type: skyl.EventDone, Usage: &usage, StopReason: reason}
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
	if err := s.body.Close(); err != nil {
		return fmt.Errorf("skyl: closing gemini stream: %w", err)
	}
	return nil
}

// Verify at compile time that Provider satisfies the interface.
var _ skyl.Provider = (*Provider)(nil)
