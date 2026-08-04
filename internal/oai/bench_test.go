package oai

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/BAGOMBEKA-JOB-DEV/skyl"
)

// What actually costs per request and per token.
//
// buildPayload plus json.Marshal is what a retry re-does from scratch, and the
// per-chunk unmarshal runs once per token per concurrent stream.

func benchRequest(messages int) *skyl.Request {
	req := &skyl.Request{
		Model:     "gpt-5.6",
		System:    "You are a helpful assistant.",
		MaxTokens: 1024,
		Messages:  make([]skyl.Message, 0, messages),
		Tools: []skyl.Tool{{
			Name:        "get_weather",
			Description: "Look up the weather.",
			Parameters: map[string]any{
				"type":       "object",
				"properties": map[string]any{"city": map[string]any{"type": "string"}},
			},
		}},
	}
	for i := 0; i < messages; i++ {
		if i%2 == 0 {
			req.Messages = append(req.Messages, skyl.UserText(strings.Repeat("word ", 40)))
		} else {
			req.Messages = append(req.Messages, skyl.AssistantText(strings.Repeat("reply ", 40)))
		}
	}
	return req
}

// A retry rebuilds the whole payload, so this is per attempt, not per request.
func BenchmarkBuildPayloadAndMarshal(b *testing.B) {
	c := New(Config{Name: "bench", BaseURL: "http://localhost"})

	for _, size := range []struct {
		name string
		n    int
	}{
		{"2turns", 2},
		{"20turns", 20},
		{"100turns", 100},
	} {
		req := benchRequest(size.n)
		b.Run(size.name, func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				payload, err := c.buildPayload(req, false)
				if err != nil {
					b.Fatal(err)
				}
				if _, err := json.Marshal(payload); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// Once per token, per stream. The full wireResponse is allocated even for a
// one-token frame.
func BenchmarkStreamChunkDecode(b *testing.B) {
	data := []byte(`{"id":"chatcmpl-1","object":"chat.completion.chunk","model":"gpt-5.6",` +
		`"choices":[{"index":0,"delta":{"content":"hello"}}]}`)

	b.ReportAllocs()
	b.SetBytes(int64(len(data)))
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		var chunk wireResponse
		if err := json.Unmarshal(data, &chunk); err != nil {
			b.Fatal(err)
		}
	}
}

// Tool arguments accumulate by string concatenation, which is quadratic in the
// number of fragments. This is the shape that gets expensive on a large tool
// call.
func BenchmarkAccumulateToolArguments(b *testing.B) {
	for _, frags := range []struct {
		name string
		n    int
	}{
		{"8frags", 8},
		{"64frags", 64},
		{"512frags", 512},
	} {
		b.Run(frags.name, func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				s := &stream{pending: make(map[int]*pendingCall)}
				idx := 0
				for j := 0; j < frags.n; j++ {
					var tc wireToolCall
					tc.Index = &idx
					tc.Function.Arguments = `{"key":"value"},`
					s.accumulate(tc)
				}
			}
		})
	}
}

// Response decoding on the non-streaming path, including retaining Raw.
func BenchmarkCompleteDecode(b *testing.B) {
	body := []byte(`{"id":"chatcmpl-1","model":"gpt-5.6","choices":[{"message":` +
		`{"role":"assistant","content":"` + strings.Repeat("word ", 200) + `"},` +
		`"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":200}}`)

	b.ReportAllocs()
	b.SetBytes(int64(len(body)))
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		var wire wireResponse
		if err := json.Unmarshal(body, &wire); err != nil {
			b.Fatal(err)
		}
		_ = textFromContent(wire.Choices[0].Message.Content)
	}
}
