// Package oai implements the OpenAI chat-completions wire format.
//
// It backs both provider/openai and provider/openaicompat. Most of the
// industry serves this shape, so implementing it once and configuring the base
// URL reaches OpenAI itself plus xAI, DeepSeek, Mistral, Groq, Together,
// Fireworks, OpenRouter, Ollama, vLLM, and the rest of the long tail.
package oai

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/BAGOMBEKA-JOB-DEV/skyl"
	"github.com/BAGOMBEKA-JOB-DEV/skyl/internal/httpx"
	"github.com/BAGOMBEKA-JOB-DEV/skyl/internal/sse"
)

// Config parameterises a chat-completions client.
type Config struct {
	// Name is the provider name reported in responses and errors.
	Name string

	// BaseURL is the API root, without a trailing slash, for example
	// "https://api.openai.com/v1".
	BaseURL string

	// APIKey is sent as a bearer token. Empty is valid — local runtimes such
	// as Ollama need no credential.
	APIKey string

	// HTTPClient overrides the default client.
	HTTPClient *http.Client

	// ExtraHeaders are added to every request. Some hosts require them;
	// OpenRouter, for instance, attributes traffic this way.
	ExtraHeaders map[string]string

	// MaxTokensField names the output-cap field. OpenAI's newer models
	// require "max_completion_tokens"; most compatible hosts still take
	// "max_tokens". Empty defaults to "max_tokens".
	MaxTokensField string
}

// Client speaks the OpenAI chat-completions API.
//
// It is safe for concurrent use.
type Client struct {
	cfg Config
	hc  *http.Client
}

// New returns a Client for cfg.
func New(cfg Config) *Client {
	hc := cfg.HTTPClient
	if hc == nil {
		hc = httpx.DefaultClient()
	}
	if cfg.MaxTokensField == "" {
		cfg.MaxTokensField = "max_tokens"
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	return &Client{cfg: cfg, hc: hc}
}

// Name returns the configured provider name.
func (c *Client) Name() string { return c.cfg.Name }

func (c *Client) headers() map[string]string {
	h := make(map[string]string, len(c.cfg.ExtraHeaders)+1)
	for k, v := range c.cfg.ExtraHeaders {
		h[k] = v
	}
	if c.cfg.APIKey != "" {
		h["Authorization"] = "Bearer " + c.cfg.APIKey
	}
	return h
}

// ---------------------------------------------------------------------------
// Wire types
// ---------------------------------------------------------------------------

type wireMessage struct {
	Role       string         `json:"role"`
	Content    any            `json:"content,omitempty"`
	ToolCalls  []wireToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
}

type wireToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Index    *int   `json:"index,omitempty"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type wireResponse struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		Message      wireMessage `json:"message"`
		Delta        wireMessage `json:"delta"`
		FinishReason string      `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens        int `json:"prompt_tokens"`
		CompletionTokens    int `json:"completion_tokens"`
		PromptTokensDetails struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    any    `json:"code"`
	} `json:"error"`
}

// ---------------------------------------------------------------------------
// Request mapping
// ---------------------------------------------------------------------------

