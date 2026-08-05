package skyl

import (
	"context"
	"errors"
	"testing"

	"github.com/BAGOMBEKA-JOB-DEV/skyl/internal/testutil"
)

// recordHooks collects every event a client emits.
func recordHooks(events *[]HookEvent) Option {
	return WithHook(func(_ context.Context, ev HookEvent) {
		*events = append(*events, ev)
	})
}

// eventsOfType filters by operation.
func eventsOfType(events []HookEvent, op string) []HookEvent {
	var out []HookEvent
	for _, ev := range events {
		if ev.Operation == op {
			out = append(out, ev)
		}
	}
	return out
}

// A drained stream reports its usage. This is the whole reason the terminal
// event exists: streaming is the dominant mode, and before this the only
// stream event fired at the handshake, before a single token existed.
func TestStreamEndReportsUsageOnDrain(t *testing.T) {
	t.Parallel()

	var events []HookEvent
	p := &fakeProvider{
		stream: &scriptedStream{
			events: []StreamEvent{
				{Type: EventTextDelta, Text: "hi"},
				{
					Type:       EventDone,
					Usage:      &Usage{InputTokens: 11, OutputTokens: 7},
					StopReason: StopEndTurn,
				},
			},
		},
	}

	s, err := New(p, recordHooks(&events)).Stream(context.Background(), validRequest())
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	for s.Next() { //nolint:revive // draining is the point
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close() = %v", err)
	}

	ends := eventsOfType(events, OpStreamEnd)
	if len(ends) != 1 {
		t.Fatalf("got %d %s events, want exactly 1", len(ends), OpStreamEnd)
	}
	end := ends[0]

	if !end.Completed {
		t.Error("Completed = false for a fully drained stream")
	}
	if end.Usage.InputTokens != 11 || end.Usage.OutputTokens != 7 {
		t.Errorf("Usage = %+v, want 11/7 from the terminal event", end.Usage)
	}
	if end.StopReason != StopEndTurn {
		t.Errorf("StopReason = %q, want %q", end.StopReason, StopEndTurn)
	}
	if end.Request == nil {
		t.Error("Request is nil; a hook cannot report sampling parameters without it")
	}
	// The handshake event is still emitted, and still carries no usage — the
	// two events answer different questions.
	handshakes := eventsOfType(events, OpStream)
	if len(handshakes) != 1 {
		t.Fatalf("got %d %s events, want 1", len(handshakes), OpStream)
	}
	if handshakes[0].Usage != (Usage{}) {
		t.Errorf("handshake Usage = %+v, want zero — no token exists yet", handshakes[0].Usage)
	}
}

// A drained stream that is never closed still reports.
//
// The [Stream] contract does not oblige a caller to close a stream it has run
// to completion, so `for s.Next() {}` with no Close is a legal — and common —
// shape. Emitting only from Close made that shape silent: the usage was known,
// recorded, and then thrown away. Every other test in this file happens to call
// Close, which is precisely why the gap survived.
func TestStreamEndFiresWithoutCloseOnDrainedStream(t *testing.T) {
	t.Parallel()

	var events []HookEvent
	p := &fakeProvider{
		stream: &scriptedStream{
			events: []StreamEvent{
				{Type: EventTextDelta, Text: "hi"},
				{
					Type:       EventDone,
					Usage:      &Usage{InputTokens: 11, OutputTokens: 7},
					StopReason: StopEndTurn,
				},
			},
		},
	}

	s, err := New(p, recordHooks(&events)).Stream(context.Background(), validRequest())
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	for s.Next() { //nolint:revive // draining without Close is the point
	}
	// Deliberately no Close.

	ends := eventsOfType(events, OpStreamEnd)
	if len(ends) != 1 {
		t.Fatalf("got %d %s events, want exactly 1 — a drained stream must report even unclosed", len(ends), OpStreamEnd)
	}
	if !ends[0].Completed {
		t.Error("Completed = false for a fully drained stream")
	}
	if ends[0].Usage.InputTokens != 11 || ends[0].Usage.OutputTokens != 7 {
		t.Errorf("Usage = %+v, want 11/7 — the usage was known and must not be dropped", ends[0].Usage)
	}

	// And closing afterwards must not double-report.
	if err := s.Close(); err != nil {
		t.Fatalf("Close() = %v", err)
	}
	if got := len(eventsOfType(events, OpStreamEnd)); got != 1 {
		t.Errorf("got %d %s events after a late Close, want 1 — emit must latch", got, OpStreamEnd)
	}
}

