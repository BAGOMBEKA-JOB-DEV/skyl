// Tests for the OpenAI adapter's own wiring.
//
// The adapter is a thin layer over internal/oai, and the shared contract suite
// in provider/contract_test.go already covers the request/response mapping they
// have in common. What is *not* shared is exactly what lives here: the defaults
// and options this package chooses. A thin layer is precisely where a wrong
// constant hides — swap "max_completion_tokens" for "max_tokens" and every
// mapping test still passes while every real call fails.
//
// So these tests assert against the bytes on the wire, not against struct
// fields: a test that reads back the config it just set proves nothing.
package openai_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/BAGOMBEKA-JOB-DEV/skyl"
	"github.com/BAGOMBEKA-JOB-DEV/skyl/provider/openai"
)

// capture records the request an adapter actually sent.
type capture struct {
	path    string
	headers http.Header
	body    map[string]any
}

// newServer returns a server that records one request and replies with a
// minimal valid completion.
//
// The Content-Type matters: real providers always send JSON, and a fake that
// omits it tests a situation that cannot occur.
func newServer(t *testing.T, got *capture) *httptest.Server {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.path = r.URL.Path
		got.headers = r.Header.Clone()

		if raw, err := io.ReadAll(r.Body); err == nil && len(raw) > 0 {
			if err := json.Unmarshal(raw, &got.body); err != nil {
				t.Errorf("request body is not valid JSON: %v", err)
			}
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"id": "chatcmpl-1",
			"model": "gpt-5.6",
			"choices": [{"message": {"role": "assistant", "content": "ok"},
			             "finish_reason": "stop"}],
			"usage": {"prompt_tokens": 1, "completion_tokens": 1}
		}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func complete(t *testing.T, p *openai.Provider) {
	t.Helper()
	_, err := p.Complete(context.Background(), &skyl.Request{
		Model:     "gpt-5.6",
		MaxTokens: 64,
		Messages:  []skyl.Message{skyl.UserText("hi")},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
}

// TestMaxTokensFieldDefault pins the single most consequential default in this
// package. OpenAI's current models reject "max_tokens" outright, so getting
// this wrong breaks every request that sets a cap.
func TestMaxTokensFieldDefault(t *testing.T) {
	t.Parallel()

	var got capture
	srv := newServer(t, &got)
	complete(t, openai.New("sk-test", openai.WithBaseURL(srv.URL)))

	if _, ok := got.body["max_completion_tokens"]; !ok {
		t.Errorf("expected max_completion_tokens in payload, got keys %v", keys(got.body))
	}
	if _, ok := got.body["max_tokens"]; ok {
		t.Error("payload carried max_tokens, which current OpenAI models reject")
	}
}

// TestWithMaxTokensField covers the escape hatch for older deployments.
func TestWithMaxTokensField(t *testing.T) {
	t.Parallel()

	var got capture
	srv := newServer(t, &got)
	complete(t, openai.New("sk-test",
		openai.WithBaseURL(srv.URL),
		openai.WithMaxTokensField("max_tokens"),
	))

	if _, ok := got.body["max_tokens"]; !ok {
		t.Errorf("expected max_tokens after override, got keys %v", keys(got.body))
	}
	if _, ok := got.body["max_completion_tokens"]; ok {
		t.Error("override left the default field in place")
	}
}

func TestHeaderOptions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		opt    openai.Option
		header string
		want   string
	}{
		{"organization", openai.WithOrganization("org-abc"), "Openai-Organization", "org-abc"},
		{"project", openai.WithProject("proj-xyz"), "Openai-Project", "proj-xyz"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var got capture
			srv := newServer(t, &got)
			complete(t, openai.New("sk-test", openai.WithBaseURL(srv.URL), tt.opt))

			if v := got.headers.Get(tt.header); v != tt.want {
				t.Errorf("header %s = %q, want %q", tt.header, v, tt.want)
			}
		})
	}
}

