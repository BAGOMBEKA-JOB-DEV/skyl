package skyl

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Sentinel errors describing what went wrong, independent of provider.
//
// Branch on these with [errors.Is] rather than inspecting message text —
// providers reword their messages, and string matching breaks silently when
// they do.
var (
	// ErrAuth means the credential was missing, malformed, or rejected.
	// Never retried: the same key will fail again.
	ErrAuth = errors.New("skyl: authentication failed")

	// ErrRateLimit means the provider is throttling. Retried with backoff.
	ErrRateLimit = errors.New("skyl: rate limited")

	// ErrNotFound means the model or endpoint does not exist for this
	// account. Because skyl passes model IDs through unvalidated, a typo
	// arrives here rather than failing locally.
	ErrNotFound = errors.New("skyl: not found")

	// ErrBadRequest means the request was malformed. Never retried.
	ErrBadRequest = errors.New("skyl: invalid request")

	// ErrServer means the provider failed on its side. Retried with backoff.
	ErrServer = errors.New("skyl: provider server error")

	// ErrUnsupported means this provider cannot express part of the request.
	//
	// Adapters return it instead of silently dropping data, because a
	// quietly discarded image looks like a model that ignored the question.
	ErrUnsupported = errors.New("skyl: unsupported by this provider")

	// ErrRefusal means the model or its safety classifiers declined. Never
	// retried: the same prompt gets the same answer.
	ErrRefusal = errors.New("skyl: model declined the request")

	// ErrStreamClosed means the stream was used after being closed.
	ErrStreamClosed = errors.New("skyl: stream is closed")
)

// Error is a provider failure with enough context to act on.
//
// It wraps one of the sentinel errors above, so [errors.Is] works through it,
// and [errors.As] recovers the detail:
//
//	var e *skyl.Error
//	if errors.As(err, &e) {
//		log.Printf("%s returned %d", e.Provider, e.StatusCode)
//	}
//
// An Error never contains credentials. See docs/rules.md §7.2.
type Error struct {
	// Provider is the adapter that produced the failure.
	Provider string

	// StatusCode is the HTTP status, or 0 for transport-level failures.
	StatusCode int

	// Message is the provider's explanation, when it gave one.
	Message string

	// Kind is the sentinel this failure classifies as. Unwrap returns it.
	Kind error

	// RetryAfter is how long the provider asked us to wait. Zero when it did
	// not say.
	RetryAfter time.Duration

	// Body is the raw error payload, truncated. Useful when a provider
	// reports something skyl does not model.
	Body string
}

// Error implements the error interface.
func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString("skyl: ")
	if e.Provider != "" {
		b.WriteString(e.Provider)
		b.WriteString(": ")
	}
	if e.Kind != nil {
		// Trim the sentinel's own "skyl: " prefix to avoid repeating it.
		b.WriteString(strings.TrimPrefix(e.Kind.Error(), "skyl: "))
	} else {
		b.WriteString("request failed")
	}
	if e.StatusCode != 0 {
		fmt.Fprintf(&b, " (http %d)", e.StatusCode)
	}
	if e.Message != "" {
		b.WriteString(": ")
		b.WriteString(e.Message)
	}
	return b.String()
}

// Unwrap returns the sentinel this error classifies as, so [errors.Is]
// matches it.
func (e *Error) Unwrap() error { return e.Kind }

// Retryable reports whether retrying could plausibly succeed.
//
// Rate limits, server errors, and transport failures are retryable.
// Authentication failures, malformed requests, missing models, and refusals
// are not — retrying those burns quota to receive the same answer.
func (e *Error) Retryable() bool {
	switch e.Kind {
	case ErrRateLimit, ErrServer:
		return true
	default:
		// StatusCode 0 means the request never got a reply: a dial timeout,
		// a reset connection. Those are worth another attempt.
		return e.StatusCode == 0 && e.Kind == nil
	}
}

// maxBodyInError bounds how much of a provider error payload we keep. Enough
// to diagnose, small enough not to bloat logs.
const maxBodyInError = 2048

// NewError builds a classified provider error.
//
// Adapters use it so that every provider failure reaches callers in the same
// shape.
func NewError(provider string, status int, kind error, message string, body []byte) *Error {
	e := &Error{
		Provider:   provider,
		StatusCode: status,
		Kind:       kind,
		Message:    message,
	}
	if len(body) > 0 {
		if len(body) > maxBodyInError {
			e.Body = string(body[:maxBodyInError]) + "… (truncated)"
		} else {
			e.Body = string(body)
		}
	}
	return e
}

// ClassifyStatus maps an HTTP status code onto a skyl sentinel.
//
// Adapters should prefer a provider's own error type where it is more precise,
// and fall back to this.
func ClassifyStatus(status int) error {
	switch {
	case status == http.StatusUnauthorized,
		status == http.StatusForbidden,
		status == http.StatusProxyAuthRequired:
		return ErrAuth
	case status == http.StatusNotFound:
		return ErrNotFound
	case status == http.StatusTooManyRequests:
		return ErrRateLimit
	case status == http.StatusRequestTimeout,
		status == http.StatusConflict:
		// Transient conflicts and timeouts are worth another attempt.
		return ErrServer
	case status >= 500:
		return ErrServer
	case status >= 400:
		return ErrBadRequest
	default:
		return nil
	}
}

// ParseRetryAfter interprets a Retry-After header.
//
// The header may be a delay in seconds or an HTTP date; both forms are
// handled. It returns zero when the value is absent or unparseable, and never
// returns a negative duration.
func ParseRetryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.ParseFloat(v, 64); err == nil {
		if secs <= 0 {
			return 0
		}
		return time.Duration(secs * float64(time.Second))
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

// Unsupportedf builds an [ErrUnsupported] error naming what could not be
// represented, so the caller can tell exactly which part failed.
//
// Adapters use it instead of silently dropping data they cannot express.
func Unsupportedf(provider, format string, args ...any) error {
	return &Error{
		Provider: provider,
		Kind:     ErrUnsupported,
		Message:  fmt.Sprintf(format, args...),
	}
}