func (c *Client) buildPayload(req *skyl.Request, stream bool) (map[string]any, error) {
	msgs := make([]wireMessage, 0, len(req.Messages)+1)
	if req.System != "" {
		msgs = append(msgs, wireMessage{Role: "system", Content: req.System})
	}

	for i, m := range req.Messages {
		converted, err := c.convertMessage(i, m)
		if err != nil {
			return nil, err
		}
		msgs = append(msgs, converted...)
	}

	payload := map[string]any{
		"model":    req.Model,
		"messages": msgs,
	}
	if stream {
		payload["stream"] = true
		// Ask for usage on the final chunk; hosts that don't know the option
		// ignore it, and we fall back to zero usage.
		payload["stream_options"] = map[string]any{"include_usage": true}
	}
	if req.MaxTokens > 0 {
		payload[c.cfg.MaxTokensField] = req.MaxTokens
	}
	if req.Temperature != nil {
		payload["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		payload["top_p"] = *req.TopP
	}
	if len(req.Stop) > 0 {
		payload["stop"] = req.Stop
	}
	if len(req.Tools) > 0 {
		tools := make([]map[string]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			params := t.Parameters
			if params == nil {
				params = map[string]any{"type": "object", "properties": map[string]any{}}
			}
			tools = append(tools, map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        t.Name,
					"description": t.Description,
					"parameters":  params,
				},
			})
		}
		payload["tools"] = tools
	}
	if tc := req.ToolChoice; tc != nil {
		switch tc.Mode {
		case skyl.ToolChoiceSpecific:
			payload["tool_choice"] = map[string]any{
				"type":     "function",
				"function": map[string]any{"name": tc.Name},
			}
		case skyl.ToolChoiceAuto, skyl.ToolChoiceNone, skyl.ToolChoiceRequired:
			payload["tool_choice"] = string(tc.Mode)
		}
	}
	if req.Thinking != nil && req.Thinking.Enabled && req.Thinking.Effort != "" {
		// Reasoning-capable hosts accept an effort hint; others ignore it.
		payload["reasoning_effort"] = string(req.Thinking.Effort)
	}

	return httpx.Merge(payload, req.ProviderOptions), nil
}

// convertMessage maps one skyl message onto one or more wire messages.
//
// A tool-role message expands to one wire message per result, because the
// OpenAI format carries a single tool_call_id per message.
func (c *Client) convertMessage(idx int, m skyl.Message) ([]wireMessage, error) {
	switch m.Role {
	case skyl.RoleTool:
		var out []wireMessage
		for _, p := range m.Parts {
			tr, ok := p.(skyl.ToolResult)
			if !ok {
				return nil, skyl.Unsupportedf(c.cfg.Name,
					"message %d: tool messages may only contain tool results, got %T", idx, p)
			}
			content := tr.Content
			if tr.IsError && content != "" {
				content = "error: " + content
			}
			out = append(out, wireMessage{
				Role:       "tool",
				ToolCallID: tr.CallID,
				Content:    content,
			})
		}
		return out, nil

	case skyl.RoleUser, skyl.RoleAssistant:
		var (
			blocks    []map[string]any
			textOnly  strings.Builder
			toolCalls []wireToolCall
			multipart bool
		)

		for _, p := range m.Parts {
			switch v := p.(type) {
			case skyl.Text:
				textOnly.WriteString(v.Text)
				blocks = append(blocks, map[string]any{"type": "text", "text": v.Text})

			case skyl.Image:
				if m.Role != skyl.RoleUser {
					return nil, skyl.Unsupportedf(c.cfg.Name,
						"message %d: images are only supported on user messages", idx)
				}
				url := v.URL
				if url == "" {
					url = "data:" + v.MediaType + ";base64," +
						base64.StdEncoding.EncodeToString(v.Data)
				}
				multipart = true
				blocks = append(blocks, map[string]any{
					"type":      "image_url",
					"image_url": map[string]any{"url": url},
				})

			case skyl.ToolCall:
				if m.Role != skyl.RoleAssistant {
					return nil, skyl.Unsupportedf(c.cfg.Name,
						"message %d: tool calls are only supported on assistant messages", idx)
				}
				tc := wireToolCall{ID: v.ID, Type: "function"}
				tc.Function.Name = v.Name
				tc.Function.Arguments = string(v.Arguments)
				if tc.Function.Arguments == "" {
					tc.Function.Arguments = "{}"
				}
				toolCalls = append(toolCalls, tc)

			default:
				return nil, skyl.Unsupportedf(c.cfg.Name,
					"message %d: cannot represent part of type %T", idx, p)
			}
		}

		wm := wireMessage{Role: string(m.Role), ToolCalls: toolCalls}
		switch {
		case multipart:
			wm.Content = blocks
		case textOnly.Len() > 0:
			wm.Content = textOnly.String()
		case len(toolCalls) > 0:
			// An assistant turn that is purely tool calls carries no content.
		default:
			wm.Content = ""
		}
		return []wireMessage{wm}, nil

	default:
		return nil, skyl.Unsupportedf(c.cfg.Name, "message %d: unknown role %q", idx, m.Role)
	}
}

// ---------------------------------------------------------------------------
// Response mapping
// ---------------------------------------------------------------------------

