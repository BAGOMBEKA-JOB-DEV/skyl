package skyl

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeProvider is a scriptable Provider for exercising Client behaviour
// without touching the network.
type fakeProvider struct {
	name string

	mu    sync.Mutex
	calls int

	// results is consumed one entry per Complete call; the last entry repeats.
	results []result
	models  []ModelInfo
	modelsE error
	stream  Stream
	streamE error
}

type result struct {
	resp *Response
	err  error
}

func (f *fakeProvider) Name() string {
	if f.name == "" {
		return "fake"
	}
	return f.name
}

func (f *fakeProvider) Complete(ctx context.Context, _ *Request) (*Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	i := min(f.calls, len(f.results)-1)
	f.calls++
	if len(f.results) == 0 {
		return &Response{Provider: f.Name()}, nil
	}
	return f.results[i].resp, f.results[i].err
}

func (f *fakeProvider) Stream(context.Context, *Request) (Stream, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.stream, f.streamE
}

func (f *fakeProvider) Models(context.Context) ([]ModelInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.models, f.modelsE
}

func (f *fakeProvider) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func validRequest() *Request {
	return &Request{
		Model:    "some-model",
		Messages: []Message{UserText("hi")},
	}
}

// fastClient keeps retry backoff negligible so tests stay quick.
func fastClient(p Provider, opts ...Option) *Client {
	base := []Option{WithRetryDelay(time.Microsecond, time.Millisecond)}
	return New(p, append(base, opts...)...)
}

func TestNewPanicsOnNilProvider(t *testing.T) {
	t.Parallel()

	defer func() {
		if recover() == nil {
			t.Error("New(nil) did not panic; a nil provider must fail loudly, not on first use")
		}
	}()
	New(nil)
}

func TestCompleteValidatesBeforeDispatch(t *testing.T) {
	t.Parallel()

	p := &fakeProvider{}
	_, err := New(p).Complete(context.Background(), &Request{Model: "", Messages: nil})

	if !errors.Is(err, ErrBadRequest) {
		t.Errorf("err = %v, want ErrBadRequest", err)
	}
	if p.callCount() != 0 {
		t.Errorf("provider was called %d times; a malformed request must not cost a round trip",
			p.callCount())
	}
}

func TestCompleteRetriesRetryableErrors(t *testing.T) {
	t.Parallel()

	p := &fakeProvider{results: []result{
		{err: NewError("fake", 429, ErrRateLimit, "slow down", nil)},
		{err: NewError("fake", 500, ErrServer, "boom", nil)},
		{resp: &Response{Provider: "fake", Message: AssistantText("ok")}},
	}}

	resp, err := fastClient(p).Complete(context.Background(), validRequest())
	if err != nil {
		t.Fatalf("Complete() error = %v, want success after retries", err)
	}
	if resp.Text() != "ok" {
		t.Errorf("Text() = %q, want %q", resp.Text(), "ok")
	}
	if p.callCount() != 3 {
		t.Errorf("provider called %d times, want 3", p.callCount())
	}
}

func TestCompleteDoesNotRetryTerminalErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		kind error
		code int
	}{
		{"auth", ErrAuth, 401},
		{"bad request", ErrBadRequest, 400},
		{"not found", ErrNotFound, 404},
		{"refusal", ErrRefusal, 200},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := &fakeProvider{results: []result{
				{err: NewError("fake", tc.code, tc.kind, "nope", nil)},
			}}

			_, err := fastClient(p).Complete(context.Background(), validRequest())
			if !errors.Is(err, tc.kind) {
				t.Errorf("err = %v, want %v", err, tc.kind)
			}
			if p.callCount() != 1 {
				t.Errorf("provider called %d times; %s must not be retried", p.callCount(), tc.name)
			}
		})
	}
}

func TestCompleteGivesUpAfterMaxRetries(t *testing.T) {
	t.Parallel()

	p := &fakeProvider{results: []result{
		{err: NewError("fake", 500, ErrServer, "always down", nil)},
	}}

	_, err := fastClient(p, WithMaxRetries(2)).Complete(context.Background(), validRequest())
	if err == nil {
		t.Fatal("Complete() error = nil, want failure")
	}
	if !errors.Is(err, ErrServer) {
		t.Errorf("err = %v, want it to wrap ErrServer", err)
	}
	if !strings.Contains(err.Error(), "3 attempts") {
		t.Errorf("err = %q, want it to report the attempt count", err.Error())
	}
	if p.callCount() != 3 {
		t.Errorf("provider called %d times, want 3 (initial + 2 retries)", p.callCount())
	}
}

