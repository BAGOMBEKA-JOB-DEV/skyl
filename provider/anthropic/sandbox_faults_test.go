//go:build sandbox

// Mid-stream failures for the Anthropic adapter, over a real socket.
//
// Worth having separately from the other three: this adapter does not parse
// SSE itself. The official SDK decodes the event stream and accumulates it into
// a message, so what is under test here is whether skyl notices that the SDK
// stopped early — a question the other adapters do not have.
package anthropic_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/BAGOMBEKA-JOB-DEV/skyl"
	"github.com/BAGOMBEKA-JOB-DEV/skyl/internal/sandbox"
	"github.com/BAGOMBEKA-JOB-DEV/skyl/internal/testutil"
)

func drainStream(t *testing.T, s skyl.Stream) (string, error) {
	t.Helper()
	defer s.Close() //nolint:errcheck // test cleanup

	var text strings.Builder
	for s.Next() {
		if ev := s.Event(); ev.Type == skyl.EventTextDelta {
			text.WriteString(ev.Text)
		}
	}
	return text.String(), s.Err()
}

func TestSandboxAnthropicStreamTruncationIsDetected(t *testing.T) {
	stream, err := skyl.New(sandboxProvider(t)).Stream(testutil.Context(t), &skyl.Request{
		Model:     sandbox.FaultTruncate,
		MaxTokens: 64,
		Messages:  []skyl.Message{skyl.UserText("hello")},
	})
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}

	text, err := drainStream(t, stream)
	if err == nil {
		t.Fatal("Err() = nil for a stream that was cut short")
	}
	if !errors.Is(err, skyl.ErrServer) {
		t.Errorf("Err() = %v, want it to classify as ErrServer", err)
	}
	if !strings.Contains(err.Error(), "truncated") {
		t.Errorf("Err() = %q, want it to say the response is truncated", err)
	}
	if text == "" {
		t.Error("no text delivered; the bytes read before the cut are still data")
	}
}

func TestSandboxAnthropicMidStreamErrorIsSurfaced(t *testing.T) {
	stream, err := skyl.New(sandboxProvider(t)).Stream(testutil.Context(t), &skyl.Request{
		Model:     sandbox.FaultMidStreamError,
		MaxTokens: 64,
		Messages:  []skyl.Message{skyl.UserText("hello")},
	})
	if err != nil {
		t.Fatalf("Stream() error = %v, want the failure to arrive mid-stream", err)
	}

	if _, err := drainStream(t, stream); err == nil {
		t.Fatal("Err() = nil; the provider reported a failure inside the stream")
	}
}
