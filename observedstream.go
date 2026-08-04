package skyl

import (
	"context"
	"time"
)

// observedStream reports a stream's outcome to the hooks once it is finished
// with.
//
// [Client.Stream] returns as soon as the provider accepts the request, so the
// event it emits there describes a handshake and nothing more: no tokens exist
// yet, and the Client never touches the stream again. That left streaming usage
// — the cost signal for the mode most chat products actually use —
// unobservable through the only observability surface skyl has.
//
// This wraps the provider's stream and emits [OpStreamEnd] exactly once, when
// the stream ends. It holds no goroutine and adds no synchronisation: the
// [Stream] contract is single-consumer (see stream.go), so the wrapper inherits
// that and needs none.
//
// # Why it fires on Close as well as on the terminal event
//
// Abandoning a stream is not an error path, it is an ordinary one. A caller
// returns early, a client hangs up, a handler stops writing — the gateway does
// exactly this whenever an SSE client disconnects. Firing only on the terminal
// event would make every one of those silent, and those tokens were generated
// and billed regardless.
//
// Firing only on Close has the opposite problem: a completed stream that the
// caller also closes would report whatever usage happened to be known rather
// than the real total, and a zero would be indistinguishable from a genuine
// zero.
//
// So it fires on whichever comes first and latches. [HookEvent.Completed] is
// what tells the two apart, and it is the field a cost dashboard should check
// before trusting Usage.
type observedStream struct {
	inner  Stream
	client *Client

	// ctx is the context the stream was opened with. By the time the terminal
	// event fires it is often already cancelled — that is what a hang-up
	// looks like — so it is passed to hooks for values and cancellation
	// awareness, not as a live deadline.
	ctx context.Context //nolint:containedctx // see above; the hook contract documents it

	req      *Request
	provider string
	attempt  int
	started  time.Time

	// usage and stopReason are captured from the terminal event as it passes
	// through to the caller.
	usage      Usage
	stopReason StopReason

	completed bool
	fired     bool
}

// Next advances the underlying stream, watching for the terminal event.
func (s *observedStream) Next() bool {
	if !s.inner.Next() {
		return false
	}

	// EventDone is the stream saying it finished cleanly, and is the only
	// place usage arrives. Record it here rather than emitting immediately:
	// the caller may still call Next again, and the event should describe the
	// stream's whole life.
	if ev := s.inner.Event(); ev.Type == EventDone {
		s.completed = true
		if ev.Usage != nil {
			s.usage = *ev.Usage
		}
		s.stopReason = ev.StopReason
	}
	return true
}

func (s *observedStream) Event() StreamEvent { return s.inner.Event() }

func (s *observedStream) Err() error { return s.inner.Err() }

// Close releases the stream and emits the terminal hook event.
//
// It stays idempotent, as the [Stream] contract requires, and the hook fires at
// most once however many times Close is called.
func (s *observedStream) Close() error {
	err := s.inner.Close()
	s.emit()
	return err
}

// emit fires the terminal event, once.
func (s *observedStream) emit() {
	if s.fired {
		return
	}
	s.fired = true

	s.client.emit(s.ctx, HookEvent{
		Provider:   s.provider,
		Model:      s.req.Model,
		Operation:  OpStreamEnd,
		Attempt:    s.attempt,
		Duration:   time.Since(s.started),
		Err:        s.inner.Err(),
		Usage:      s.usage,
		StopReason: s.stopReason,
		Completed:  s.completed,
		Request:    s.req,
	})
}
