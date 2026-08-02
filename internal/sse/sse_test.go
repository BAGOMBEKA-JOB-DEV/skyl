package sse

import (
	"errors"
	"io"
	"strings"
	"testing"
)

func collect(t *testing.T, input string) ([]Event, error) {
	t.Helper()
	r := NewReader(strings.NewReader(input))
	var got []Event
	for r.Next() {
		ev := r.Event()
		// Copy the payload: Event.Data aliases the reader's buffer.
		got = append(got, Event{Name: ev.Name, Data: append([]byte(nil), ev.Data...)})
	}
	return got, r.Err()
}

func TestReaderDecodesRecords(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		want  []Event
	}{
		{
			name:  "single data record",
			input: "data: hello\n\n",
			want:  []Event{{Data: []byte("hello")}},
		},
		{
			name:  "named event",
			input: "event: message\ndata: hi\n\n",
			want:  []Event{{Name: "message", Data: []byte("hi")}},
		},
		{
			name:  "two records",
			input: "data: one\n\ndata: two\n\n",
			want:  []Event{{Data: []byte("one")}, {Data: []byte("two")}},
		},
		{
			name:  "multi-line data is newline joined",
			input: "data: line1\ndata: line2\n\n",
			want:  []Event{{Data: []byte("line1\nline2")}},
		},
		{
			name:  "comments are skipped",
			input: ": keep-alive\ndata: payload\n\n",
			want:  []Event{{Data: []byte("payload")}},
		},
		{
			name:  "comment-only record yields nothing",
			input: ": ping\n\ndata: real\n\n",
			want:  []Event{{Data: []byte("real")}},
		},
		{
			name:  "CRLF line endings",
			input: "data: hello\r\n\r\n",
			want:  []Event{{Data: []byte("hello")}},
		},
		{
			name:  "final record without trailing blank line",
			input: "data: last\n",
			want:  []Event{{Data: []byte("last")}},
		},
		{
			name:  "id and retry fields are ignored",
			input: "id: 7\nretry: 100\ndata: payload\n\n",
			want:  []Event{{Data: []byte("payload")}},
		},
		{
			name:  "only one leading space is stripped",
			input: "data:  two-spaces\n\n",
			want:  []Event{{Data: []byte(" two-spaces")}},
		},
		{
			name:  "field with no colon",
			input: "data\n\n",
			want:  []Event{{Data: []byte("")}},
		},
		{
			name:  "empty stream",
			input: "",
			want:  nil,
		},
		{
			name:  "blank lines only",
			input: "\n\n\n",
			want:  nil,
		},
		{
			name:  "DONE sentinel is passed through verbatim",
			input: "data: [DONE]\n\n",
			want:  []Event{{Data: []byte("[DONE]")}},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := collect(t, tc.input)
			if err != nil {
				t.Fatalf("Err() = %v, want nil", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("decoded %d events, want %d: %+v", len(got), len(tc.want), got)
			}
			for i := range got {
				if got[i].Name != tc.want[i].Name {
					t.Errorf("event %d name = %q, want %q", i, got[i].Name, tc.want[i].Name)
				}
				if string(got[i].Data) != string(tc.want[i].Data) {
					t.Errorf("event %d data = %q, want %q", i, got[i].Data, tc.want[i].Data)
				}
			}
		})
	}
}

func TestReaderStopsAfterExhaustion(t *testing.T) {
	t.Parallel()

	r := NewReader(strings.NewReader("data: one\n\n"))
	if !r.Next() {
		t.Fatal("first Next() = false, want true")
	}
	if r.Next() {
		t.Error("second Next() = true, want false at end of stream")
	}
	// Repeated calls after exhaustion must stay false rather than loop.
	if r.Next() {
		t.Error("Next() after exhaustion = true, want false")
	}
	if err := r.Err(); err != nil {
		t.Errorf("Err() = %v, want nil for a cleanly ended stream", err)
	}
}

// errReader fails partway through, standing in for a severed connection.
type errReader struct {
	data []byte
	i    int
	err  error
}

func (e *errReader) Read(p []byte) (int, error) {
	if e.i >= len(e.data) {
		return 0, e.err
	}
	n := copy(p, e.data[e.i:])
	e.i += n
	return n, nil
}

func TestReaderSurfacesReadError(t *testing.T) {
	t.Parallel()

	want := errors.New("connection reset")
	r := NewReader(&errReader{data: []byte("data: partial\n\ndata: more"), err: want})

	for r.Next() {
	}
	if err := r.Err(); !errors.Is(err, want) {
		t.Errorf("Err() = %v, want %v", err, want)
	}
}

func TestReaderRejectsOversizedEvent(t *testing.T) {
	t.Parallel()

	// A single line past the buffer cap must fail rather than grow without
	// bound — an unbounded stream would be a memory-exhaustion vector.
	huge := "data: " + strings.Repeat("x", maxEventBytes+1024) + "\n\n"
	r := NewReader(strings.NewReader(huge))

	for r.Next() {
	}
	if r.Err() == nil {
		t.Error("Err() = nil, want an error for an oversized event")
	}
}

func TestReaderHandlesEmptyReader(t *testing.T) {
	t.Parallel()

	r := NewReader(strings.NewReader(""))
	if r.Next() {
		t.Error("Next() on an empty stream = true, want false")
	}
	if err := r.Err(); err != nil && !errors.Is(err, io.EOF) {
		t.Errorf("Err() = %v, want nil or EOF", err)
	}
}
