// Package providertest is a contract suite every skyl adapter must pass.
//
// Adapters translate between skyl's types and a vendor's wire format, and the
// rules they must obey are identical regardless of vendor (docs/rules.md §6):
// always populate Raw, read the model from the response, classify errors onto
// skyl's sentinels, never leak a credential, honour the context, and never
// leak a goroutine when a stream is abandoned.
//
// Writing those assertions once means a new adapter inherits them by filling
// in a [Suite], and a regression in one adapter cannot hide behind another
// adapter's tests.
package providertest

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/BAGOMBEKA-JOB-DEV/skyl"
	"github.com/BAGOMBEKA-JOB-DEV/skyl/internal/testutil"
)

// Suite describes one adapter well enough to exercise the shared contract.
//
// The payload fields are vendor-specific by necessity: the whole point of an
// adapter is that its wire format differs. Everything asserted about them is
// vendor-neutral.
type Suite struct {
	// Name identifies the adapter in test output.
	Name string

	// New builds the adapter pointed at a test server.
	New func(baseURL string) skyl.Provider

	// APIKey is the credential New bakes in, so the suite can assert it never
	// reaches an error message.
	APIKey string

	// SuccessBody is a minimal successful completion in the vendor's format.
	SuccessBody string

	// WantText is the assistant text SuccessBody encodes.
	WantText string

	// WantModel is the model SuccessBody reports, which must differ from the
	// model the suite requests — that is how it proves the adapter reads the
	// model from the response instead of echoing the request.
	WantModel string

	// ErrorBody is an error payload in the vendor's format. Used with a
	// variety of status codes.
	ErrorBody string

	// StreamFrames are the SSE `data:` payloads of a stream whose text
	// deltas concatenate to "ab".
	StreamFrames []string

	// StreamRawSSE is the complete SSE body, written verbatim, for an adapter
	// whose framing is not bare `data:` frames — Anthropic's stream is a
	// sequence of *named* events, so StreamFrames cannot express it.
	//
	// When set it replaces StreamFrames. Its text deltas must still
	// concatenate to "ab", so the same assertions hold for every adapter.
	StreamRawSSE string

	// SkipStream turns off the streaming assertions, for an adapter that does
	// not support streaming.
	//
	// It is not for an adapter whose *framing* is merely unusual: use
	// StreamRawSSE for that. Skipping costs three checks, and one of them —
	// the goroutine-leak check — has no equivalent anywhere else.
	SkipStream bool
}

// Run executes the whole contract against the adapter.
func (s Suite) Run(t *testing.T) {
	t.Helper()

	t.Run(s.Name+"/populates Raw", s.testRaw)
	t.Run(s.Name+"/reports provider name", s.testProviderName)
	t.Run(s.Name+"/reads model from response", s.testModelFromResponse)
	t.Run(s.Name+"/accepts any model id", s.testAnyModelID)
	t.Run(s.Name+"/honours ProviderOptions", s.testProviderOptions)
	t.Run(s.Name+"/classifies errors", s.testErrorClassification)
	t.Run(s.Name+"/never leaks the credential", s.testCredentialNeverLeaks)
	t.Run(s.Name+"/honours context cancellation", s.testContextCancellation)

	if !s.SkipStream {
		t.Run(s.Name+"/streams text", s.testStreamText)
		t.Run(s.Name+"/stream Close is idempotent", s.testStreamCloseIdempotent)
		t.Run(s.Name+"/abandoned stream leaks no goroutine", s.testStreamNoLeak)
	}
}

func (s Suite) request() *skyl.Request {
	return &skyl.Request{
		Model:     "requested-model",
		MaxTokens: 64,
		Messages:  []skyl.Message{skyl.UserText("hello")},
	}
}

// serve starts a test server returning body with status, and returns the
// adapter pointed at it.
func (s Suite) serve(t *testing.T, status int, body string) skyl.Provider {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Real providers always declare JSON, and some SDKs refuse to decode
		// a body without it — so the fake must too, or it is not a fake of
		// anything real.
		w.Header().Set("Content-Type", "application/json")
		if status != http.StatusOK {
			w.WriteHeader(status)
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return s.New(srv.URL)
}

// serveSSE starts a test server streaming the suite's frames.
func (s Suite) serveSSE(t *testing.T) skyl.Provider {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flush := func() {
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
		if s.StreamRawSSE != "" {
			_, _ = w.Write([]byte(s.StreamRawSSE))
			flush()
			return
		}
		for _, frame := range s.StreamFrames {
			_, _ = w.Write([]byte("data: " + frame + "\n\n"))
			flush()
		}
	}))
	t.Cleanup(srv.Close)
	return s.New(srv.URL)
}