// The case the design exists for. A caller that returns early — or a client
// that hangs up, which is what the gateway sees every day — still consumed
// tokens upstream. Reporting nothing would make that spend invisible.
func TestStreamEndFiresOnAbandonedStream(t *testing.T) {
	t.Parallel()

	var events []HookEvent
	p := &fakeProvider{
		stream: &scriptedStream{
			events: []StreamEvent{
				{Type: EventTextDelta, Text: "par"},
				{Type: EventTextDelta, Text: "tial"},
				{Type: EventDone, Usage: &Usage{InputTokens: 5, OutputTokens: 99}},
			},
		},
	}

	s, err := New(p, recordHooks(&events)).Stream(context.Background(), validRequest())
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	// Read one event, then walk away.
	s.Next()
	if err := s.Close(); err != nil {
		t.Fatalf("Close() = %v", err)
	}

	ends := eventsOfType(events, OpStreamEnd)
	if len(ends) != 1 {
		t.Fatalf("got %d %s events, want exactly 1 for an abandoned stream",
			len(ends), OpStreamEnd)
	}
	if ends[0].Completed {
		t.Error("Completed = true for a stream that was closed early")
	}
	// Usage is whatever arrived, which is nothing — and Completed is what says
	// so. A dashboard that trusts this zero without checking Completed is
	// reading a number that was never reported.
	if ends[0].Usage != (Usage{}) {
		t.Errorf("Usage = %+v, want zero — the terminal event never arrived", ends[0].Usage)
	}
}

// Close is contractually idempotent, and callers legitimately call it from a
// defer and again on an error path. The hook must not double-fire.
func TestStreamEndFiresOnlyOnce(t *testing.T) {
	t.Parallel()

	var events []HookEvent
	p := &fakeProvider{
		stream: &scriptedStream{
			events: []StreamEvent{{Type: EventDone, Usage: &Usage{OutputTokens: 3}}},
		},
	}

	s, err := New(p, recordHooks(&events)).Stream(context.Background(), validRequest())
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	for s.Next() { //nolint:revive // draining is the point
	}
	for range 3 {
		if err := s.Close(); err != nil {
			t.Errorf("Close() = %v, want nil on every call", err)
		}
	}

	if got := len(eventsOfType(events, OpStreamEnd)); got != 1 {
		t.Errorf("got %d %s events after three Closes, want 1", got, OpStreamEnd)
	}
}

// A stream that failed mid-flight carries its error into the terminal event,
// so a hook can record the failure without the caller reporting it separately.
func TestStreamEndCarriesTheStreamError(t *testing.T) {
	t.Parallel()

	boom := errors.New("connection reset")
	var events []HookEvent
	p := &fakeProvider{
		stream: &scriptedStream{
			events: []StreamEvent{{Type: EventTextDelta, Text: "par"}},
			err:    boom,
		},
	}

	s, err := New(p, recordHooks(&events)).Stream(context.Background(), validRequest())
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	for s.Next() { //nolint:revive // draining is the point
	}
	_ = s.Close()

	ends := eventsOfType(events, OpStreamEnd)
	if len(ends) != 1 {
		t.Fatalf("got %d %s events, want 1", len(ends), OpStreamEnd)
	}
	if !errors.Is(ends[0].Err, boom) {
		t.Errorf("Err = %v, want the stream's own error", ends[0].Err)
	}
	if ends[0].Completed {
		t.Error("Completed = true for a stream that failed")
	}
}

// With no hooks registered there is nothing to report, so the provider's
// stream is returned unwrapped — the observation costs nothing when unused.
func TestStreamIsNotWrappedWithoutHooks(t *testing.T) {
	t.Parallel()

	inner := &scriptedStream{events: []StreamEvent{{Type: EventDone}}}
	p := &fakeProvider{stream: inner}

	s, err := New(p).Stream(context.Background(), validRequest())
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	defer s.Close() //nolint:errcheck // test cleanup

	if s != Stream(inner) {
		t.Errorf("Stream() returned %T, want the provider's own stream unwrapped", s)
	}
}

