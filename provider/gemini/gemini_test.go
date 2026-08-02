package gemini

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/BAGOMBEKA-JOB-DEV/skyl"
)

func newTestProvider(t *testing.T, h http.HandlerFunc) *Provider {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return New("test-key", WithBaseURL(srv.URL))
}

func basicRequest() *skyl.Request {
	return &skyl.Request{
		Model:     "gemini-3.6-flash",
		MaxTokens: 100,
		Messages:  []skyl.Message{skyl.UserText("hello")},
	}
}

func TestCompleteSuccess(t *testing.T) {
	t.Parallel()

	var captured map[string]any
	p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &captured)

		// The key must ride in a header, not the URL, so it stays out of
		// proxy and server access logs.
		if got := r.Header.Get("x-goog-api-key"); got != "test-key" {
			t.Errorf("x-goog-api-key = %q, want test-key", got)
		}
		if strings.Contains(r.URL.RawQuery, "test-key") {
			t.Error("the API key leaked into the query string")
		}
		if !strings.Contains(r.URL.Path, "gemini-3.6-flash:generateContent") {
			t.Errorf("path = %q, want the model and method", r.URL.Path)
		}

		_, _ = io.WriteString(w, `{
			"candidates": [{"content": {"role": "model", "parts": [{"text": "hi there"}]},
			                "finishReason": "STOP"}],
			"usageMetadata": {"promptTokenCount": 7, "candidatesTokenCount": 4,
			                  "cachedContentTokenCount": 2},
			"modelVersion": "gemini-3.6-flash-001",
			"responseId": "resp-1"
		}`)
	})

	resp, err := p.Complete(context.Background(), basicRequest())
	if err != nil {
		t.Fatalf("Complete() error = %v", err)
	}

	if resp.Text() != "hi there" {
		t.Errorf("Text() = %q, want %q", resp.Text(), "hi there")
	}
	if resp.Model != "gemini-3.6-flash-001" {
		t.Errorf("Model = %q, want the version from the response", resp.Model)
	}
	if resp.Provider != "gemini" {
		t.Errorf("Provider = %q, want gemini", resp.Provider)
	}
	if resp.StopReason != skyl.StopEndTurn {
		t.Errorf("StopReason = %q, want %q", resp.StopReason, skyl.StopEndTurn)
	}
	if resp.Usage.InputTokens != 7 || resp.Usage.OutputTokens != 4 || resp.Usage.CacheReadTokens != 2 {
		t.Errorf("Usage = %+v, want 7/4/2", resp.Usage)
	}
	if len(resp.Raw) == 0 {
		t.Error("Raw must always be populated")
	}

	// Gemini calls the assistant role "model" and nests under contents/parts.
	contents := captured["contents"].([]any)
	first := contents[0].(map[string]any)
	if first["role"] != "user" {
		t.Errorf("role = %v, want user", first["role"])
	}
}

func TestSystemPromptBecomesSystemInstruction(t *testing.T) {
	t.Parallel()

	var captured map[string]any
	p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &captured)
		_, _ = io.WriteString(w, `{"candidates":[{"content":{"parts":[{"text":"ok"}]},"finishReason":"STOP"}]}`)
	})

	req := basicRequest()
	req.System = "be terse"
	if _, err := p.Complete(context.Background(), req); err != nil {
		t.Fatalf("Complete() error = %v", err)
	}

	si, ok := captured["systemInstruction"].(map[string]any)
	if !ok {
		t.Fatalf("systemInstruction = %v, want the system prompt hoisted out of messages",
			captured["systemInstruction"])
	}
	parts := si["parts"].([]any)
	if parts[0].(map[string]any)["text"] != "be terse" {
		t.Errorf("systemInstruction = %v, want %q", si, "be terse")
	}
}

func TestAssistantRoleIsMappedToModel(t *testing.T) {
	t.Parallel()

	var captured map[string]any
	p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &captured)
		_, _ = io.WriteString(w, `{"candidates":[{"content":{"parts":[{"text":"ok"}]},"finishReason":"STOP"}]}`)
	})

	req := basicRequest()
	req.Messages = append(req.Messages, skyl.AssistantText("prior turn"))
	if _, err := p.Complete(context.Background(), req); err != nil {
		t.Fatalf("Complete() error = %v", err)
	}

	contents := captured["contents"].([]any)
	second := contents[1].(map[string]any)
	if second["role"] != "model" {
		t.Errorf("assistant role = %v, want %q", second["role"], "model")
	}
}

