package skyl

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestClassifyStatus(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		status int
		want   error
	}{
		{"ok", 200, nil},
		{"created", 201, nil},
		{"unauthorized", 401, ErrAuth},
		{"forbidden", 403, ErrAuth},
		{"proxy auth", 407, ErrAuth},
		{"not found", 404, ErrNotFound},
		{"too many requests", 429, ErrRateLimit},
		{"request timeout is transient", 408, ErrServer},
		{"conflict is transient", 409, ErrServer},
		{"bad request", 400, ErrBadRequest},
		{"unprocessable", 422, ErrBadRequest},
		{"internal error", 500, ErrServer},
		{"bad gateway", 502, ErrServer},
		{"overloaded", 529, ErrServer},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := ClassifyStatus(tc.status); got != tc.want {
				t.Errorf("ClassifyStatus(%d) = %v, want %v", tc.status, got, tc.want)
			}
		})
	}
}

func TestParseRetryAfter(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		value string
		want  time.Duration
		fuzzy bool
	}{
		{name: "empty", value: "", want: 0},
		{name: "whitespace", value: "   ", want: 0},
		{name: "seconds", value: "30", want: 30 * time.Second},
		{name: "fractional seconds", value: "1.5", want: 1500 * time.Millisecond},
		{name: "zero", value: "0", want: 0},
		{name: "negative is ignored", value: "-5", want: 0},
		{name: "garbage", value: "soon", want: 0},
		{name: "past http date", value: "Mon, 02 Jan 2006 15:04:05 GMT", want: 0},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := ParseRetryAfter(tc.value); got != tc.want {
				t.Errorf("ParseRetryAfter(%q) = %v, want %v", tc.value, got, tc.want)
			}
		})
	}

	t.Run("future http date", func(t *testing.T) {
		t.Parallel()
		future := time.Now().Add(45 * time.Second).UTC().Format(http.TimeFormat)
		got := ParseRetryAfter(future)
		if got <= 0 || got > 50*time.Second {
			t.Errorf("ParseRetryAfter(future date) = %v, want roughly 45s", got)
		}
	})
}

func TestErrorIsAndAs(t *testing.T) {
	t.Parallel()

	err := NewError("openai", 429, ErrRateLimit, "slow down", []byte(`{"error":{}}`))

	if !errors.Is(err, ErrRateLimit) {
		t.Error("errors.Is(err, ErrRateLimit) = false; classification must survive wrapping")
	}
	if errors.Is(err, ErrAuth) {
		t.Error("errors.Is(err, ErrAuth) = true; classification leaked across sentinels")
	}

	var target *Error
	if !errors.As(err, &target) {
		t.Fatal("errors.As did not recover *Error")
	}
	if target.Provider != "openai" || target.StatusCode != 429 {
		t.Errorf("recovered provider=%q status=%d, want openai/429", target.Provider, target.StatusCode)
	}
}

func TestErrorMessageFormat(t *testing.T) {
	t.Parallel()

	got := NewError("anthropic", 401, ErrAuth, "invalid x-api-key", nil).Error()

	for _, want := range []string{"anthropic", "401", "invalid x-api-key"} {
		if !strings.Contains(got, want) {
			t.Errorf("Error() = %q, want it to contain %q", got, want)
		}
	}
	// The sentinel already carries a "skyl: " prefix; it must not be doubled.
	if strings.Count(got, "skyl:") != 1 {
		t.Errorf("Error() = %q, want exactly one %q prefix", got, "skyl:")
	}
}

func TestErrorRetryable(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  *Error
		want bool
	}{
		{"rate limit", &Error{Kind: ErrRateLimit}, true},
		{"server error", &Error{Kind: ErrServer}, true},
		{"transport failure", &Error{Message: "connection reset"}, true},
		{"auth", &Error{Kind: ErrAuth, StatusCode: 401}, false},
		{"bad request", &Error{Kind: ErrBadRequest, StatusCode: 400}, false},
		{"not found", &Error{Kind: ErrNotFound, StatusCode: 404}, false},
		{"refusal", &Error{Kind: ErrRefusal}, false},
		{"unsupported", &Error{Kind: ErrUnsupported}, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.err.Retryable(); got != tc.want {
				t.Errorf("Retryable() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestNewErrorTruncatesBody(t *testing.T) {
	t.Parallel()

	huge := strings.Repeat("x", maxBodyInError*3)
	e := NewError("p", 500, ErrServer, "", []byte(huge))

	if len(e.Body) > maxBodyInError+len("… (truncated)") {
		t.Errorf("Body length = %d, want it bounded near %d", len(e.Body), maxBodyInError)
	}
	if !strings.HasSuffix(e.Body, "(truncated)") {
		t.Error("a truncated body must say so")
	}

	small := NewError("p", 500, ErrServer, "", []byte("tiny"))
	if small.Body != "tiny" {
		t.Errorf("Body = %q, want it kept verbatim when small", small.Body)
	}
}

func TestUnsupportedf(t *testing.T) {
	t.Parallel()

	err := Unsupportedf("gemini", "cannot represent %s", "image URLs")

	if !errors.Is(err, ErrUnsupported) {
		t.Error("Unsupportedf did not classify as ErrUnsupported")
	}
	if !strings.Contains(err.Error(), "image URLs") {
		t.Errorf("Error() = %q, want it to name what was unsupported", err.Error())
	}
}
