package sandbox

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Gemini generateContent. The shape differs from the other two in every way
// that matters — "contents" not "messages", "model" not "assistant",
// systemInstruction as its own field, camelCase throughout — which is exactly
// why it is worth serving rather than aliasing onto the OpenAI handler.

func (h *Handler) geminiAuth(w http.ResponseWriter, r *http.Request) bool {
	if r.Header.Get("x-goog-api-key") == h.apiKey {
		return true
	}
	geminiError(w, http.StatusUnauthorized, "UNAUTHENTICATED", "API key not valid.")
	return false
}

func geminiError(w http.ResponseWriter, status int, kind, msg string) {
	if status == http.StatusTooManyRequests {
		w.Header().Set("Retry-After", "1")
	}
	writeJSON(w, status, map[string]any{
		"error": map[string]any{"code": status, "message": msg, "status": kind},
	})
}

type geminiRequest struct {
	Contents []struct {
		Role  string `json:"role"`
		Parts []struct {
			Text string `json:"text"`
		} `json:"parts"`
	} `json:"contents"`

	SystemInstruction *struct {
		Parts []struct {
			Text string `json:"text"`
		} `json:"parts"`
	} `json:"systemInstruction"`
}

func (r geminiRequest) lastUserText() string {
	for i := len(r.Contents) - 1; i >= 0; i-- {
		c := r.Contents[i]
		// Gemini's user turn has role "user"; the model's is "model".
		if c.Role != "" && c.Role != "user" {
			continue
		}
		for _, p := range c.Parts {
			if p.Text != "" {
				return p.Text
			}
		}
	}
	return ""
}

// geminiGenerate handles both generateContent and streamGenerateContent, which
// arrive as a method suffix on the model path rather than as separate routes.
func (h *Handler) geminiGenerate(w http.ResponseWriter, r *http.Request) {
	if !h.geminiAuth(w, r) {
		return
	}

	// The path segment is "{model}:{method}", for example
	// "gemini-3.6-flash:streamGenerateContent".
	spec := r.PathValue("model")
	model, method, ok := strings.Cut(spec, ":")
	if !ok {
		geminiError(w, http.StatusBadRequest, "INVALID_ARGUMENT",
			"expected models/{model}:{method}, got "+spec)
		return
	}

	switch method {
	case "generateContent", "streamGenerateContent":
	default:
		geminiError(w, http.StatusNotFound, "NOT_FOUND", "unknown method "+method)
		return
	}

	var req geminiRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		geminiError(w, http.StatusBadRequest, "INVALID_ARGUMENT",
			"could not parse request body: "+err.Error())
		return
	}

	if status, ok := injectedStatus(model); ok {
		geminiError(w, status, "UNKNOWN", "sandbox: injected status "+model)
		return
	}
	if !known("gemini", model) {
		geminiError(w, http.StatusNotFound, "NOT_FOUND",
			fmt.Sprintf("models/%s is not found for API version v1beta", model))
		return
	}

	prompt := req.lastUserText()
	answer := reply(prompt)
	inTokens := countTokens(prompt)
	outTokens := countTokens(answer)

	if method == "streamGenerateContent" {
		h.geminiStream(w, model, answer, inTokens, outTokens)
		return
	}

	writeJSON(w, http.StatusOK, geminiPayload(model, answer, "STOP", inTokens, outTokens))
}

func geminiPayload(model, text, finish string, in, out int) map[string]any {
	candidate := map[string]any{
		"content": map[string]any{
			"role":  "model",
			"parts": []any{map[string]any{"text": text}},
		},
		"index": 0,
	}
	if finish != "" {
		candidate["finishReason"] = finish
	}

	return map[string]any{
		"candidates":   []any{candidate},
		"modelVersion": model,
		"responseId":   "sandbox-gemini-1",
		"usageMetadata": map[string]any{
			"promptTokenCount":     in,
			"candidatesTokenCount": out,
			"totalTokenCount":      in + out,
		},
	}
}

// geminiStream emits alt=sse frames: bare `data:` records, no event names, and
// no [DONE] sentinel — the stream simply ends.
func (h *Handler) geminiStream(w http.ResponseWriter, model, answer string, in, out int) {
	flush := beginSSE(w)

	pieces := chunk(answer)
	for i, piece := range pieces {
		// Only the final frame carries finishReason and usage, matching the
		// real API — an adapter that reads them from the first frame would
		// pass against a fake that repeated them everywhere.
		finish := ""
		payload := geminiPayload(model, piece, finish, 0, 0)
		if i == len(pieces)-1 {
			payload = geminiPayload(model, piece, "STOP", in, out)
		} else {
			delete(payload, "usageMetadata")
		}

		data, err := json.Marshal(payload)
		if err != nil {
			return
		}
		_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
		flush()
		time.Sleep(streamDelay)
	}
}

func (h *Handler) geminiModels(w http.ResponseWriter, r *http.Request) {
	if !h.geminiAuth(w, r) {
		return
	}

	models := make([]any, 0, len(knownModels["gemini"]))
	for _, m := range knownModels["gemini"] {
		models = append(models, map[string]any{
			// The API returns a qualified resource name; the adapter is
			// responsible for trimming the prefix, so serving it unqualified
			// would hide a bug.
			"name":                       "models/" + m.ID,
			"displayName":                m.Display,
			"inputTokenLimit":            m.In,
			"outputTokenLimit":           m.Out,
			"supportedGenerationMethods": []any{"generateContent", "streamGenerateContent"},
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{"models": models})
}
