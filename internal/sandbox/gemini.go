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
			Text             string `json:"text"`
			FunctionResponse *struct {
				Name     string         `json:"name"`
				Response map[string]any `json:"response"`
			} `json:"functionResponse"`
		} `json:"parts"`
	} `json:"contents"`

	SystemInstruction *struct {
		Parts []struct {
			Text string `json:"text"`
		} `json:"parts"`
	} `json:"systemInstruction"`

	Tools []struct {
		FunctionDeclarations []struct {
			Name        string         `json:"name"`
			Description string         `json:"description"`
			Parameters  map[string]any `json:"parameters"`
		} `json:"functionDeclarations"`
	} `json:"tools"`

	ToolConfig *struct {
		FunctionCallingConfig *struct {
			Mode                 string   `json:"mode"`
			AllowedFunctionNames []string `json:"allowedFunctionNames"`
		} `json:"functionCallingConfig"`
	} `json:"toolConfig"`

	// Gemini nests sampling parameters under generationConfig rather than
	// putting them at the top level, which is the shape ProviderOptions has to
	// merge into.
	GenerationConfig *struct {
		MaxOutputTokens int `json:"maxOutputTokens"`
	} `json:"generationConfig"`
}

// maxTokens reports the request's output cap, or 0 when it set none.
func (r geminiRequest) maxTokens() int {
	if r.GenerationConfig == nil {
		return 0
	}
	return r.GenerationConfig.MaxOutputTokens
}

func (r geminiRequest) toolNames() []string {
	var names []string
	for _, t := range r.Tools {
		for _, d := range t.FunctionDeclarations {
			if d.Name != "" {
				names = append(names, d.Name)
			}
		}
	}
	return names
}

// toolChoice maps Gemini's functionCallingConfig onto the shared vocabulary.
// Gemini spells "required" as ANY, and expresses "one specific tool" as ANY
// plus a one-element allow list rather than as its own mode.
func (r geminiRequest) toolChoice() (mode, forced string) {
	if r.ToolConfig == nil || r.ToolConfig.FunctionCallingConfig == nil {
		return choiceAuto, ""
	}
	cfg := r.ToolConfig.FunctionCallingConfig
	switch cfg.Mode {
	case "NONE":
		return choiceNone, ""
	case "ANY":
		if len(cfg.AllowedFunctionNames) == 1 {
			return choiceRequired, cfg.AllowedFunctionNames[0]
		}
		return choiceRequired, ""
	default:
		return choiceAuto, ""
	}
}

// lastToolResult finds a functionResponse part. Gemini has no tool role and no
// call IDs — it correlates purely by function name, which is why the adapter
// sets ToolCall.ID to the name.
func (r geminiRequest) lastToolResult() (string, bool) {
	for i := len(r.Contents) - 1; i >= 0; i-- {
		for _, p := range r.Contents[i].Parts {
			if p.FunctionResponse == nil {
				continue
			}
			if p.FunctionResponse.Name == "" {
				return "", false
			}
			if s, ok := p.FunctionResponse.Response["content"].(string); ok {
				return s, true
			}
		}
	}
	return "", false
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
	fault, faulty := streamFault(model)
	if !faulty && !known("gemini", model) {
		geminiError(w, http.StatusNotFound, "NOT_FOUND",
			fmt.Sprintf("models/%s is not found for API version v1beta", model))
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

	// maxOutputTokens bounds prose only; a functionCall is emitted whole.
	var truncated bool
	if call == nil {
		answer, truncated = truncate(answer, req.maxTokens())
	}

	inTokens := countTokens(prompt)
	outTokens := countTokens(answer)

	if method == "streamGenerateContent" {
		h.geminiStream(w, model, answer, call, fault, inTokens, outTokens, truncated)
		return
	}

	parts := []any{map[string]any{"text": answer}}
	if call != nil {
		var args map[string]any
		// Gemini sends arguments as a decoded object under "args", not as a
		// JSON string and not under "arguments".
		_ = json.Unmarshal([]byte(call.Args), &args)
		parts = []any{map[string]any{
			"functionCall": map[string]any{"name": call.Name, "args": args},
		}}
		outTokens = countTokens(call.Args)
	}

	finish := "STOP"
	if truncated {
		finish = "MAX_TOKENS"
	}
	writeJSON(w, http.StatusOK, geminiPayload(model, parts, finish, inTokens, outTokens))
}

// geminiPayload builds a response around whatever parts the turn produced.
//
// It takes parts rather than a bare string because a Gemini turn is not always
// text: a functionCall is a part like any other, and a turn can carry both.
func geminiPayload(model string, parts []any, finish string, in, out int) map[string]any {
	candidate := map[string]any{
		"content": map[string]any{
			"role":  "model",
			"parts": parts,
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
func (h *Handler) geminiStream(w http.ResponseWriter, model, answer string, call *toolCall, fault string, in, out int, truncated bool) {
	flush := beginSSE(w)

	send := func(payload map[string]any) {
		data, err := json.Marshal(payload)
		if err != nil {
			return
		}
		_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
		flush()
		time.Sleep(streamDelay)
	}

	text := func(s string) []any { return []any{map[string]any{"text": s}} }

	// Gemini has no [DONE] sentinel, so a finishReason is the only signal that
	// the response is whole — which is exactly what makes truncation here worth
	// simulating.
	switch fault {
	case FaultTruncate:
		p := geminiPayload(model, text("par"), "", 0, 0)
		delete(p, "usageMetadata")
		send(p)
		return
	case FaultMidStreamError:
		p := geminiPayload(model, text("par"), "", 0, 0)
		delete(p, "usageMetadata")
		send(p)
		send(map[string]any{"error": map[string]any{
			"code": 503, "status": "UNAVAILABLE",
			"message": "sandbox: injected mid-stream failure",
		}})
		return
	}

	if call != nil {
		var args map[string]any
		_ = json.Unmarshal([]byte(call.Args), &args)
		// One frame carrying *two* parts: a preamble and the call. Real models
		// do this, and it is the only way to exercise an adapter that has to
		// queue more than one event from a single frame.
		parts := []any{
			map[string]any{"text": "Checking. "},
			map[string]any{"functionCall": map[string]any{"name": call.Name, "args": args}},
		}
		send(geminiPayload(model, parts, "STOP", in, countTokens(call.Args)))
		return
	}

	pieces := chunk(answer)
	for i, piece := range pieces {
		// Only the final frame carries finishReason and usage, matching the
		// real API — an adapter that reads them from the first frame would
		// pass against a fake that repeated them everywhere.
		payload := geminiPayload(model, text(piece), "", 0, 0)
		if i == len(pieces)-1 {
			finish := "STOP"
			if truncated {
				finish = "MAX_TOKENS"
			}
			payload = geminiPayload(model, text(piece), finish, in, out)
		} else {
			delete(payload, "usageMetadata")
		}
		send(payload)
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
