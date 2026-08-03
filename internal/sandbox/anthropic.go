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
	if !known("anthropic", req.Model) {
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
	inTokens := countTokens(prompt)
	outTokens := countTokens(answer)

	if req.Stream {
		h.anthropicStream(w, req.Model, answer, inTokens, outTokens)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"id":            "msg_sandbox_1",
		"type":          "message",
		"role":          "assistant",
		"model":         req.Model,
		"content":       []any{map[string]any{"type": "text", "text": answer}},
		"stop_reason":   "end_turn",
		"stop_sequence": nil,
		"usage": map[string]any{
			"input_tokens":  inTokens,
			"output_tokens": outTokens,
		},
	})
}

// anthropicStream emits the documented event sequence. The SDK accumulates
// these into a message, so the order and the names both matter.
func (h *Handler) anthropicStream(w http.ResponseWriter, model, answer string, in, out int) {
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

	for _, piece := range chunk(answer) {
		send("content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": 0,
			"delta": map[string]any{"type": "text_delta", "text": piece},
		})
	}

	send("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})

	send("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil},
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