// TestHeaderOptionsSkipEmpty covers the empty-value branch. Sending
// "OpenAI-Organization: " is not the same as not sending it — some accounts
// reject the empty value — so an unset option must add nothing at all.
func TestHeaderOptionsSkipEmpty(t *testing.T) {
	t.Parallel()

	var got capture
	srv := newServer(t, &got)
	complete(t, openai.New("sk-test",
		openai.WithBaseURL(srv.URL),
		openai.WithOrganization(""),
		openai.WithProject(""),
	))

	for _, h := range []string{"Openai-Organization", "Openai-Project"} {
		if _, ok := got.headers[h]; ok {
			t.Errorf("empty option still sent header %s", h)
		}
	}
}

// TestWithHTTPClient proves the supplied client is the one used — the point of
// the option is custom transports, so a silently ignored client would defeat
// proxying, instrumentation, and private trust stores alike.
func TestWithHTTPClient(t *testing.T) {
	t.Parallel()

	var got capture
	srv := newServer(t, &got)

	used := false
	hc := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		used = true
		return http.DefaultTransport.RoundTrip(r)
	})}

	complete(t, openai.New("sk-test", openai.WithBaseURL(srv.URL), openai.WithHTTPClient(hc)))

	if !used {
		t.Error("WithHTTPClient client was not used")
	}
}

func TestAuthorizationAndDefaults(t *testing.T) {
	t.Parallel()

	var got capture
	srv := newServer(t, &got)
	p := openai.New("sk-test", openai.WithBaseURL(srv.URL))

	if name := p.Name(); name != "openai" {
		t.Errorf("Name() = %q, want %q", name, "openai")
	}
	complete(t, p)

	if v := got.headers.Get("Authorization"); v != "Bearer sk-test" {
		t.Errorf("Authorization = %q, want %q", v, "Bearer sk-test")
	}
	if got.path != "/chat/completions" {
		t.Errorf("path = %q, want /chat/completions", got.path)
	}
}

// TestDefaultBaseURL guards the constant without making a network call.
func TestDefaultBaseURL(t *testing.T) {
	t.Parallel()

	if openai.DefaultBaseURL != "https://api.openai.com/v1" {
		t.Errorf("DefaultBaseURL = %q", openai.DefaultBaseURL)
	}
}

func TestModels(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			t.Errorf("path = %q, want /models", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data": [
			{"id": "gpt-5.6", "context_length": 400000},
			{"id": "gpt-5.4-nano"},
			{"no_id": true}
		]}`)
	}))
	t.Cleanup(srv.Close)

	models, err := openai.New("sk-test", openai.WithBaseURL(srv.URL)).
		Models(context.Background())
	if err != nil {
		t.Fatalf("Models: %v", err)
	}

	// The entry without an id is skipped rather than surfaced as a blank model.
	if len(models) != 2 {
		t.Fatalf("got %d models, want 2: %+v", len(models), models)
	}
	if models[0].ID != "gpt-5.6" || models[0].ContextWindow != 400000 {
		t.Errorf("models[0] = %+v", models[0])
	}
	if models[0].Provider != "openai" {
		t.Errorf("models[0].Provider = %q, want openai", models[0].Provider)
	}
}

// TestModelsError checks the failure path is classified rather than swallowed.
func TestModelsError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error": {"message": "bad key"}}`)
	}))
	t.Cleanup(srv.Close)

	_, err := openai.New("sk-test", openai.WithBaseURL(srv.URL)).Models(context.Background())
	if err == nil {
		t.Fatal("expected an error")
	}

	var e *skyl.Error
	if !errors.As(err, &e) {
		t.Fatalf("error is not *skyl.Error: %T", err)
	}
	if e.Provider != "openai" {
		t.Errorf("Provider = %q, want openai", e.Provider)
	}
	if e.StatusCode != http.StatusUnauthorized {
		t.Errorf("StatusCode = %d, want %d", e.StatusCode, http.StatusUnauthorized)
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func keys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
