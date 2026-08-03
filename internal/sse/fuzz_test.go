package sse

import (
	"strings"
	"testing"
	"time"
)

// FuzzReader checks that no input can make the reader panic, hang, or invent
// data.
//
// The reader parses bytes straight off a network socket, so its input is
// attacker-influenceable in exactly the way fuzzing is designed to explore: a
// compromised or merely buggy provider, or anything on the path, can send
// frames no documentation describes.
func FuzzReader(f *testing.F) {
	seeds := []string{
		"",
		"\n\n",
		"data: hello\n\n",
		"event: message\ndata: hi\n\n",
		"data: line1\ndata: line2\n\n",
		": comment\n\n",
		"data: [DONE]\n\n",
		"data\n\n",
		"data:\n\n",
		"\r\n\r\n",
		"data: a\r\ndata: b\r\n\r\n",
		"id: 1\nretry: 50\ndata: x\n\n",
		"data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n",
		strings.Repeat("data: x\n\n", 100),
		"event:\ndata:\n\n",
		":\n:\n:\n\n",
		"data: \xff\xfe\x00\n\n",
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, input string) {
		done := make(chan struct{})

		go func() {
			defer close(done)

			r := NewReader(strings.NewReader(input))
			total := 0
			events := 0

			for r.Next() {
				ev := r.Event()

				// The reader must never produce more payload than it consumed.
				// Anything else means it is fabricating bytes.
				total += len(ev.Data) + len(ev.Name)
				if total > len(input)+1 {
					t.Errorf("reader emitted %d bytes from %d bytes of input", total, len(input))
					return
				}

				// A blank line separates records, so the number of records is
				// bounded by the input length. An unbounded count means Next
				// is not making progress.
				events++
				if events > len(input)+2 {
					t.Errorf("reader emitted %d events from %d bytes of input", events, len(input))
					return
				}
			}

			// After exhaustion Next must stay false rather than loop forever.
			for range 3 {
				if r.Next() {
					t.Error("Next() returned true after the stream was exhausted")
					return
				}
			}
		}()

		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("reader hung; a stream parser must always terminate")
		}
	})
}