func TestWithMaxRetriesZeroDisablesRetry(t *testing.T) {
	t.Parallel()

	p := &fakeProvider{results: []result{
		{err: NewError("fake", 500, ErrServer, "down", nil)},
	}}

	_, _ = fastClient(p, WithMaxRetries(0)).Complete(context.Background(), validRequest())
	if p.callCount() != 1 {
		t.Errorf("provider called %d times, want 1", p.callCount())
	}
}

func TestCompleteStopsOnCancelledContext(t *testing.T) {
	t.Parallel()

	p := &fakeProvider{results: []result{
		{err: NewError("fake", 500, ErrServer, "down", nil)},
	}}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := fastClient(p).Complete(ctx, validRequest())
	if err == nil {
		t.Fatal("Complete() error = nil, want a context error")
	}
	if p.callCount() > 1 {
		t.Errorf("provider called %d times; a cancelled context must stop retrying", p.callCount())
	}
}

func TestHooksObserveEveryAttempt(t *testing.T) {
	t.Parallel()

	p := &fakeProvider{results: []result{
		{err: NewError("fake", 500, ErrServer, "boom", nil)},
		{resp: &Response{Provider: "fake", Usage: Usage{InputTokens: 7, OutputTokens: 3}}},
	}}

	var (
		mu     sync.Mutex
		events []HookEvent
	)
	client := fastClient(p, WithHook(func(_ context.Context, ev HookEvent) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, ev)
	}))

	if _, err := client.Complete(context.Background(), validRequest()); err != nil {
		t.Fatalf("Complete() error = %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(events) != 2 {
		t.Fatalf("saw %d hook events, want 2 (one per attempt)", len(events))
	}
	if events[0].Err == nil {
		t.Error("first hook event should carry the failure that was retried")
	}
	if events[0].Attempt != 0 || events[1].Attempt != 1 {
		t.Errorf("attempt numbers = %d,%d; want 0,1", events[0].Attempt, events[1].Attempt)
	}
	if events[1].Usage.InputTokens != 7 {
		t.Errorf("usage not reported to hook: %+v", events[1].Usage)
	}
	for _, ev := range events {
		if ev.Operation != "complete" || ev.Provider != "fake" {
			t.Errorf("hook event = %+v, want operation=complete provider=fake", ev)
		}
	}
}

func TestMultipleHooksAllFire(t *testing.T) {
	t.Parallel()

	var a, b atomic.Int32
	client := fastClient(&fakeProvider{results: []result{{resp: &Response{}}}},
		WithHook(func(context.Context, HookEvent) { a.Add(1) }),
		WithHook(func(context.Context, HookEvent) { b.Add(1) }),
	)

	if _, err := client.Complete(context.Background(), validRequest()); err != nil {
		t.Fatalf("Complete() error = %v", err)
	}
	if a.Load() != 1 || b.Load() != 1 {
		t.Errorf("hook counts = %d,%d; want 1,1", a.Load(), b.Load())
	}
}

func TestNilHookIsIgnored(t *testing.T) {
	t.Parallel()

	client := fastClient(&fakeProvider{results: []result{{resp: &Response{}}}}, WithHook(nil))
	if _, err := client.Complete(context.Background(), validRequest()); err != nil {
		t.Fatalf("a nil hook should be dropped, not panic: %v", err)
	}
}

func TestModelsDoesNotRetryUnsupported(t *testing.T) {
	t.Parallel()

	p := &fakeProvider{modelsE: Unsupportedf("fake", "no models endpoint")}

	_, err := fastClient(p).Models(context.Background())
	if !errors.Is(err, ErrUnsupported) {
		t.Errorf("err = %v, want ErrUnsupported", err)
	}
	if p.callCount() != 1 {
		t.Errorf("provider called %d times; a provider that cannot list models never will",
			p.callCount())
	}
}

