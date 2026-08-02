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

// HookEvent describes one completed attempt against a provider.
type HookEvent struct {
	// Provider and Model identify what was called.
	Provider string
	Model    string

	// Operation is "complete", "stream", or "models".
	Operation string

	// Attempt is the zero-based retry attempt this event reports.
	Attempt int

	// Duration is how long the attempt took.
	Duration time.Duration

	// Err is the attempt's error, or nil on success. A non-nil Err on a
	// non-final attempt was retried.
	Err error

	// Usage is token consumption, populated for successful "complete" calls.
	// Streaming usage is not known until the stream is drained, so it is
	// absent for "stream".
	Usage Usage
}

// Hook observes attempts. Register one with [WithHook].
//
// Hooks run synchronously on the calling goroutine, so a slow hook slows the
// request. Do metrics and logging; do not do I/O without a timeout.
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
			maxRetries: defaultMaxRetries,
			baseDelay:  defaultBaseDelay,
			maxDelay:   defaultMaxDelay,
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
			Operation: "complete",
			Attempt:   attempt,
			Duration:  time.Since(start),
			Err:       err,
		}
		if resp != nil {
			ev.Usage = resp.Usage
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
			Operation: "stream",
			Attempt:   attempt,
			Duration:  time.Since(start),
			Err:       err,
		})

		if err == nil {
			return stream, nil
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
