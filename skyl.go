// Package skyl provides one Go interface for every AI model.
//
// It lets you talk to Claude, GPT, Gemini, and hundreds of other models
// through a single stable API, then switch between them by changing one
// string.
//
//	client := skyl.New(anthropic.New(os.Getenv("ANTHROPIC_API_KEY")))
//
//	resp, err := client.Complete(ctx, &skyl.Request{
//		Model:     "claude-opus-5",
//		MaxTokens: 1024,
//		Messages:  []skyl.Message{skyl.UserText("Hello")},
//	})
//
// # Models
//
// skyl never validates model IDs against a list. [Request.Model] is passed to
// the provider untouched, so a model released after your skyl build works
// immediately. Ask a provider what it currently offers with [Client.Models].
//
// # Escape hatches
//
// Every abstraction over a fast-moving API is wrong somewhere, so skyl is
// never the last word: [Request.ProviderOptions] sends fields skyl does not
// model, and [Response.Raw] exposes the untouched provider reply.
package skyl

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// defaultTimeout bounds a single attempt. Generous, because reasoning models
// legitimately take minutes on hard problems.
const defaultTimeout = 10 * time.Minute

// Operations reported in [HookEvent.Operation].
const (
	// OpComplete is a non-streaming request.
	OpComplete = "complete"

	// OpStream is a streaming request's handshake. It fires as soon as the
	// provider accepts the request, before any token exists, so it carries no
	// usage — see [OpStreamEnd].
	OpStream = "stream"

	// OpStreamEnd fires once when a stream finishes, and is where streaming
	// token usage is reported. Duration covers the whole stream rather than
	// the handshake.
	OpStreamEnd = "stream_end"

	// OpModels is a model-listing request.
	OpModels = "models"
)

// HookEvent describes one completed attempt against a provider.
type HookEvent struct {
	// Provider and Model identify what was called. Model is the model that was
	// *asked for*; see ResponseModel for the one that answered.
	Provider string
	Model    string

	// Operation is one of [OpComplete], [OpStream], [OpStreamEnd], or
	// [OpModels].
	Operation string

	// Attempt is the zero-based retry attempt this event reports.
	Attempt int

	// Duration is how long the attempt took. For [OpStream] that is the
	// handshake alone, because the stream outlives the call; for
	// [OpStreamEnd] it is the whole stream, handshake included.
	Duration time.Duration

	// Err is the attempt's error, or nil on success. A non-nil Err on a
	// non-final attempt was retried.
	Err error

	// Usage is token consumption. Populated for successful [OpComplete] calls,
	// and for [OpStreamEnd] when the stream ran to completion.
	Usage Usage

	// ResponseID is the provider's identifier for the response, when it gave
	// one. It is what a provider's support team will ask for.
	ResponseID string

	// ResponseModel is the model that actually served the request, read from
	// the response rather than echoed from the request — providers can and do
	// serve a different model than the one asked for.
	ResponseModel string

	// StopReason explains why generation ended, for [OpComplete] and a
	// completed [OpStreamEnd].
	StopReason StopReason

	// Completed reports whether a stream ran to its terminal event. It is
	// meaningful only for [OpStreamEnd].
	//
	// False means the caller closed the stream early — an abandoned request,
	// a client hang-up, an error mid-flight. Those tokens were still generated
	// and still billed, so the event fires anyway; Usage is simply whatever
	// arrived before the stream was dropped, usually nothing.
	Completed bool

	// Request is the request that produced this event.
	//
	// It is supplied so a hook can report sampling parameters — the
	// OpenTelemetry GenAI conventions ask for temperature, top_p and
	// max_tokens — without this struct growing a field per parameter.
	//
	// It carries the prompt. Anything a hook does with it is a decision about
	// user data: logging it verbatim ships conversation content to wherever
	// the logs go. Treat it as read-only; skyl reuses it across retries.
	Request *Request
}

// Hook observes attempts. Register one with [WithHook].
//
// Hooks run synchronously on the calling goroutine, so a slow hook slows the
// request. Do metrics and logging; do not do I/O without a timeout.
//
// Two things to know about [OpStreamEnd] in particular. It fires from whichever
// of the stream's terminal event or its Close comes first, so on the Close path
// the hook runs inside the caller's defer and its latency lands there. And the
// ctx it receives is the one the stream was opened with, which is frequently
// already cancelled by then — a client hanging up is the ordinary reason a
// stream is abandoned. A hook that needs to record that event must not depend
// on that ctx being live.
type Hook func(ctx context.Context, ev HookEvent)