func TestToolCallRoundTrip(t *testing.T) {
	t.Parallel()

	p := newTestProvider(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"candidates":[{"content":{"role":"model","parts":[
			{"functionCall":{"name":"get_weather","args":{"city":"Kampala"}}}
		]},"finishReason":"STOP"}]}`)
	})

	resp, err := p.Complete(context.Background(), basicRequest())
	if err != nil {
		t.Fatalf("Complete() error = %v", err)
	}
	if resp.StopReason != skyl.StopToolUse {
		t.Errorf("StopReason = %q, want %q — a function call implies tool use", resp.StopReason, skyl.StopToolUse)
	}

	calls := resp.ToolCalls()
	if len(calls) != 1 {
		t.Fatalf("got %d calls, want 1", len(calls))
	}
	// Gemini has no call IDs, so the adapter uses the function name; that is
	// what makes replaying a ToolResult work without caller effort.
	if calls[0].ID != "get_weather" || calls[0].Name != "get_weather" {
		t.Errorf("call = %+v, want ID and Name both get_weather", calls[0])
	}
}

func TestToolResultIsSentAsFunctionResponse(t *testing.T) {
	t.Parallel()

	var captured map[string]any
	p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &captured)
		_, _ = io.WriteString(w, `{"candidates":[{"content":{"parts":[{"text":"ok"}]},"finishReason":"STOP"}]}`)
	})

	req := basicRequest()
	req.Messages = append(req.Messages, skyl.ToolResultMessage("get_weather", "22C"))
	if _, err := p.Complete(context.Background(), req); err != nil {
		t.Fatalf("Complete() error = %v", err)
	}

	contents := captured["contents"].([]any)
	last := contents[len(contents)-1].(map[string]any)
	parts := last["parts"].([]any)
	fr, ok := parts[0].(map[string]any)["functionResponse"].(map[string]any)
	if !ok {
		t.Fatalf("part = %v, want a functionResponse", parts[0])
	}
	if fr["name"] != "get_weather" {
		t.Errorf("functionResponse name = %v, want get_weather", fr["name"])
	}
}

func TestImageURLIsRejectedNotDropped(t *testing.T) {
	t.Parallel()

	p := newTestProvider(t, func(http.ResponseWriter, *http.Request) {
		t.Error("provider was called; an unrepresentable request must fail locally")
	})

	req := basicRequest()
	req.Messages = []skyl.Message{{
		Role:  skyl.RoleUser,
		Parts: []skyl.Part{skyl.Image{URL: "https://example.com/a.png"}},
	}}

	_, err := p.Complete(context.Background(), req)
	if !errors.Is(err, skyl.ErrUnsupported) {
		t.Errorf("err = %v, want ErrUnsupported — Gemini needs inline data, and dropping the image silently would look like the model ignoring the question", err)
	}
}

func TestBlockedPromptIsARefusal(t *testing.T) {
	t.Parallel()

	p := newTestProvider(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"candidates":[],"promptFeedback":{"blockReason":"SAFETY"}}`)
	})

	_, err := p.Complete(context.Background(), basicRequest())
	if !errors.Is(err, skyl.ErrRefusal) {
		t.Errorf("err = %v, want ErrRefusal", err)
	}
	if !strings.Contains(err.Error(), "SAFETY") {
		t.Errorf("err = %q, want it to name the block reason", err)
	}
}

func TestFinishReasonMapping(t *testing.T) {
	t.Parallel()

	tests := []struct {
		reason string
		want   skyl.StopReason
	}{
		{"STOP", skyl.StopEndTurn},
		{"MAX_TOKENS", skyl.StopMaxTokens},
		{"SAFETY", skyl.StopRefusal},
		{"RECITATION", skyl.StopRefusal},
		{"PROHIBITED_CONTENT", skyl.StopRefusal},
		{"SOMETHING_NEW", skyl.StopUnknown},
	}

	for _, tc := range tests {
		t.Run(tc.reason, func(t *testing.T) {
			t.Parallel()
			if got := mapFinishReason(tc.reason, false); got != tc.want {
				t.Errorf("mapFinishReason(%q) = %q, want %q", tc.reason, got, tc.want)
			}
		})
	}

	// A function call always means tool use, whatever the reason string says.
	if got := mapFinishReason("STOP", true); got != skyl.StopToolUse {
		t.Errorf("mapFinishReason with calls = %q, want %q", got, skyl.StopToolUse)
	}
}

