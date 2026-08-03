//go:build sandbox

// Client-level behaviour against the sandbox: retry, classification, and
// cancellation, driven over a real socket.
//
// These paths are the reason the sandbox is worth having. Retry and backoff
// are wired between skyl.Client and an adapter, so an in-process fake that
// returns a Go error skips most of what actually happens — the status code,
// the Retry-After header, the connection reuse. Injecting a status here
// exercises the whole chain.
package provider_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/BAGOMBEKA-JOB-DEV/skyl"
	"github.com/BAGOMBEKA-JOB-DEV/skyl/internal/sandbox"
	"github.com/BAGOMBEKA-JOB-DEV/skyl/provider/openai"

	"net/http/httptest"
)

// newCountingSandbox returns the sandbox handler alongside its URL, so a test
// can assert how many requests actually reached the wire.
func newCountingSandbox(t *testing.T) (*sandbox.Handler, string) {
	t.Helper()

	h := sandbox.New()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return h, srv.URL
}

func sandboxRequest(model string) *skyl.Request {
	return &skyl.Request{
		Model:     model,
		MaxTokens: 64,
		Messages:  []skyl.Message{skyl.UserText("What is the capital of France?")},
	}
}

// TestSandboxRetriesServerErrors checks a 500 is retried the configured number
// of times and then surfaces. The request counter is the assertion: it proves
// the attempts reached the server rather than being satisfied locally.
func TestSandboxRetriesServerErrors(t *testing.T) {
	h, base := newCountingSandbox(t)

	client := skyl.New(
		openai.New(sandbox.DefaultAPIKey, openai.WithBaseURL(base+"/openai/v1")),
		skyl.WithMaxRetries(2),
		skyl.WithRetryDelay(time.Millisecond, 10*time.Millisecond),
	)

	_, err := client.Complete(t.Context(), sandboxRequest(sandbox.StatusModelPrefix+"500"))
	if err == nil {
		t.Fatal("expected an error from an injected 500")
	}
	if !errors.Is(err, skyl.ErrServer) {
		t.Errorf("err = %v, want ErrServer", err)
	}

	// One initial attempt plus two retries.
	if got := h.Requests(); got != 3 {
		t.Errorf("server saw %d requests, want 3 (1 attempt + 2 retries)", got)
	}
}

// TestSandboxDoesNotRetryClientErrors is the other half of the policy: a 400
// is the same answer every time, so retrying it only spends quota.
func TestSandboxDoesNotRetryClientErrors(t *testing.T) {
	h, base := newCountingSandbox(t)

	client := skyl.New(
		openai.New(sandbox.DefaultAPIKey, openai.WithBaseURL(base+"/openai/v1")),
		skyl.WithMaxRetries(3),
		skyl.WithRetryDelay(time.Millisecond, 10*time.Millisecond),
	)

	_, err := client.Complete(t.Context(), sandboxRequest(sandbox.StatusModelPrefix+"400"))
	if err == nil {
		t.Fatal("expected an error from an injected 400")
	}
	if !errors.Is(err, skyl.ErrBadRequest) {
		t.Errorf("err = %v, want ErrBadRequest", err)
	}
	if got := h.Requests(); got != 1 {
		t.Errorf("server saw %d requests, want 1 — a 400 must not be retried", got)
	}
}

// TestSandboxDoesNotRetryAuthErrors guards the most expensive mistake in the
// list: hammering a provider with a credential that will never work.
func TestSandboxDoesNotRetryAuthErrors(t *testing.T) {
	h, base := newCountingSandbox(t)

	client := skyl.New(
		openai.New("wrong-key", openai.WithBaseURL(base+"/openai/v1")),
		skyl.WithMaxRetries(3),
		skyl.WithRetryDelay(time.Millisecond, 10*time.Millisecond),
	)

	_, err := client.Complete(t.Context(), sandboxRequest("gpt-5.6"))
	if err == nil {
		t.Fatal("expected an auth error")
	}
	if !errors.Is(err, skyl.ErrAuth) {
		t.Errorf("err = %v, want ErrAuth", err)
	}
	if got := h.Requests(); got != 1 {
		t.Errorf("server saw %d requests, want 1 — an auth failure must not be retried", got)
	}
}

// TestSandboxParsesRetryAfter checks the provider's own hint survives the
// round trip, since backoff prefers it when it is longer than the computed
// delay. Retries are disabled so the first error is the one observed.
func TestSandboxParsesRetryAfter(t *testing.T) {
	_, base := newCountingSandbox(t)

	p := openai.New(sandbox.DefaultAPIKey, openai.WithBaseURL(base+"/openai/v1"))

	_, err := p.Complete(t.Context(), sandboxRequest(sandbox.StatusModelPrefix+"429"))
	if err == nil {
		t.Fatal("expected an error from an injected 429")
	}
	if !errors.Is(err, skyl.ErrRateLimit) {
		t.Fatalf("err = %v, want ErrRateLimit", err)
	}

	var e *skyl.Error
	if !errors.As(err, &e) {
		t.Fatalf("err is not *skyl.Error: %T", err)
	}
	if e.RetryAfter != time.Second {
		t.Errorf("RetryAfter = %v, want 1s from the header", e.RetryAfter)
	}
}

// TestSandboxCancellationStopsRetrying checks a cancelled context ends the
// attempt loop rather than burning through the remaining budget.
func TestSandboxCancellationStopsRetrying(t *testing.T) {
	h, base := newCountingSandbox(t)

	client := skyl.New(
		openai.New(sandbox.DefaultAPIKey, openai.WithBaseURL(base+"/openai/v1")),
		skyl.WithMaxRetries(5),
		// Long enough that cancellation lands during the first backoff.
		skyl.WithRetryDelay(2*time.Second, 5*time.Second),
	)

	ctx, cancel := context.WithTimeout(t.Context(), 150*time.Millisecond)
	defer cancel()

	_, err := client.Complete(ctx, sandboxRequest(sandbox.StatusModelPrefix+"503"))
	if err == nil {
		t.Fatal("expected an error")
	}
	// One attempt reached the server; the cancellation stopped the rest.
	if got := h.Requests(); got > 2 {
		t.Errorf("server saw %d requests after cancellation, want at most 2", got)
	}
}

// TestSandboxStreamHandshakeError checks a failed stream handshake surfaces as
// a classified error instead of a stream that yields nothing.
func TestSandboxStreamHandshakeError(t *testing.T) {
	_, base := newCountingSandbox(t)

	p := openai.New(sandbox.DefaultAPIKey, openai.WithBaseURL(base+"/openai/v1"))

	stream, err := p.Stream(t.Context(), sandboxRequest(sandbox.StatusModelPrefix+"429"))
	if err == nil {
		_ = stream.Close()
		t.Fatal("Stream() returned no error on an injected 429")
	}
	if !errors.Is(err, skyl.ErrRateLimit) {
		t.Errorf("err = %v, want ErrRateLimit", err)
	}
}
