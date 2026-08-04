//go:build sandbox

// Mid-stream failures, over a real socket.
//
//	go test -tags=sandbox ./provider/
//
// Every adapter's mid-stream error handling was dead code before this suite
// existed. The sandbox could inject an HTTP status, but only before the
// response began — once the SSE header is written the status is fixed, so a
// stream that dies in flight could not be simulated at all. That is an
// ordinary production event: a dropped load balancer, a proxy timeout, a
// provider erroring after the first token.
//
// Both cases below fire *after* the client has already received content, which
// is what makes them different from a handshake failure.
package provider_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/BAGOMBEKA-JOB-DEV/skyl"
	"github.com/BAGOMBEKA-JOB-DEV/skyl/internal/sandbox"
	"github.com/BAGOMBEKA-JOB-DEV/skyl/internal/testutil"
	"github.com/BAGOMBEKA-JOB-DEV/skyl/provider/openai"
)

// drain consumes a stream and reports the text seen and the error that stopped
// it.
func drain(t *testing.T, s skyl.Stream) (string, error) {
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

// A stream cut short must not look complete. The adapters detect this by
// tracking whether a terminal signal arrived; before that fix, a dropped
// connection produced a clean EventDone over a partial answer.
func TestSandboxStreamTruncationIsDetected(t *testing.T) {
	base := newSandbox(t)

	for name, p := range toolTargets(t, base) {
		t.Run(name, func(t *testing.T) {
			stream, err := skyl.New(p).Stream(testutil.Context(t), &skyl.Request{
				Model:     sandbox.FaultTruncate,
				MaxTokens: 64,
				Messages:  []skyl.Message{skyl.UserText("hello")},
			})
			if err != nil {
				t.Fatalf("Stream() error = %v", err)
			}

			text, err := drain(t, stream)
			if err == nil {
				t.Fatal("Err() = nil for a stream that was cut short")
			}
			if !errors.Is(err, skyl.ErrServer) {
				t.Errorf("Err() = %v, want it to classify as ErrServer", err)
			}
			if !strings.Contains(err.Error(), "truncated") {
				t.Errorf("Err() = %q, want it to say the response is truncated", err)
			}
			// The partial text is still delivered: a caller needs both the
			// fragment and the fact that it is one.
			if text == "" {
				t.Error("no text delivered; the bytes read before the cut are still data")
			}
		})
	}
}

// An error inside the stream body, after a 200 and a good frame. This is the
// leg every adapter carried and none exercised.
func TestSandboxMidStreamErrorIsSurfaced(t *testing.T) {
	base := newSandbox(t)

	for name, p := range toolTargets(t, base) {
		t.Run(name, func(t *testing.T) {
			stream, err := skyl.New(p).Stream(testutil.Context(t), &skyl.Request{
				Model:     sandbox.FaultMidStreamError,
				MaxTokens: 64,
				Messages:  []skyl.Message{skyl.UserText("hello")},
			})
			if err != nil {
				// The handshake succeeded, so an error here would mean the
				// adapter mistook an in-band failure for a transport one.
				t.Fatalf("Stream() error = %v, want the failure to arrive mid-stream", err)
			}

			_, err = drain(t, stream)
			if err == nil {
				t.Fatal("Err() = nil; the provider reported a failure inside the stream")
			}
			if !errors.Is(err, skyl.ErrServer) {
				t.Errorf("Err() = %v, want it to classify as ErrServer", err)
			}
		})
	}
}

// A mid-stream failure must not be retried. Once bytes have reached the
// caller, replaying the request would duplicate output they have already seen —
// which is why Client.Stream retries only the handshake.
func TestSandboxMidStreamFailureIsNotRetried(t *testing.T) {
	box, base := newCountingSandbox(t)
	p := openai.New(sandbox.DefaultAPIKey, openai.WithBaseURL(base+"/openai/v1"))
	before := box.Requests()

	stream, err := skyl.New(p).Stream(testutil.Context(t), &skyl.Request{
		Model:     sandbox.FaultMidStreamError,
		MaxTokens: 64,
		Messages:  []skyl.Message{skyl.UserText("hello")},
	})
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	if _, err := drain(t, stream); err == nil {
		t.Fatal("Err() = nil, want the mid-stream failure")
	}

	if got := box.Requests() - before; got != 1 {
		t.Errorf("served %d requests, want exactly 1 — a mid-stream failure must "+
			"not be retried, or the caller sees duplicated output", got)
	}
}
