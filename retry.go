package skyl

import (
	"context"
	"errors"
	"math/rand/v2"
	"time"
)

// Retry defaults. Deliberately conservative: a library that retries hard by
// default turns a provider's bad minute into its bad hour.
const (
	defaultMaxRetries = 3
	defaultBaseDelay  = 500 * time.Millisecond
	defaultMaxDelay   = 30 * time.Second
)

// retryPolicy decides whether and how long to wait before another attempt.
type retryPolicy struct {
	maxRetries int
	baseDelay  time.Duration
	maxDelay   time.Duration
}

// shouldRetry reports whether err is worth another attempt.
//
// Only rate limits, provider-side failures, and transport errors qualify.
// Retrying an auth failure, a malformed request, a missing model, or a refusal
// spends quota to receive the same answer.
func shouldRetry(err error) bool {
	if err == nil {
		return false
	}
	// A cancelled or expired context means the caller is done waiting.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}

	var e *Error
	if errors.As(err, &e) {
		return e.Retryable()
	}

	// Unclassified errors are most often transport failures, which are worth
	// one more attempt. Anything an adapter classified will have taken the
	// branch above.
	return true
}

// backoff returns how long to wait before attempt n (zero-based).
//
// The delay is exponential with full jitter: a uniform sample from
// [0, min(base*2^n, max)]. Jitter is not decoration — a fleet retrying on a
// fixed schedule reconverges into a thundering herd against a provider that is
// already struggling.
//
// A provider's own Retry-After hint wins over the computed delay when it is
// longer, because the provider knows better than we do.
func (p retryPolicy) backoff(attempt int, retryAfter time.Duration) time.Duration {
	base := p.baseDelay
	if base <= 0 {
		base = defaultBaseDelay
	}
	maxDelay := p.maxDelay
	if maxDelay <= 0 {
		maxDelay = defaultMaxDelay
	}

	// Cap the shift before it overflows; 2^20 * base is far past maxDelay.
	shift := attempt
	if shift > 20 {
		shift = 20
	}

	ceiling := base << uint(shift)
	if ceiling > maxDelay || ceiling <= 0 {
		ceiling = maxDelay
	}

	delay := time.Duration(rand.Int64N(int64(ceiling) + 1))

	if retryAfter > 0 && retryAfter > delay {
		delay = retryAfter
	}
	if delay > maxDelay {
		// Honour a long Retry-After up to a bound; an hour-long hint should
		// not silently wedge the caller's request.
		delay = maxDelay
	}
	return delay
}

// retryAfterFrom extracts a provider's Retry-After hint, if it gave one.
func retryAfterFrom(err error) time.Duration {
	var e *Error
	if errors.As(err, &e) {
		return e.RetryAfter
	}
	return 0
}

// sleep waits for d, or returns early if ctx is done.
func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