func TestErrorClassification(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		status int
		want   error
	}{
		{"unauthorized", 401, skyl.ErrAuth},
		{"not found", 404, skyl.ErrNotFound},
		{"rate limited", 429, skyl.ErrRateLimit},
		{"server error", 500, skyl.ErrServer},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := newTestProvider(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, `{"error":{"message":"nope"}}`)
			})

			_, err := p.Complete(context.Background(), basicRequest())
			if !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
			if strings.Contains(err.Error(), "test-key") {
				t.Error("the API key leaked into the error")
			}
		})
	}
}

func TestModels(t *testing.T) {
	t.Parallel()

	p := newTestProvider(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"models":[
			{"name":"models/gemini-3.6-flash","displayName":"Gemini 3.6 Flash",
			 "inputTokenLimit":1048576,"outputTokenLimit":65536},
			{"name":"models/gemini-3-pro"}
		]}`)
	})

	models, err := p.Models(context.Background())
	if err != nil {
		t.Fatalf("Models() error = %v", err)
	}
	if len(models) != 2 {
		t.Fatalf("got %d models, want 2", len(models))
	}
	// The API returns "models/x"; callers need the bare ID for Request.Model.
	if models[0].ID != "gemini-3.6-flash" {
		t.Errorf("ID = %q, want the models/ prefix stripped", models[0].ID)
	}
	if models[0].ContextWindow != 1048576 || models[0].MaxOutputTokens != 65536 {
		t.Errorf("limits = %d/%d, want 1048576/65536", models[0].ContextWindow, models[0].MaxOutputTokens)
	}
}

func TestStream(t *testing.T) {
	t.Parallel()

	p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, ":streamGenerateContent") {
			t.Errorf("path = %q, want the streaming method", r.URL.Path)
		}
		if r.URL.Query().Get("alt") != "sse" {
			t.Errorf("alt = %q, want sse", r.URL.Query().Get("alt"))
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, frame := range []string{
			`{"candidates":[{"content":{"parts":[{"text":"Hello"}]}}]}`,
			`{"candidates":[{"content":{"parts":[{"text":", world"}]}}]}`,
			`{"candidates":[{"content":{"parts":[]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":5}}`,
		} {
			_, _ = io.WriteString(w, "data: "+frame+"\n\n")
			if fl, ok := w.(http.Flusher); ok {
				fl.Flush()
			}
		}
	})

	s, err := p.Stream(context.Background(), basicRequest())
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	defer s.Close() //nolint:errcheck // test cleanup

	var (
		b    strings.Builder
		last skyl.StreamEvent
	)
	for s.Next() {
		ev := s.Event()
		last = ev
		if ev.Type == skyl.EventTextDelta {
			b.WriteString(ev.Text)
		}
	}
	if err := s.Err(); err != nil {
		t.Fatalf("Err() = %v", err)
	}
	if b.String() != "Hello, world" {
		t.Errorf("text = %q, want %q", b.String(), "Hello, world")
	}
	if last.Type != skyl.EventDone {
		t.Errorf("final event = %q, want %q", last.Type, skyl.EventDone)
	}
	if last.Usage == nil || last.Usage.OutputTokens != 5 {
		t.Errorf("Usage = %+v, want output 5", last.Usage)
	}
}

func TestStreamCloseIsIdempotent(t *testing.T) {
	t.Parallel()

	p := newTestProvider(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"a\"}]}}]}\n\n")
	})

	s, err := p.Stream(context.Background(), basicRequest())
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	if err := s.Close(); err != nil {
		t.Errorf("first Close() = %v", err)
	}
	if err := s.Close(); err != nil {
		t.Errorf("second Close() = %v, want nil — Close must be idempotent", err)
	}
}

func TestProviderSatisfiesInterface(t *testing.T) {
	t.Parallel()

	var p skyl.Provider = New("k")
	if p.Name() != "gemini" {
		t.Errorf("Name() = %q, want gemini", p.Name())
	}
}