// The wrapper must hold no goroutine. It exists between the caller and a live
// network read, so a leak here is one per streaming request.
func TestObservedStreamLeaksNoGoroutine(t *testing.T) {
	// Not parallel: the leak check counts process-wide goroutines.
	client := New(
		&fakeProvider{stream: &scriptedStream{events: []StreamEvent{{Type: EventDone}}}},
		recordHooks(&[]HookEvent{}),
	)

	// Warm up first, so one-time allocations are in the baseline.
	warm, err := client.Stream(context.Background(), validRequest())
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	for warm.Next() { //nolint:revive // draining is the point
	}
	_ = warm.Close()

	defer testutil.CheckNoGoroutineLeaks(t)()

	for range 5 {
		s, err := client.Stream(context.Background(), validRequest())
		if err != nil {
			t.Fatalf("Stream() error = %v", err)
		}
		s.Next()
		_ = s.Close()
	}
}

// CollectStream closes what it drains, so the terminal event still fires
// exactly once through it.
func TestStreamEndFiresThroughCollectStream(t *testing.T) {
	t.Parallel()

	var events []HookEvent
	p := &fakeProvider{
		stream: &scriptedStream{
			events: []StreamEvent{
				{Type: EventTextDelta, Text: "Paris"},
				{Type: EventDone, Usage: &Usage{InputTokens: 4, OutputTokens: 1}, StopReason: StopEndTurn},
			},
		},
	}

	s, err := New(p, recordHooks(&events)).Stream(context.Background(), validRequest())
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	resp, err := CollectStream(s, "fake", "fake-model")
	if err != nil {
		t.Fatalf("CollectStream() error = %v", err)
	}
	if resp.Text() != "Paris" {
		t.Errorf("Text() = %q, want Paris", resp.Text())
	}

	ends := eventsOfType(events, OpStreamEnd)
	if len(ends) != 1 || !ends[0].Completed {
		t.Fatalf("got %d %s events (completed=%v), want 1 completed",
			len(ends), OpStreamEnd, len(ends) == 1 && ends[0].Completed)
	}
	if ends[0].Usage.InputTokens != 4 {
		t.Errorf("Usage = %+v, want the terminal event's", ends[0].Usage)
	}
}

// Complete's event carries what the OpenTelemetry conventions ask for: the
// model that actually answered, the response ID, and the stop reason. All
// three were in scope at the emit site and simply not carried before.
func TestCompleteEventCarriesResponseDetail(t *testing.T) {
	t.Parallel()

	var events []HookEvent
	p := &fakeProvider{results: []result{{resp: &Response{
		ID:         "resp_123",
		Provider:   "fake",
		Model:      "served-model-0613",
		StopReason: StopMaxTokens,
		Usage:      Usage{InputTokens: 9, OutputTokens: 3},
	}}}}

	req := validRequest()
	req.MaxTokens = 64

	if _, err := New(p, recordHooks(&events)).Complete(context.Background(), req); err != nil {
		t.Fatalf("Complete() error = %v", err)
	}

	completes := eventsOfType(events, OpComplete)
	if len(completes) != 1 {
		t.Fatalf("got %d %s events, want 1", len(completes), OpComplete)
	}
	ev := completes[0]

	if ev.ResponseID != "resp_123" {
		t.Errorf("ResponseID = %q, want resp_123", ev.ResponseID)
	}
	if ev.ResponseModel != "served-model-0613" {
		t.Errorf("ResponseModel = %q, want the model that answered", ev.ResponseModel)
	}
	if ev.Model != "some-model" {
		t.Errorf("Model = %q, want the model that was asked for", ev.Model)
	}
	if ev.StopReason != StopMaxTokens {
		t.Errorf("StopReason = %q, want %q", ev.StopReason, StopMaxTokens)
	}
	if ev.Request == nil || ev.Request.MaxTokens != 64 {
		t.Error("Request is not carried; sampling parameters are unreportable without it")
	}
}