// Client wraps a [Provider] with the behaviour every production caller needs:
// request validation, retry with jittered backoff, per-attempt timeouts, and
// observability hooks.
//
// Keeping these in Client rather than in each adapter means they are written
// and tested once, so a new adapter inherits them for free.
//
// A Client is safe for concurrent use by multiple goroutines.
type Client struct {
	provider Provider
	policy   retryPolicy
	timeout  time.Duration
	hooks    []Hook
}

// Option configures a [Client]. Options are applied in order.
type Option func(*Client)

// WithMaxRetries sets how many times a retryable failure is retried.
//
// Zero disables retries. Negative values are ignored. The default is 3.
func WithMaxRetries(n int) Option {
	return func(c *Client) {
		if n >= 0 {
			c.policy.maxRetries = n
		}
	}
}

// WithRetryDelay sets the backoff bounds.
//
// The delay for attempt n is sampled uniformly from [0, min(base*2^n, max)].
// Non-positive values are ignored. Defaults are 500ms and 30s.
func WithRetryDelay(base, max time.Duration) Option {
	return func(c *Client) {
		if base > 0 {
			c.policy.baseDelay = base
		}
		if max > 0 {
			c.policy.maxDelay = max
		}
	}
}

// WithRetryAfterCap bounds how long a provider's own Retry-After hint may
// delay a retry.
//
// It is separate from [WithRetryDelay]'s max, which caps only skyl's computed
// backoff: a provider asking for 60 seconds is a normal rate-limit window and
// should be honoured, while a provider asking for an hour should not silently
// wedge the caller. Non-positive values are ignored. The default is 5 minutes.
func WithRetryAfterCap(d time.Duration) Option {
	return func(c *Client) {
		if d > 0 {
			c.policy.retryAfterCap = d
		}
	}
}

// WithTimeout bounds a single attempt.
//
// It applies per attempt, not to the whole retry sequence — bound that with
// the context you pass. Non-positive values disable the per-attempt timeout,
// leaving only the caller's context. The default is 10 minutes.
func WithTimeout(d time.Duration) Option {
	return func(c *Client) { c.timeout = d }
}

// WithHook registers an observer for completed attempts. Hooks accumulate;
// calling it twice registers both.
func WithHook(h Hook) Option {
	return func(c *Client) {
		if h != nil {
			c.hooks = append(c.hooks, h)
		}
	}
}