func TestModelsSuccess(t *testing.T) {
	t.Parallel()

	p := &fakeProvider{models: []ModelInfo{{ID: "m1"}, {ID: "m2"}}}
	got, err := fastClient(p).Models(context.Background())
	if err != nil {
		t.Fatalf("Models() error = %v", err)
	}
	if len(got) != 2 || got[0].ID != "m1" {
		t.Errorf("Models() = %+v, want two models starting with m1", got)
	}
}

func TestStreamValidatesAndRetriesHandshake(t *testing.T) {
	t.Parallel()

	t.Run("validates", func(t *testing.T) {
		t.Parallel()
		p := &fakeProvider{}
		_, err := New(p).Stream(context.Background(), &Request{})
		if !errors.Is(err, ErrBadRequest) {
			t.Errorf("err = %v, want ErrBadRequest", err)
		}
		if p.callCount() != 0 {
			t.Error("provider was called despite an invalid request")
		}
	})

	t.Run("does not retry terminal handshake failure", func(t *testing.T) {
		t.Parallel()
		p := &fakeProvider{streamE: NewError("fake", 401, ErrAuth, "bad key", nil)}
		_, err := fastClient(p).Stream(context.Background(), validRequest())
		if !errors.Is(err, ErrAuth) {
			t.Errorf("err = %v, want ErrAuth", err)
		}
		if p.callCount() != 1 {
			t.Errorf("provider called %d times, want 1", p.callCount())
		}
	})
}

func TestProviderAccessor(t *testing.T) {
	t.Parallel()

	p := &fakeProvider{name: "custom"}
	if got := New(p).Provider(); got != p {
		t.Error("Provider() did not return the wrapped provider")
	}
}

func TestClientIsConcurrencySafe(t *testing.T) {
	t.Parallel()

	client := fastClient(&fakeProvider{results: []result{{resp: &Response{}}}})

	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := client.Complete(context.Background(), validRequest()); err != nil {
				t.Errorf("concurrent Complete() error = %v", err)
			}
		}()
	}
	wg.Wait()
}

func TestCollectStream(t *testing.T) {
	t.Parallel()

	call := ToolCall{ID: "c1", Name: "get_weather", Arguments: json.RawMessage(`{"city":"Kampala"}`)}
	s := &scriptedStream{events: []StreamEvent{
		{Type: EventTextDelta, Text: "Hello"},
		{Type: EventTextDelta, Text: ", world"},
		{Type: EventToolCall, ToolCall: &call},
		{Type: EventDone, Usage: &Usage{InputTokens: 4, OutputTokens: 9}, StopReason: StopToolUse},
	}}

	resp, err := CollectStream(s, "fake", "some-model")
	if err != nil {
		t.Fatalf("CollectStream() error = %v", err)
	}
	if resp.Text() != "Hello, world" {
		t.Errorf("Text() = %q, want %q", resp.Text(), "Hello, world")
	}
	if calls := resp.ToolCalls(); len(calls) != 1 || calls[0].Name != "get_weather" {
		t.Errorf("ToolCalls() = %+v, want one get_weather call", calls)
	}
	if resp.StopReason != StopToolUse {
		t.Errorf("StopReason = %q, want %q", resp.StopReason, StopToolUse)
	}
	if resp.Usage.OutputTokens != 9 {
		t.Errorf("Usage.OutputTokens = %d, want 9", resp.Usage.OutputTokens)
	}
	if !s.closed {
		t.Error("CollectStream must close the stream it drains")
	}
}

func TestCollectStreamPropagatesError(t *testing.T) {
	t.Parallel()

	s := &scriptedStream{
		events: []StreamEvent{{Type: EventTextDelta, Text: "partial"}},
		err:    NewError("fake", 500, ErrServer, "died mid-stream", nil),
	}

	if _, err := CollectStream(s, "fake", "m"); !errors.Is(err, ErrServer) {
		t.Errorf("err = %v, want ErrServer", err)
	}
	if !s.closed {
		t.Error("CollectStream must close the stream even when it fails")
	}
}

// scriptedStream replays a fixed event list.
type scriptedStream struct {
	events []StreamEvent
	err    error
	i      int
	closed bool
}

func (s *scriptedStream) Next() bool {
	if s.i >= len(s.events) {
		return false
	}
	s.i++
	return true
}

func (s *scriptedStream) Event() StreamEvent { return s.events[s.i-1] }
func (s *scriptedStream) Err() error         { return s.err }
func (s *scriptedStream) Close() error       { s.closed = true; return nil }
