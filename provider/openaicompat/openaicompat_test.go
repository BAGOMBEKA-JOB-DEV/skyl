// Tests for the generic OpenAI-compatible adapter's own wiring.
//
// The shared contract suite in provider/contract_test.go covers the wire
// mapping. This file covers what only this package decides: its defaults, its
// options, and the two behaviours the long tail of hosts actually depends on —
// running with no credential at all (Ollama, LM Studio, llama.cpp) and adding
// per-host headers (OpenRouter's attribution).
package openaicompat_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/BAGOMBEKA-JOB-DEV/skyl"
	"github.com/BAGOMBEKA-JOB-DEV/skyl/provider/openaicompat"
)

type capture struct {
	path    string
	headers http.Header
	body    map[string]any
}

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
			"id": "cmpl-1",
			"model": "llama3.3",
			"choices": [{"message": {"role": "assistant", "content": "ok"},
			             "finish_reason": "stop"}],
			"usage": {"prompt_tokens": 1, "completion_tokens": 1}
		}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func complete(t *testing.T, p *openaicompat.Provider) {
	t.Helper()
	_, err := p.Complete(context.Background(), &skyl.Request{
		Model:     "llama3.3",
		MaxTokens: 64,
		Messages:  []skyl.Message{skyl.UserText("hi")},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
}

// TestNewPanicsWithoutBaseURL pins the documented contract. A provider with no
// host is a programmer error caught at construction, not a confusing dial
// failure on the first request.
func TestNewPanicsWithoutBaseURL(t *testing.T) {
	t.Parallel()

	defer func() {
		if recover() == nil {
			t.Error("New without WithBaseURL did not panic")
		}
	}()
	_ = openaicompat.New(openaicompat.WithAPIKey("k"))
}

func TestDefaultName(t *testing.T) {
	t.Parallel()

	p := openaicompat.New(openaicompat.WithBaseURL("http://example.invalid/v1"))
	if got := p.Name(); got != "openai-compatible" {
		t.Errorf("Name() = %q, want openai-compatible", got)
	}
}

func TestWithName(t *testing.T) {
	t.Parallel()

	p := openaicompat.New(
		openaicompat.WithBaseURL("http://example.invalid/v1"),
		openaicompat.WithName("groq"),
	)
	if got := p.Name(); got != "groq" {
		t.Errorf("Name() = %q, want groq", got)
	}
}

// TestMaxTokensFieldDefault pins the default that differs from provider/openai.
// Compatible hosts overwhelmingly expect the older field name, so the two
// adapters must not share a default.
func TestMaxTokensFieldDefault(t *testing.T) {
	t.Parallel()

	var got capture
	srv := newServer(t, &got)
	complete(t, openaicompat.New(openaicompat.WithBaseURL(srv.URL)))

	if _, ok := got.body["max_tokens"]; !ok {
		t.Errorf("expected max_tokens in payload, got keys %v", keys(got.body))
	}
	if _, ok := got.body["max_completion_tokens"]; ok {
		t.Error("payload carried max_completion_tokens, which most compatible hosts reject")
	}
}

func TestWithMaxTokensField(t *testing.T) {
	t.Parallel()

	var got capture
	srv := newServer(t, &got)
	complete(t, openaicompat.New(
		openaicompat.WithBaseURL(srv.URL),
		openaicompat.WithMaxTokensField("max_completion_tokens"),
	))

	if _, ok := got.body["max_completion_tokens"]; !ok {
		t.Errorf("expected max_completion_tokens after override, got keys %v", keys(got.body))
	}
}

// TestNoAPIKeySendsNoAuthorization is the local-runtime case. Ollama and
// llama.cpp need no credential, and some reject a bearer header outright — so
// an unset key must send no Authorization header rather than an empty one.
func TestNoAPIKeySendsNoAuthorization(t *testing.T) {
	t.Parallel()

	var got capture
	srv := newServer(t, &got)
	complete(t, openaicompat.New(openaicompat.WithBaseURL(srv.URL)))

	if _, ok := got.headers["Authorization"]; ok {
		t.Errorf("sent Authorization %q with no API key configured",
			got.headers.Get("Authorization"))
	}
}

func TestWithAPIKey(t *testing.T) {
	t.Parallel()

	var got capture
	srv := newServer(t, &got)
	complete(t, openaicompat.New(
		openaicompat.WithBaseURL(srv.URL),
		openaicompat.WithAPIKey("gsk-test"),
	))

	if v := got.headers.Get("Authorization"); v != "Bearer gsk-test" {
		t.Errorf("Authorization = %q, want %q", v, "Bearer gsk-test")
	}
}

// TestWithHeader covers OpenRouter-style attribution, including that repeated
// use accumulates rather than replacing.
func TestWithHeader(t *testing.T) {
	t.Parallel()

	var got capture
	srv := newServer(t, &got)
	complete(t, openaicompat.New(
		openaicompat.WithBaseURL(srv.URL),
		openaicompat.WithHeader("HTTP-Referer", "https://example.com"),
		openaicompat.WithHeader("X-Title", "skyl"),
	))

	if v := got.headers.Get("HTTP-Referer"); v != "https://example.com" {
		t.Errorf("HTTP-Referer = %q", v)
	}
	if v := got.headers.Get("X-Title"); v != "skyl" {
		t.Errorf("X-Title = %q", v)
	}
}

func TestWithHTTPClient(t *testing.T) {
	t.Parallel()

	var got capture
	srv := newServer(t, &got)

	used := false
	hc := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		used = true
		return http.DefaultTransport.RoundTrip(r)
	})}

	complete(t, openaicompat.New(
		openaicompat.WithBaseURL(srv.URL),
		openaicompat.WithHTTPClient(hc),
	))

	if !used {
		t.Error("WithHTTPClient client was not used")
	}
}

// TestBaseURLTrailingSlash guards against the doubled slash that a
// copy-pasted base URL otherwise produces.
func TestBaseURLTrailingSlash(t *testing.T) {
	t.Parallel()

	var got capture
	srv := newServer(t, &got)
	complete(t, openaicompat.New(openaicompat.WithBaseURL(srv.URL+"/")))

	if got.path != "/chat/completions" {
		t.Errorf("path = %q, want /chat/completions", got.path)
	}
}

func TestModels(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			t.Errorf("path = %q, want /models", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data": [{"id": "llama3.3", "name": "Llama 3.3"}]}`)
	}))
	t.Cleanup(srv.Close)

	models, err := openaicompat.New(
		openaicompat.WithBaseURL(srv.URL),
		openaicompat.WithName("ollama"),
	).Models(context.Background())
	if err != nil {
		t.Fatalf("Models: %v", err)
	}

	if len(models) != 1 {
		t.Fatalf("got %d models, want 1", len(models))
	}
	if models[0].ID != "llama3.3" || models[0].DisplayName != "Llama 3.3" {
		t.Errorf("models[0] = %+v", models[0])
	}
	// The configured name must propagate, so metrics can tell hosts apart.
	if models[0].Provider != "ollama" {
		t.Errorf("models[0].Provider = %q, want ollama", models[0].Provider)
	}
}

// TestModelsUnsupported covers hosts that do not implement /models: the caller
// must be able to tell "cannot ask" from "none available".
func TestModelsUnsupported(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error": {"message": "not implemented"}}`)
	}))
	t.Cleanup(srv.Close)

	models, err := openaicompat.New(openaicompat.WithBaseURL(srv.URL)).
		Models(context.Background())
	if err == nil {
		t.Fatalf("expected an error, got %d models", len(models))
	}
	if models != nil {
		t.Errorf("expected nil models alongside the error, got %+v", models)
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
