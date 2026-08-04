package sse

import (
	"strings"
	"testing"
)

// The SSE reader runs once per streaming frame — i.e. once per token, per
// concurrent stream. It is the hottest path in the library, and nothing
// measured it before.

func BenchmarkReaderNext(b *testing.B) {
	// A realistic OpenAI delta frame.
	const frame = "data: " +
		`{"id":"chatcmpl-1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"hello"}}]}` +
		"\n\n"
	stream := strings.Repeat(frame, 256)

	b.ReportAllocs()
	b.SetBytes(int64(len(stream)))
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		r := NewReader(strings.NewReader(stream))
		for r.Next() {
			_ = r.Event()
		}
	}
}

// Multi-line data records are joined per the spec, which is the append-heavy
// path.
func BenchmarkReaderMultilineData(b *testing.B) {
	frame := "event: message\ndata: " + strings.Repeat("line\ndata: ", 8) + "end\n\n"
	stream := strings.Repeat(frame, 128)

	b.ReportAllocs()
	b.SetBytes(int64(len(stream)))
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		r := NewReader(strings.NewReader(stream))
		for r.Next() {
			_ = r.Event()
		}
	}
}