// Raw is the caller's escape hatch from skyl's abstraction, so an adapter that
// forgets it silently removes the user's only way to reach an unmodelled
// field (docs/rules.md §6.2).
func (s Suite) testRaw(t *testing.T) {
	resp, err := s.serve(t, http.StatusOK, s.SuccessBody).Complete(context.Background(), s.request())
	if err != nil {
		t.Fatalf("Complete() error = %v", err)
	}
	if len(resp.Raw) == 0 {
		t.Error("Response.Raw is empty; it must always carry the provider's untouched body")
	}
	if got := resp.Text(); got != s.WantText {
		t.Errorf("Text() = %q, want %q", got, s.WantText)
	}
}

func (s Suite) testProviderName(t *testing.T) {
	p := s.serve(t, http.StatusOK, s.SuccessBody)
	resp, err := p.Complete(context.Background(), s.request())
	if err != nil {
		t.Fatalf("Complete() error = %v", err)
	}
	if resp.Provider != p.Name() {
		t.Errorf("Response.Provider = %q, want %q", resp.Provider, p.Name())
	}
}

// Providers can and do serve a different model than the one requested, so the
// response must report what actually ran (docs/rules.md §6.4).
func (s Suite) testModelFromResponse(t *testing.T) {
	if s.WantModel == "" {
		t.Skip("suite does not pin a response model")
	}
	resp, err := s.serve(t, http.StatusOK, s.SuccessBody).Complete(context.Background(), s.request())
	if err != nil {
		t.Fatalf("Complete() error = %v", err)
	}
	if resp.Model != s.WantModel {
		t.Errorf("Response.Model = %q, want %q read from the response, not echoed from the request",
			resp.Model, s.WantModel)
	}
}

// ADR-0004: model IDs are opaque. An adapter that validates them would block a
// user from a model released after this build.
func (s Suite) testAnyModelID(t *testing.T) {
	p := s.serve(t, http.StatusOK, s.SuccessBody)

	for _, model := range []string{
		"a-model-released-tomorrow",
		"llama3.3:70b-instruct-q4_K_M",
		"accounts/fireworks/models/deepseek-v3",
	} {
		req := s.request()
		req.Model = model
		if _, err := p.Complete(context.Background(), req); err != nil {
			t.Errorf("Complete() rejected model %q: %v", model, err)
		}
	}
}

// docs/rules.md §6.5: ProviderOptions is the caller's escape hatch, and it is
// only an escape hatch if it actually reaches the wire. An adapter that builds
// a typed request struct rather than a map can forget it silently — which is
// exactly what provider/anthropic did — so the check belongs here, where every
// adapter runs it, rather than in one adapter's own tests.
func (s Suite) testProviderOptions(t *testing.T) {
	var captured map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &captured)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(s.SuccessBody))
	}))
	t.Cleanup(srv.Close)

	req := s.request()
	// A key no adapter models, so reaching the payload proves passthrough
	// rather than coincidence.
	req.ProviderOptions = map[string]any{"skyl_test_passthrough": "reached"}

	if _, err := s.New(srv.URL).Complete(context.Background(), req); err != nil {
		t.Fatalf("Complete() error = %v", err)
	}
	if captured == nil {
		t.Fatal("the request body could not be decoded")
	}
	if got := captured["skyl_test_passthrough"]; got != "reached" {
		t.Errorf("ProviderOptions did not reach the payload (got %v); "+
			"docs/rules.md §6.5 requires every adapter to honour it", got)
	}
}

func (s Suite) testErrorClassification(t *testing.T) {
	tests := []struct {
		status int
		want   error
	}{
		{http.StatusUnauthorized, skyl.ErrAuth},
		{http.StatusForbidden, skyl.ErrAuth},
		{http.StatusNotFound, skyl.ErrNotFound},
		{http.StatusTooManyRequests, skyl.ErrRateLimit},
		{http.StatusBadRequest, skyl.ErrBadRequest},
		{http.StatusInternalServerError, skyl.ErrServer},
		{http.StatusServiceUnavailable, skyl.ErrServer},
	}

	for _, tc := range tests {
		p := s.serve(t, tc.status, s.ErrorBody)
		_, err := p.Complete(context.Background(), s.request())
		if err == nil {
			t.Errorf("status %d: Complete() error = nil, want a failure", tc.status)
			continue
		}
		if !errors.Is(err, tc.want) {
			t.Errorf("status %d: err = %v, want it to classify as %v", tc.status, err, tc.want)
		}

		var e *skyl.Error
		if !errors.As(err, &e) {
			t.Errorf("status %d: errors.As did not recover *skyl.Error", tc.status)
			continue
		}
		if e.StatusCode != tc.status {
			t.Errorf("status %d: Error.StatusCode = %d", tc.status, e.StatusCode)
		}
	}
}

