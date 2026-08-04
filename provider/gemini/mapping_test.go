package gemini

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/BAGOMBEKA-JOB-DEV/skyl"
)

// Request-mapping coverage for blocks no test executed before.
//
// Gemini diverges from the other two in every field name — functionDeclarations
// rather than tools, toolConfig rather than tool_choice, generationConfig
// wrapping the sampling knobs — which is exactly why the mapping needs its own
// assertions rather than inheriting confidence from the OpenAI adapter.

func captureRequest(t *testing.T, req *skyl.Request) map[string]any {
	t.Helper()

	var captured map[string]any
	p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &captured); err != nil {
			t.Errorf("outbound body is not JSON: %v", err)
		}
		_, _ = io.WriteString(w,
			`{"candidates":[{"content":{"parts":[{"text":"ok"}]},"finishReason":"STOP"}]}`)
	})

	if _, err := p.Complete(context.Background(), req); err != nil {
		t.Fatalf("Complete() error = %v", err)
	}
	return captured
}

func generationConfig(t *testing.T, captured map[string]any) map[string]any {
	t.Helper()
	gen, ok := captured["generationConfig"].(map[string]any)
	if !ok {
		t.Fatalf("generationConfig = %v, want an object", captured["generationConfig"])
	}
	return gen
}

func TestSamplingParametersAreMapped(t *testing.T) {
	t.Parallel()

	temp, topP := 0.3, 0.9
	req := basicRequest()
	req.Temperature = &temp
	req.TopP = &topP
	req.Stop = []string{"END", "STOP"}

	gen := generationConfig(t, captureRequest(t, req))

	if gen["temperature"] != 0.3 {
		t.Errorf("temperature = %v, want 0.3", gen["temperature"])
	}
	// Gemini spells it topP, not top_p — a rename is exactly the kind of
	// mistake an untested mapping hides.
	if gen["topP"] != 0.9 {
		t.Errorf("topP = %v, want 0.9", gen["topP"])
	}
	stops, ok := gen["stopSequences"].([]any)
	if !ok || len(stops) != 2 || stops[0] != "END" {
		t.Errorf("stopSequences = %v, want [END STOP]", gen["stopSequences"])
	}
}

// Nil sampling parameters must be omitted rather than sent as zero: several
// current models reject an explicit temperature, and 0 is not "unset".
func TestSamplingParametersOmittedWhenUnset(t *testing.T) {
	t.Parallel()

	captured := captureRequest(t, basicRequest())
	gen, _ := captured["generationConfig"].(map[string]any)
	for _, field := range []string{"temperature", "topP", "stopSequences"} {
		if _, ok := gen[field]; ok {
			t.Errorf("%s was sent for a request that did not set it", field)
		}
	}
}

func TestToolsBecomeFunctionDeclarations(t *testing.T) {
	t.Parallel()

	req := basicRequest()
	req.Tools = []skyl.Tool{{
		Name:        "get_weather",
		Description: "Look up the weather.",
		Parameters: map[string]any{
			"type":       "object",
			"properties": map[string]any{"city": map[string]any{"type": "string"}},
		},
	}}

	captured := captureRequest(t, req)

	tools, ok := captured["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("tools = %v, want one entry", captured["tools"])
	}
	// Gemini nests declarations inside a tools array rather than listing them
	// directly, which is a shape no other provider uses.
	decls, ok := tools[0].(map[string]any)["functionDeclarations"].([]any)
	if !ok || len(decls) != 1 {
		t.Fatalf("functionDeclarations = %v, want one", tools[0])
	}
	d := decls[0].(map[string]any)
	if d["name"] != "get_weather" {
		t.Errorf("name = %v, want get_weather", d["name"])
	}
	if d["description"] != "Look up the weather." {
		t.Errorf("description = %v, want it carried", d["description"])
	}
	if _, ok := d["parameters"]; !ok {
		t.Error("parameters were dropped")
	}
}

