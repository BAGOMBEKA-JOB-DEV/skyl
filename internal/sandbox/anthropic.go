package sandbox

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// Anthropic Messages API. Field names and the streaming event sequence follow
// the published protocol; the official SDK parses these responses, so a
// mistake here shows up as a decode failure rather than passing silently.

func (h *Handler) anthropicAuth(w http.ResponseWriter, r *http.Request) bool {
	if r.Header.Get("x-api-key") == h.apiKey {
		return true
	}
	writeJSON(w, http.StatusUnauthorized, map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    "authentication_error",
			"message": "invalid x-api-key",
		},
	})
	return false
}

func anthropicError(w http.ResponseWriter, status int, kind, msg string) {
	if status == http.StatusTooManyRequests {
		w.Header().Set("Retry-After", "1")
	}
	writeJSON(w, status, map[string]any{
		"type":  "error",
		"error": map[string]any{"type": kind, "message": msg},
	})
}

type anthropicRequest struct {
	Model     string `json:"model"`
	MaxTokens int    `json:"max_tokens"`
	Stream    bool   `json:"stream"`
	System    []struct {
		Text string `json:"text"`
	} `json:"system"`
	Messages []struct {
		Role    string `json:"role"`
		Content any    `json:"content"`
	} `json:"messages"`

	Tools []struct {
		Name        string         `json:"name"`
		Description string         `json:"description"`
		InputSchema map[string]any `json:"input_schema"`
	} `json:"tools"`

	ToolChoice *struct {
		Type string `json:"type"`
		Name string `json:"name"`
	} `json:"tool_choice"`
}

func (r anthropicRequest) toolNames() []string {
	names := make([]string, 0, len(r.Tools))
	for _, t := range r.Tools {
		if t.Name != "" {
			names = append(names, t.Name)
		}
	}
	return names
}

// toolChoice maps Anthropic's vocabulary onto the shared one. "any" is what
// every other provider spells "required".
func (r anthropicRequest) toolChoice() (mode, forced string) {
	if r.ToolChoice == nil {
		return choiceAuto, ""
	}
	switch r.ToolChoice.Type {
	case "none":
		return choiceNone, ""
	case "any":
		return choiceRequired, ""
	case "tool":
		return choiceRequired, r.ToolChoice.Name
	default:
		return choiceAuto, ""
	}
}

// lastToolResult finds a tool_result block. Anthropic has no tool role: results
// come back as blocks inside an ordinary user turn.
func (r anthropicRequest) lastToolResult() (string, bool) {
	for i := len(r.Messages) - 1; i >= 0; i-- {
		m := r.Messages[i]
		if m.Role != "user" {
			continue
		}
		blocks, ok := m.Content.([]any)
		if !ok {
			continue
		}
		for _, raw := range blocks {
			block, ok := raw.(map[string]any)
			if !ok || block["type"] != "tool_result" {
				continue
			}
			// The real API rejects a tool_result whose tool_use_id does not
			// match a tool_use block it issued, so an adapter that drops the
			// correlation must not appear to work here either.
			if id, _ := block["tool_use_id"].(string); id != toolCallID {
				return "", false
			}
			// tool_result content is either a bare string or an array of
			// content blocks, and the API accepts both. The official SDK sends
			// the array form — NewToolResultBlock wraps the string in a text
			// block — so a sandbox that understood only the string would reject
			// what its own adapter sends.
			switch c := block["content"].(type) {
			case string:
				return c, true
			case []any:
				for _, raw := range c {
					inner, ok := raw.(map[string]any)
					if !ok || inner["type"] != "text" {
						continue
					}
					if s, ok := inner["text"].(string); ok {
						return s, true
					}
				}
			}
		}
	}
	return "", false
}

// lastUserText pulls the newest user text out of the request, tolerating both
// content shapes the API accepts: a bare string, or a list of blocks.
func (r anthropicRequest) lastUserText() string {
	for i := len(r.Messages) - 1; i >= 0; i-- {
		m := r.Messages[i]
		if m.Role != "user" {
			continue
		}
		switch c := m.Content.(type) {
		case string:
			return c
		case []any:
			for _, raw := range c {
				block, ok := raw.(map[string]any)
				if !ok {
					continue
				}
				if block["type"] == "text" {
					if s, ok := block["text"].(string); ok {
						return s
					}
				}
			}
		}
	}
	return ""
}