// docs/rules.md §7.2: a credential must never reach an error string, or it
// ends up in the logs of anyone who prints the error.
func (s Suite) testCredentialNeverLeaks(t *testing.T) {
	if s.APIKey == "" {
		t.Skip("suite has no credential to check")
	}

	for _, status := range []int{
		http.StatusUnauthorized,
		http.StatusTooManyRequests,
		http.StatusInternalServerError,
	} {
		_, err := s.serve(t, status, s.ErrorBody).Complete(context.Background(), s.request())
		if err == nil {
			continue
		}
		if strings.Contains(err.Error(), s.APIKey) {
			t.Errorf("status %d: the API key appears in the error message", status)
		}
		var e *skyl.Error
		if errors.As(err, &e) && strings.Contains(e.Body, s.APIKey) {
			t.Errorf("status %d: the API key appears in Error.Body", status)
		}
	}
}

// A hung provider must not be able to outlive the caller's context, or it
// exhausts the caller's resources (docs/rules.md §7.4).
func (s Suite) testContextCancellation(t *testing.T) {
	blocked := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-blocked:
		}
	}))
	t.Cleanup(func() { close(blocked); srv.Close() })

	p := s.New(srv.URL)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := p.Complete(ctx, s.request()); done <- err }()

	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Error("Complete() returned nil after the context was cancelled")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Complete() ignored context cancellation and is still running")
	}
}

func (s Suite) testStreamText(t *testing.T) {
	stream, err := s.serveSSE(t).Stream(context.Background(), s.request())
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	defer stream.Close() //nolint:errcheck // test cleanup

	var (
		text strings.Builder
		last skyl.StreamEvent
	)
	for stream.Next() {
		ev := stream.Event()
		last = ev
		if ev.Type == skyl.EventTextDelta {
			text.WriteString(ev.Text)
		}
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("Err() = %v, want nil", err)
	}
	if text.String() != "ab" {
		t.Errorf("streamed text = %q, want %q", text.String(), "ab")
	}
	// Every stream must end with a terminal event, so a caller accumulating
	// usage has somewhere to read it.
	if last.Type != skyl.EventDone {
		t.Errorf("final event = %q, want %q", last.Type, skyl.EventDone)
	}
}

func (s Suite) testStreamCloseIdempotent(t *testing.T) {
	stream, err := s.serveSSE(t).Stream(context.Background(), s.request())
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	if err := stream.Close(); err != nil {
		t.Errorf("first Close() = %v, want nil", err)
	}
	// Callers legitimately Close from a defer and again on an error path.
	if err := stream.Close(); err != nil {
		t.Errorf("second Close() = %v, want nil — Close must be idempotent", err)
	}
}

// A stream that leaks its reader leaks one goroutine per request. That never
// shows up in a unit test's wall clock; it surfaces as unbounded memory growth
// in production.
func (s Suite) testStreamNoLeak(t *testing.T) {
	p := s.serveSSE(t)

	// Warm up before taking the baseline. The test server's accept loop and
	// the HTTP client's persistent-connection goroutines are created on first
	// use and torn down by t.Cleanup — which runs *after* the deferred check
	// below. Counting them in the baseline is what keeps this measuring skyl
	// rather than net/http.
	warm, err := p.Stream(context.Background(), s.request())
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	for warm.Next() { //nolint:revive // draining is the point
	}
	if err := warm.Close(); err != nil {
		t.Fatalf("Close() = %v", err)
	}

	defer testutil.CheckNoGoroutineLeaks(t)()

	for range 5 {
		stream, err := p.Stream(context.Background(), s.request())
		if err != nil {
			t.Fatalf("Stream() error = %v", err)
		}
		// Abandon after a single event, as a caller returning early would.
		stream.Next()
		if err := stream.Close(); err != nil {
			t.Errorf("Close() = %v", err)
		}
	}
}