func mapFinishReason(s string) skyl.StopReason {
	switch s {
	case "stop", "end_turn":
		return skyl.StopEndTurn
	case "length", "max_tokens":
		return skyl.StopMaxTokens
	case "tool_calls", "function_call":
		return skyl.StopToolUse
	case "content_filter", "refusal":
		return skyl.StopRefusal
	case "":
		return skyl.StopUnknown
	default:
		return skyl.StopUnknown
	}
}

// textFromContent extracts assistant text from a chat-completions content
// field.
//
// The field is typed `any` because hosts disagree about its shape. OpenAI
// itself sends a bare string, but vLLM, some Azure deployments, and several
// OpenRouter upstreams send the same array-of-blocks form they accept on the
// request side. Reading only the string case meant those hosts produced a
// successful response with no text at all — the silent data loss docs/rules.md
// §6.1 exists to prevent.
func textFromContent(content any) string {
	switch v := content.(type) {
	case nil:
		return ""

	case string:
		return v

	case []any:
		var b strings.Builder
		for _, item := range v {
			block, ok := item.(map[string]any)
			if !ok {
				continue
			}
			// Blocks carrying anything other than text (images, audio) have no
			// text to contribute; skipping them is not a loss, because the
			// whole payload remains on Response.Raw.
			if t, ok := block["type"].(string); ok && t != "text" {
				continue
			}
			if s, ok := block["text"].(string); ok {
				b.WriteString(s)
			}
		}
		return b.String()

	default:
		return ""
	}
}

func toolCallFromWire(w wireToolCall) skyl.ToolCall {
	args := json.RawMessage(w.Function.Arguments)
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	return skyl.ToolCall{ID: w.ID, Name: w.Function.Name, Arguments: args}
}

// Complete implements a non-streaming chat completion.
func (c *Client) Complete(ctx context.Context, req *skyl.Request) (*skyl.Response, error) {
	payload, err := c.buildPayload(req, false)
	if err != nil {
		return nil, err
	}

	httpResp, err := httpx.PostJSON(ctx, c.hc, c.cfg.BaseURL+"/chat/completions", c.headers(), payload)
	if err != nil {
		return nil, c.transportError(err)
	}
	defer httpResp.Body.Close() //nolint:errcheck // response body close on read path

	if httpResp.StatusCode >= 300 {
		return nil, c.httpError(httpResp)
	}

	raw, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return nil, c.transportError(err)
	}

	var wire wireResponse
	if err := json.Unmarshal(raw, &wire); err != nil {
		return nil, skyl.NewError(c.cfg.Name, httpResp.StatusCode, skyl.ErrServer,
			"malformed response body", raw)
	}
	// Some hosts return 200 with an error object rather than a status code.
	if wire.Error != nil {
		return nil, skyl.NewError(c.cfg.Name, httpResp.StatusCode, skyl.ErrServer,
			wire.Error.Message, raw)
	}
	if len(wire.Choices) == 0 {
		return nil, skyl.NewError(c.cfg.Name, httpResp.StatusCode, skyl.ErrServer,
			"response contained no choices", raw)
	}

	choice := wire.Choices[0]
	var parts []skyl.Part
	if text := textFromContent(choice.Message.Content); text != "" {
		parts = append(parts, skyl.Text{Text: text})
	}
	for _, tc := range choice.Message.ToolCalls {
		parts = append(parts, toolCallFromWire(tc))
	}

	stopReason := mapFinishReason(choice.FinishReason)

	// A refusal that produced no content is a failure, not a success with an
	// empty string: a caller who only reads Text() would see the model return
	// nothing and have no idea why. A refusal that still produced text is
	// returned normally, with StopRefusal on the response.
	if stopReason == skyl.StopRefusal && len(parts) == 0 {
		return nil, skyl.NewError(c.cfg.Name, httpResp.StatusCode, skyl.ErrRefusal,
			"the model declined the request ("+choice.FinishReason+")", raw)
	}

	model := wire.Model
	if model == "" {
		model = req.Model
	}

	return &skyl.Response{
		ID:         wire.ID,
		Provider:   c.cfg.Name,
		Model:      model,
		Message:    skyl.Message{Role: skyl.RoleAssistant, Parts: parts},
		StopReason: stopReason,
		// cached_tokens is already part of prompt_tokens here, which is exactly
		// the inclusion rule skyl.Usage defines — so these map across directly.
		Usage: skyl.Usage{
			InputTokens:     wire.Usage.PromptTokens,
			OutputTokens:    wire.Usage.CompletionTokens,
			CacheReadTokens: wire.Usage.PromptTokensDetails.CachedTokens,
		},
		Raw: raw,
	}, nil
}

