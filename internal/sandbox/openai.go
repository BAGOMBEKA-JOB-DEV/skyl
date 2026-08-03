package sandbox

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// OpenAI chat-completions. This mount backs both provider/openai and
// provider/openaicompat, which is the same split the real world has: one
// protocol, many hosts.

// oaiAuth accepts a bearer token, and also accepts none at all.
//
// Credential-free access is not a shortcut here — it is the behaviour local
// runtimes such as Ollama, LM Studio, and llama.cpp actually have, and
// provider/openaicompat exists to reach them. Requiring a key would make the
// sandbox unable to represent them.
func (h *Handler) oaiAuth(w http.ResponseWriter, r *http.Request) bool {
	auth := r.Header.Get("Authorization")
	if auth == "" || auth == "Bearer "+h.apiKey {
		return true
	}
	oaiError(w, http.StatusUnauthorized, "invalid_request_error", "invalid_api_key",
		"Incorrect API key provided.")
	return false
}

func oaiError(w http.ResponseWriter, status int, kind, code, msg string) {
	if status == http.StatusTooManyRequests {
		w.Header().Set("Retry-After", "1")
	}
	writeJSON(w, status, map[string]any{
		"error": map[string]any{"message": msg, "type": kind, "code": code},
	})
}

type oaiRequest struct {
	Model    string `json:"model"`
	Stream   bool   `json:"stream"`
	Messages []struct {
		Role    string `json:"role"`
		Content any    `json:"content"`
	} `json:"messages"`

	// Both spellings are accepted. OpenAI's current models require
	// max_completion_tokens while compatible hosts overwhelmingly still take
	// max_tokens, and the two adapters default differently — so the sandbox
	// has to honour whichever arrives.
	MaxTokens           int `json:"max_tokens"`
	MaxCompletionTokens int `json:"max_completion_tokens"`
}

func (r oaiRequest) lastUserText() string {
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

// oaiCatalogue is the model list both OpenAI mounts serve. The compat mount
// deliberately shares it: a compatible host publishes whatever models it has,
// and the sandbox has exactly one set.
const oaiCatalogue = "openai"

func (h *Handler) oaiChat(w http.ResponseWriter, r *http.Request) {
	if !h.oaiAuth(w, r) {
		return
	}

	var req oaiRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		oaiError(w, http.StatusBadRequest, "invalid_request_error", "",
			"could not parse request body: "+err.Error())
		return
	}

	if status, ok := injectedStatus(req.Model); ok {
		oaiError(w, status, "api_error", "", "sandbox: injected status "+req.Model)
		return
	}
	if !known(oaiCatalogue, req.Model) {
		oaiError(w, http.StatusNotFound, "invalid_request_error", "model_not_found",
			fmt.Sprintf("The model `%s` does not exist.", req.Model))
		return
	}

	prompt := req.lastUserText()
	answer := reply(prompt)
	inTokens := countTokens(prompt)
	outTokens := countTokens(answer)

	if req.Stream {
		h.oaiStream(w, req.Model, answer, inTokens, outTokens)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"id":      "chatcmpl-sandbox-1",
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   req.Model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": answer},
			"finish_reason": "stop",
		}},
		"usage": map[string]any{
			"prompt_tokens":     inTokens,
			"completion_tokens": outTokens,
			"total_tokens":      inTokens + outTokens,
		},
	})
}

func (h *Handler) oaiStream(w http.ResponseWriter, model, answer string, in, out int) {
	flush := beginSSE(w)

	send := func(payload any) {
		data, err := json.Marshal(payload)
		if err != nil {
			return
		}
		_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
		flush()
		time.Sleep(streamDelay)
	}

	base := func(delta, finish any) map[string]any {
		choice := map[string]any{"index": 0, "delta": delta}
		if finish != nil {
			choice["finish_reason"] = finish
		}
		return map[string]any{
			"id":      "chatcmpl-sandbox-1",
			"object":  "chat.completion.chunk",
			"created": time.Now().Unix(),
			"model":   model,
			"choices": []any{choice},
		}
	}

	send(base(map[string]any{"role": "assistant", "content": ""}, nil))
	for _, piece := range chunk(answer) {
		send(base(map[string]any{"content": piece}, nil))
	}
	send(base(map[string]any{}, "stop"))

	// A final chunk carrying usage and no choices, which is what
	// stream_options.include_usage produces.
	send(map[string]any{
		"id":      "chatcmpl-sandbox-1",
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []any{},
		"usage": map[string]any{
			"prompt_tokens":     in,
			"completion_tokens": out,
			"total_tokens":      in + out,
		},
	})

	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	flush()
}

func (h *Handler) oaiModels(w http.ResponseWriter, r *http.Request) {
	if !h.oaiAuth(w, r) {
		return
	}

	entries := knownModels[oaiCatalogue]
	data := make([]any, 0, len(entries))
	for _, m := range entries {
		data = append(data, map[string]any{
			"id":             m.ID,
			"object":         "model",
			"created":        1735689600,
			"owned_by":       "skyl-sandbox",
			"name":           m.Display,
			"context_length": m.In,
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}