// New returns a Client that dispatches to p.
//
// It panics if p is nil: a nil provider is a programmer error that would
// otherwise surface as a confusing nil dereference on the first request.
func New(p Provider, opts ...Option) *Client {
	if p == nil {
		panic("skyl: New called with a nil Provider")
	}
	c := &Client{
		provider: p,
		policy: retryPolicy{
			maxRetries:    defaultMaxRetries,
			baseDelay:     defaultBaseDelay,
			maxDelay:      defaultMaxDelay,
			retryAfterCap: defaultRetryAfterCap,
		},
		timeout: defaultTimeout,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Provider returns the underlying provider, for callers who want to bypass
// Client's retry and validation.
func (c *Client) Provider() Provider { return c.provider }

// Complete runs a request to completion, retrying retryable failures.
//
// The request is validated locally first, so a malformed request fails without
// costing a round trip.
func (c *Client) Complete(ctx context.Context, req *Request) (*Response, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}

	var lastErr error
	for attempt := 0; attempt <= c.policy.maxRetries; attempt++ {
		if attempt > 0 {
			delay := c.policy.backoff(attempt-1, retryAfterFrom(lastErr))
			if err := sleep(ctx, delay); err != nil {
				return nil, fmt.Errorf("skyl: waiting to retry: %w", err)
			}
		}

		start := time.Now()
		attemptCtx, cancel := c.withTimeout(ctx)
		resp, err := c.provider.Complete(attemptCtx, req)
		cancel()

		ev := HookEvent{
			Provider:  c.provider.Name(),
			Model:     req.Model,
			Operation: OpComplete,
			Attempt:   attempt,
			Duration:  time.Since(start),
			Err:       err,
			Request:   req,
		}
		if resp != nil {
			ev.Usage = resp.Usage
			ev.ResponseID = resp.ID
			ev.ResponseModel = resp.Model
			ev.StopReason = resp.StopReason
		}
		c.emit(ctx, ev)

		if err == nil {
			return resp, nil
		}
		lastErr = err

		if !shouldRetry(err) || ctx.Err() != nil {
			return nil, err
		}
	}

	return nil, fmt.Errorf("skyl: giving up after %d attempts: %w",
		c.policy.maxRetries+1, lastErr)
}

// Stream runs a request, returning events as the model produces them.
//
// Only the initial handshake is retried. Once bytes are flowing, a mid-stream
// failure is surfaced through [Stream.Err] rather than retried, because
// replaying a partially consumed response would duplicate output the caller
// has already seen.
//
// The returned Stream is bound to ctx and must be closed.
func (c *Client) Stream(ctx context.Context, req *Request) (Stream, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}

	var lastErr error
	for attempt := 0; attempt <= c.policy.maxRetries; attempt++ {
		if attempt > 0 {
			delay := c.policy.backoff(attempt-1, retryAfterFrom(lastErr))
			if err := sleep(ctx, delay); err != nil {
				return nil, fmt.Errorf("skyl: waiting to retry: %w", err)
			}
		}

		start := time.Now()
		// No per-attempt timeout: the stream outlives this call, and
		// cancelling its context would kill it mid-flight. The caller's ctx
		// bounds it.
		stream, err := c.provider.Stream(ctx, req)

		c.emit(ctx, HookEvent{
			Provider:  c.provider.Name(),
			Model:     req.Model,
			Operation: OpStream,
			Attempt:   attempt,
			Duration:  time.Since(start),
			Err:       err,
			Request:   req,
		})

		if err == nil {
			// Wrapped so the terminal event can be emitted when the caller is
			// finished with it. Without this the Client never sees the end of
			// the stream, and streaming token usage — the dominant mode for
			// chat — is unreportable.
			if len(c.hooks) == 0 {
				return stream, nil
			}
			return &observedStream{
				inner:    stream,
				client:   c,
				ctx:      ctx,
				req:      req,
				attempt:  attempt,
				started:  start,
				provider: c.provider.Name(),
			}, nil
		}
		lastErr = err

		if !shouldRetry(err) || ctx.Err() != nil {
			return nil, err
		}
	}

	return nil, fmt.Errorf("skyl: giving up after %d attempts: %w",
		c.policy.maxRetries+1, lastErr)
}

// Models lists the models the provider currently offers.
//
// The answer comes from the provider, live, so it is never stale. Providers
// with no such endpoint return [ErrUnsupported].
func (c *Client) Models(ctx context.Context) ([]ModelInfo, error) {
	var lastErr error
	for attempt := 0; attempt <= c.policy.maxRetries; attempt++ {
		if attempt > 0 {
			delay := c.policy.backoff(attempt-1, retryAfterFrom(lastErr))
			if err := sleep(ctx, delay); err != nil {
				return nil, fmt.Errorf("skyl: waiting to retry: %w", err)
			}
		}

		start := time.Now()
		attemptCtx, cancel := c.withTimeout(ctx)
		models, err := c.provider.Models(attemptCtx)
		cancel()

		c.emit(ctx, HookEvent{
			Provider:  c.provider.Name(),
			Operation: "models",
			Attempt:   attempt,
			Duration:  time.Since(start),
			Err:       err,
		})

		if err == nil {
			return models, nil
		}
		lastErr = err

		// A provider that cannot list models will never be able to; retrying
		// is pointless.
		if errors.Is(err, ErrUnsupported) || !shouldRetry(err) || ctx.Err() != nil {
			return nil, err
		}
	}

	return nil, fmt.Errorf("skyl: giving up after %d attempts: %w",
		c.policy.maxRetries+1, lastErr)
}

// withTimeout applies the per-attempt timeout, if one is configured.
func (c *Client) withTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if c.timeout <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, c.timeout)
}

// emit delivers an event to every registered hook.
func (c *Client) emit(ctx context.Context, ev HookEvent) {
	for _, h := range c.hooks {
		h(ctx, ev)
	}
}
