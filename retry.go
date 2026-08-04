package skyl

import (
	"context"
	"crypto/tls"
	"crypto/x509"
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

	// defaultRetryAfterCap bounds how long a provider's own Retry-After hint
	// may hold a request. It is deliberately much larger than defaultMaxDelay:
	// provider rate-limit windows are typically 60 seconds, so clamping a hint
	// to the jitter ceiling would retry inside a window the provider has
	// already told us is closed — spending the whole retry budget to collect
	// the same 429 four times.
	defaultRetryAfterCap = 5 * time.Minute
)

// retryPolicy decides whether and how long to wait before another attempt.
type retryPolicy struct {
	maxRetries int
	baseDelay  time.Duration

	// maxDelay caps the computed exponential backoff.
	maxDelay time.Duration

	// retryAfterCap caps a provider-supplied Retry-After. Separate from
	// maxDelay because the two answer different questions: how long we choose
	// to wait, versus how long we will let a provider make us wait.
	retryAfterCap time.Duration
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

	// A rejected certificate is a misconfiguration, not a blip: the next three
	// attempts fail identically and only delay the error the operator needs to
	// see.
	var certErr *tls.CertificateVerificationError
	var hostErr x509.HostnameError
	var authErr x509.UnknownAuthorityError
	if errors.As(err, &certErr) || errors.As(err, &hostErr) || errors.As(err, &authErr) {
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
// longer, because the provider knows better than we do — and it is bounded by
// retryAfterCap rather than by maxDelay, so that honouring a normal 60-second
// rate-limit window does not require raising the jitter ceiling to match.
func (p retryPolicy) backoff(attempt int, retryAfter time.Duration) time.Duration {
	base := p.baseDelay
	if base <= 0 {
		base = defaultBaseDelay
	}
	maxDelay := p.maxDelay
	if maxDelay <= 0 {
		maxDelay = defaultMaxDelay
	}
	retryAfterCap := p.retryAfterCap
	if retryAfterCap <= 0 {
		retryAfterCap = defaultRetryAfterCap
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
	if delay > maxDelay {
		delay = maxDelay
	}

	if retryAfter > 0 && retryAfter > delay {
		// Honour the hint, but bound it: an hour-long Retry-After should not
		// silently wedge the caller's request. The caller's context still
		// bounds the whole sequence regardless.
		delay = retryAfter
		if delay > retryAfterCap {
			delay = retryAfterCap
		}
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