func (h *Handler) anthropicMessages(w http.ResponseWriter, r *http.Request) {
	if !h.anthropicAuth(w, r) {
		return
	}

	var req anthropicRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		anthropicError(w, http.StatusBadRequest, "invalid_request_error",
			"could not parse request body: "+err.Error())
		return
	}

	if status, ok := injectedStatus(req.Model); ok {
		anthropicError(w, status, "api_error", "sandbox: injected status "+req.Model)
		return
	}
	fault, faulty := streamFault(req.Model)
	if !faulty && !known("anthropic", req.Model) {
		anthropicError(w, http.StatusNotFound, "not_found_error",
			fmt.Sprintf("model: %s", req.Model))
		return
	}
	// The real API rejects a request without max_tokens; the adapter supplies a
	// default, and enforcing it here is what proves that default is being sent.
	if req.MaxTokens <= 0 {
		anthropicError(w, http.StatusBadRequest, "invalid_request_error",
			"max_tokens: field required")
		return
	}

	prompt := req.lastUserText()
	answer := reply(prompt)

	// A tool result already present means this is the second turn: answer with
	// it rather than asking for the tool again.
	var call *toolCall
	if result, ok := req.lastToolResult(); ok {
		answer = replyToToolResult(result)
	} else {
		mode, forced := req.toolChoice()
		call = decideToolCall(prompt, req.toolNames(), mode, forced)
	}

	// max_tokens bounds prose only; a tool_use block is emitted whole.
	var truncated bool
	if call == nil {
		answer, truncated = truncate(answer, req.MaxTokens)
	}

	inTokens := countTokens(prompt)
	outTokens := countTokens(answer)

	if req.Stream {
		h.anthropicStream(w, req.Model, answer, call, fault, inTokens, outTokens, truncated)
		return
	}

	content := []any{map[string]any{"type": "text", "text": answer}}
	stopReason := "end_turn"
	if truncated {
		stopReason = "max_tokens"
	}
	if call != nil {
		var input map[string]any
		// Unlike OpenAI, Anthropic sends tool input as a decoded object rather
		// than a JSON string. An adapter that confuses the two fails here.
		_ = json.Unmarshal([]byte(call.Args), &input)
		content = []any{map[string]any{
			"type":  "tool_use",
			"id":    call.ID,
			"name":  call.Name,
			"input": input,
		}}
		stopReason = "tool_use"
		outTokens = countTokens(call.Args)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"id":            "msg_sandbox_1",
		"type":          "message",
		"role":          "assistant",
		"model":         req.Model,
		"content":       content,
		"stop_reason":   stopReason,
		"stop_sequence": nil,
		"usage": map[string]any{
			"input_tokens":  inTokens,
			"output_tokens": outTokens,
		},
	})
}

// anthropicStream emits the documented event sequence. The SDK accumulates
// these into a message, so the order and the names both matter.
func (h *Handler) anthropicStream(w http.ResponseWriter, model, answer string, call *toolCall, fault string, in, out int, truncated bool) {
	flush := beginSSE(w)

	send := func(event string, payload any) {
		data, err := json.Marshal(payload)
		if err != nil {
			return
		}
		_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
		flush()
		time.Sleep(streamDelay)
	}

	send("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":            "msg_sandbox_1",
			"type":          "message",
			"role":          "assistant",
			"model":         model,
			"content":       []any{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage":         map[string]any{"input_tokens": in, "output_tokens": 0},
		},
	})

	send("content_block_start", map[string]any{
		"type":          "content_block_start",
		"index":         0,
		"content_block": map[string]any{"type": "text", "text": ""},
	})

	// Faults fire after the block has opened and one delta has landed, so the
	// SDK's accumulator is mid-message when the stream dies.
	switch fault {
	case FaultTruncate:
		send("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": 0,
			"delta": map[string]any{"type": "text_delta", "text": "par"},
		})
		return
	case FaultMidStreamError:
		send("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": 0,
			"delta": map[string]any{"type": "text_delta", "text": "par"},
		})
		send("error", map[string]any{
			"type": "error",
			"error": map[string]any{
				"type":    "overloaded_error",
				"message": "sandbox: injected mid-stream failure",
			},
		})
		return
	}

	for _, piece := range chunk(answer) {
		send("content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": 0,
			"delta": map[string]any{"type": "text_delta", "text": piece},
		})
	}

	send("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})

	stopReason := "end_turn"
	if truncated {
		stopReason = "max_tokens"
	}
	if call != nil {
		// A tool call is a *second* content block, at index 1. The index was
		// hardcoded to 0 throughout this file before, which would have let an
		// adapter that ignores the index entirely pass.
		stopReason = "tool_use"
		send("content_block_start", map[string]any{
			"type":  "content_block_start",
			"index": 1,
			"content_block": map[string]any{
				"type":  "tool_use",
				"id":    call.ID,
				"name":  call.Name,
				"input": map[string]any{},
			},
		})
		for _, frag := range argChunks(call.Args) {
			send("content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": 1,
				"delta": map[string]any{"type": "input_json_delta", "partial_json": frag},
			})
		}
		send("content_block_stop", map[string]any{"type": "content_block_stop", "index": 1})
		out = countTokens(call.Args)
	}

	send("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": stopReason, "stop_sequence": nil},
		"usage": map[string]any{"output_tokens": out},
	})

	send("message_stop", map[string]any{"type": "message_stop"})
}

func (h *Handler) anthropicModels(w http.ResponseWriter, r *http.Request) {
	if !h.anthropicAuth(w, r) {
		return
	}

	data := make([]any, 0, len(knownModels["anthropic"]))
	for _, m := range knownModels["anthropic"] {
		data = append(data, map[string]any{
			"id":               m.ID,
			"type":             "model",
			"display_name":     m.Display,
			"created_at":       "2026-01-01T00:00:00Z",
			"max_input_tokens": m.In,
			"max_tokens":       m.Out,
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{"data": data, "has_more": false})
}