// Models lists the host's available models.
func (c *Client) Models(ctx context.Context) ([]skyl.ModelInfo, error) {
	httpResp, err := httpx.Get(ctx, c.hc, c.cfg.BaseURL+"/models", c.headers())
	if err != nil {
		return nil, c.transportError(err)
	}
	defer httpResp.Body.Close() //nolint:errcheck // response body close on read path

	if httpResp.StatusCode >= 300 {
		return nil, c.httpError(httpResp)
	}

	var wire struct {
		Data []json.RawMessage `json:"data"`
	}
	body, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return nil, c.transportError(err)
	}
	if err := json.Unmarshal(body, &wire); err != nil {
		return nil, skyl.NewError(c.cfg.Name, httpResp.StatusCode, skyl.ErrServer,
			"malformed models response", body)
	}

	out := make([]skyl.ModelInfo, 0, len(wire.Data))
	for _, entry := range wire.Data {
		var m struct {
			ID            string `json:"id"`
			Name          string `json:"name"`
			ContextLength int    `json:"context_length"`
		}
		if err := json.Unmarshal(entry, &m); err != nil || m.ID == "" {
			continue
		}
		out = append(out, skyl.ModelInfo{
			ID:            m.ID,
			Provider:      c.cfg.Name,
			DisplayName:   m.Name,
			ContextWindow: m.ContextLength,
			Raw:           entry,
		})
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Errors
// ---------------------------------------------------------------------------

// transportError wraps a failure that never produced an HTTP response.
//
// The cause is retained rather than flattened into the message, so a caller can
// still tell a deadline from a DNS failure with errors.Is.
func (c *Client) transportError(err error) error {
	return (&skyl.Error{Provider: c.cfg.Name, Message: err.Error()}).WithCause(err)
}

func (c *Client) httpError(resp *http.Response) error {
	body := httpx.ReadErrorBody(resp.Body)
	kind := skyl.ClassifyStatus(resp.StatusCode)

	msg := ""
	var wire wireResponse
	if err := json.Unmarshal(body, &wire); err == nil && wire.Error != nil {
		msg = wire.Error.Message
	}

	e := skyl.NewError(c.cfg.Name, resp.StatusCode, kind, msg, body)
	e.RetryAfter = skyl.ParseRetryAfter(resp.Header.Get("Retry-After"))
	return e
}

// ---------------------------------------------------------------------------
// Streaming
// ---------------------------------------------------------------------------

// Stream opens a streaming chat completion.
func (c *Client) Stream(ctx context.Context, req *skyl.Request) (skyl.Stream, error) {
	payload, err := c.buildPayload(req, true)
	if err != nil {
		return nil, err
	}

	headers := c.headers()
	headers["Accept"] = "text/event-stream"

	httpResp, err := httpx.PostJSON(ctx, c.hc, c.cfg.BaseURL+"/chat/completions", headers, payload)
	if err != nil {
		return nil, c.transportError(err)
	}
	if httpResp.StatusCode >= 300 {
		defer httpResp.Body.Close() //nolint:errcheck // error path
		return nil, c.httpError(httpResp)
	}

	return &stream{
		provider: c.cfg.Name,
		body:     httpResp.Body,
		reader:   sse.NewReader(httpResp.Body),
		pending:  make(map[int]*wireToolCall),
	}, nil
}

// stream decodes an SSE chat-completions response.
//
// Not safe for concurrent use, per the skyl.Stream contract; the mutex guards
// only Close, which callers legitimately invoke from a defer while the loop is
// unwinding.
type stream struct {
	provider string
	body     io.ReadCloser
	reader   *sse.Reader

	ev  skyl.StreamEvent
	err error

	// pending buffers tool-call fragments by index. Arguments arrive as
	// partial JSON across many events, so a call is only emitted once whole.
	pending map[int]*wireToolCall
	order   []int

	usage      skyl.Usage
	stopReason skyl.StopReason
	flushed    bool
	done       bool

	// sawTerminal records that the provider signalled the end of the response,
	// either with the [DONE] sentinel or a finish_reason. Without it, a
	// connection dropped mid-generation reaches EOF looking exactly like a
	// complete response, and the caller keeps a truncated answer believing it
	// is whole.
	sawTerminal bool

	mu     sync.Mutex
	closed bool
}

func (s *stream) Next() bool {
	if s.done || s.err != nil {
		return false
	}

	for s.reader.Next() {
		data := s.reader.Event().Data
		if len(data) == 0 {
			continue
		}
		if string(data) == "[DONE]" {
			s.sawTerminal = true
			return s.finish()
		}

		var chunk wireResponse
		if err := json.Unmarshal(data, &chunk); err != nil {
			// A frame we cannot parse is not fatal — providers occasionally
			// interleave keep-alives and vendor-specific records.
			continue
		}
		if chunk.Error != nil {
			s.err = skyl.NewError(s.provider, 0, skyl.ErrServer, chunk.Error.Message, data)
			return false
		}
		if chunk.Usage.PromptTokens > 0 || chunk.Usage.CompletionTokens > 0 {
			s.usage = skyl.Usage{
				InputTokens:     chunk.Usage.PromptTokens,
				OutputTokens:    chunk.Usage.CompletionTokens,
				CacheReadTokens: chunk.Usage.PromptTokensDetails.CachedTokens,
			}
		}
		if len(chunk.Choices) == 0 {
			continue
		}

		choice := chunk.Choices[0]
		if choice.FinishReason != "" {
			s.stopReason = mapFinishReason(choice.FinishReason)
			// Not every compatible host sends [DONE], but a finish_reason is
			// just as good a promise that the response is complete.
			s.sawTerminal = true
		}

		for _, tc := range choice.Delta.ToolCalls {
			s.accumulate(tc)
		}

		if text := textFromContent(choice.Delta.Content); text != "" {
			s.ev = skyl.StreamEvent{Type: skyl.EventTextDelta, Text: text, Raw: data}
			return true
		}
	}

	if err := s.reader.Err(); err != nil {
		s.err = (&skyl.Error{Provider: s.provider, Message: err.Error()}).WithCause(err)
		return false
	}

	// EOF with no terminal signal means the connection ended mid-generation —
	// a dropped proxy, a severed load balancer. The bytes read so far are a
	// partial answer, and reporting success would hand the caller a truncated
	// response indistinguishable from a complete one.
	if !s.sawTerminal && !s.flushed {
		s.err = skyl.NewError(s.provider, 0, skyl.ErrServer,
			"stream ended without a terminal event; the response is truncated", nil)
		return false
	}

	return s.finish()
}

// accumulate folds a tool-call delta into the pending buffer.
func (s *stream) accumulate(tc wireToolCall) {
	idx := 0
	if tc.Index != nil {
		idx = *tc.Index
	}
	cur, ok := s.pending[idx]
	if !ok {
		cur = &wireToolCall{}
		s.pending[idx] = cur
		s.order = append(s.order, idx)
	}
	if tc.ID != "" {
		cur.ID = tc.ID
	}
	if tc.Function.Name != "" {
		cur.Function.Name = tc.Function.Name
	}
	cur.Function.Arguments += tc.Function.Arguments
}

// finish drains buffered tool calls, then emits the terminal event.
func (s *stream) finish() bool {
	for len(s.order) > 0 {
		idx := s.order[0]
		s.order = s.order[1:]
		call := s.pending[idx]
		delete(s.pending, idx)
		if call == nil || call.Function.Name == "" {
			continue
		}
		mapped := toolCallFromWire(*call)
		s.ev = skyl.StreamEvent{Type: skyl.EventToolCall, ToolCall: &mapped}
		return true
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
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	if err := s.body.Close(); err != nil {
		return fmt.Errorf("skyl: closing %s stream: %w", s.provider, err)
	}
	return nil
}