func TestToolChoiceBecomesToolConfig(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		choice   skyl.ToolChoice
		wantMode string
		wantOnly string
	}{
		{"auto", skyl.ToolChoice{Mode: skyl.ToolChoiceAuto}, "AUTO", ""},
		{"none", skyl.ToolChoice{Mode: skyl.ToolChoiceNone}, "NONE", ""},
		{"required", skyl.ToolChoice{Mode: skyl.ToolChoiceRequired}, "ANY", ""},
		// Gemini has no "one specific tool" mode: it is ANY plus a one-element
		// allow list.
		{"specific", skyl.ToolChoice{Mode: skyl.ToolChoiceSpecific, Name: "ping"}, "ANY", "ping"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			req := basicRequest()
			req.Tools = []skyl.Tool{{Name: "ping"}}
			req.ToolChoice = &tc.choice

			cfg := captureRequest(t, req)["toolConfig"].(map[string]any)
			fcc := cfg["functionCallingConfig"].(map[string]any)

			if fcc["mode"] != tc.wantMode {
				t.Errorf("mode = %v, want %v", fcc["mode"], tc.wantMode)
			}
			if tc.wantOnly == "" {
				if _, ok := fcc["allowedFunctionNames"]; ok {
					t.Errorf("allowedFunctionNames = %v, want it absent", fcc["allowedFunctionNames"])
				}
				return
			}
			names, ok := fcc["allowedFunctionNames"].([]any)
			if !ok || len(names) != 1 || names[0] != tc.wantOnly {
				t.Errorf("allowedFunctionNames = %v, want [%s]", fcc["allowedFunctionNames"], tc.wantOnly)
			}
		})
	}
}

// Gemini takes images as inline base64 with a mimeType, under inlineData.
func TestInlineImageDataIsMapped(t *testing.T) {
	t.Parallel()

	req := basicRequest()
	req.Messages = []skyl.Message{{
		Role:  skyl.RoleUser,
		Parts: []skyl.Part{skyl.Image{MediaType: "image/png", Data: []byte{0x89, 0x50}}},
	}}

	contents := captureRequest(t, req)["contents"].([]any)
	parts := contents[0].(map[string]any)["parts"].([]any)
	inline, ok := parts[0].(map[string]any)["inlineData"].(map[string]any)
	if !ok {
		t.Fatalf("parts[0] = %v, want inlineData", parts[0])
	}
	if inline["mimeType"] != "image/png" {
		t.Errorf("mimeType = %v, want image/png", inline["mimeType"])
	}
	if inline["data"] != "iVA=" {
		t.Errorf("data = %v, want base64 of the raw bytes", inline["data"])
	}
}

// A tool call replayed in an assistant turn: Gemini calls that role "model"
// and carries the call as a functionCall part with decoded args.
func TestAssistantToolCallIsMapped(t *testing.T) {
	t.Parallel()

	req := basicRequest()
	req.Messages = []skyl.Message{
		skyl.UserText("weather?"),
		{Role: skyl.RoleAssistant, Parts: []skyl.Part{skyl.ToolCall{
			ID: "get_weather", Name: "get_weather",
			Arguments: json.RawMessage(`{"city":"Kampala"}`),
		}}},
	}

	contents := captureRequest(t, req)["contents"].([]any)
	turn := contents[1].(map[string]any)
	if turn["role"] != "model" {
		t.Errorf("role = %v, want model — Gemini has no assistant role", turn["role"])
	}

	parts := turn["parts"].([]any)
	fc, ok := parts[0].(map[string]any)["functionCall"].(map[string]any)
	if !ok {
		t.Fatalf("parts[0] = %v, want a functionCall", parts[0])
	}
	if fc["name"] != "get_weather" {
		t.Errorf("name = %v, want get_weather", fc["name"])
	}
	// Args are a decoded object under "args", not a JSON string.
	args, ok := fc["args"].(map[string]any)
	if !ok || args["city"] != "Kampala" {
		t.Errorf("args = %v (%T), want a decoded object", fc["args"], fc["args"])
	}
}

// Gemini correlates a result with its call by function name, because it issues
// no call IDs. The adapter sets ToolCall.ID to the name so this round-trips
// without the caller doing anything special.
func TestToolResultBecomesFunctionResponse(t *testing.T) {
	t.Parallel()

	req := basicRequest()
	req.Messages = []skyl.Message{
		skyl.UserText("weather?"),
		skyl.ToolResultMessage("get_weather", "22C and sunny"),
	}

	contents := captureRequest(t, req)["contents"].([]any)
	parts := contents[len(contents)-1].(map[string]any)["parts"].([]any)
	fr, ok := parts[0].(map[string]any)["functionResponse"].(map[string]any)
	if !ok {
		t.Fatalf("parts[0] = %v, want a functionResponse", parts[0])
	}
	if fr["name"] != "get_weather" {
		t.Errorf("name = %v, want the function name as the correlator", fr["name"])
	}
	resp, ok := fr["response"].(map[string]any)
	if !ok || resp["content"] != "22C and sunny" {
		t.Errorf("response = %v, want the tool output", fr["response"])
	}
}
