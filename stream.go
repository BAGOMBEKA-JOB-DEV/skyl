package skyl

import "encoding/json"

// EventType identifies what a [StreamEvent] carries.
type EventType string

// The stream event types skyl emits. Adapters map provider events onto these
// and drop provider events that carry no information a caller can act on.
const (
	// EventTextDelta carries the next fragment of assistant text in
	// [StreamEvent.Text]. This is the event most callers care about.
	EventTextDelta EventType = "text_delta"

	// EventThinkingDelta carries a fragment of the model's reasoning, when
	// the provider discloses it. Many models never emit this, and several
	// return reasoning only as an opaque summary.
	EventThinkingDelta EventType = "thinking_delta"

	// EventToolCall reports a completed tool call in [StreamEvent.ToolCall].
	// skyl buffers partial tool arguments and emits this once, when the
	// call's JSON is whole — a half-parsed tool call is not actionable.
	EventToolCall EventType = "tool_call"

	// EventDone is the final event of a successful stream. It carries
	// [StreamEvent.Usage] and [StreamEvent.StopReason] when the provider
	// reports them.
	EventDone EventType = "done"
)

// StreamEvent is one incremental update from a streaming response.
//
// Which fields are meaningful depends on [StreamEvent.Type]; the rest are
// zero.
type StreamEvent struct {
	Type EventType

	// Text is the fragment, for [EventTextDelta] and [EventThinkingDelta].
	Text string

	// ToolCall is the completed call, for [EventToolCall].
	ToolCall *ToolCall

	// Usage is token consumption, for [EventDone].
	Usage *Usage

	// StopReason is why generation ended, for [EventDone].
	StopReason StopReason

	// Raw is the provider's untouched event payload, when one exists.
	Raw json.RawMessage
}

// Stream delivers a response incrementally.
//
// It is a pull iterator rather than a channel, because that composes with
// defer and context cancellation the way Go programmers expect — a channel
// would need a second channel for errors and is easy to leak on early return.
//
// The usual shape:
//
//	stream, err := client.Stream(ctx, req)
//	if err != nil {
//		return err
//	}
//	defer stream.Close()
//
//	for stream.Next() {
//		if ev := stream.Event(); ev.Type == skyl.EventTextDelta {
//			fmt.Print(ev.Text)
//		}
//	}
//	return stream.Err()
//
// Always check [Stream.Err] after the loop: Next returning false means either
// the stream finished or it failed, and only Err distinguishes them.
//
// Abandoning a stream early is safe — Close releases the connection, and the
// reader is bound to the request context — but Close is what makes the release
// prompt rather than eventual.
//
// A Stream is NOT safe for concurrent use. One stream, one consuming
// goroutine.
type Stream interface {
	// Next advances to the next event, reporting whether one is available.
	// It returns false at end of stream and on error.
	Next() bool

	// Event returns the event Next just advanced to. It is only valid after
	// Next returns true.
	Event() StreamEvent

	// Err returns the error that stopped the stream, or nil if it ended
	// normally. Check it after Next returns false.
	Err() error

	// Close releases the stream's resources. It is safe to call more than
	// once, and safe to call before the stream is exhausted.
	Close() error
}

// CollectStream drains a stream into a single [Response].
//
// It is a convenience for callers who want streaming's early-first-byte
// behaviour without handling events — a progress spinner, say, or a timeout
// guard on a long generation. It always closes the stream.
//
// The returned Response has no Raw payload: it is assembled from events, not
// from one provider body.
func CollectStream(s Stream, provider, model string) (*Response, error) {
	defer s.Close() //nolint:errcheck // the stream error is reported via s.Err

	var (
		text  []byte
		calls []Part
		resp  = &Response{
			Provider:   provider,
			Model:      model,
			StopReason: StopUnknown,
		}
	)

	for s.Next() {
		ev := s.Event()
		switch ev.Type {
		case EventTextDelta:
			text = append(text, ev.Text...)
		case EventToolCall:
			if ev.ToolCall != nil {
				calls = append(calls, *ev.ToolCall)
			}
		case EventDone:
			if ev.Usage != nil {
				resp.Usage = *ev.Usage
			}
			if ev.StopReason != "" {
				resp.StopReason = ev.StopReason
			}
		}
	}
	if err := s.Err(); err != nil {
		return nil, err
	}

	parts := make([]Part, 0, 1+len(calls))
	if len(text) > 0 {
		parts = append(parts, Text{Text: string(text)})
	}
	parts = append(parts, calls...)

	resp.Message = Message{Role: RoleAssistant, Parts: parts}
	if resp.StopReason == StopUnknown && len(calls) > 0 {
		resp.StopReason = StopToolUse
	}
	return resp, nil
}
