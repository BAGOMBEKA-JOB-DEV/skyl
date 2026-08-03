//go:build sandbox

// Runs the live integration suite against the local sandbox.
//
//	cd provider/anthropic && go test -tags=sandbox ./...
//
// See provider/sandbox_integration_test.go for what this does and does not
// prove. In short: the transport is real, the model is not, and only
// -tags=integration against the real API can confirm the field names.
//
// This adapter is worth running here even though it is built on the official
// SDK — arguably especially so. The SDK does its own decoding, so a response
// it rejects fails loudly rather than silently producing a zero value, which
// makes the sandbox a genuine check on the event sequence being well-formed.
package anthropic_test

import (
	"net/http/httptest"
	"testing"

	"github.com/BAGOMBEKA-JOB-DEV/skyl"
	"github.com/BAGOMBEKA-JOB-DEV/skyl/internal/providertest"
	"github.com/BAGOMBEKA-JOB-DEV/skyl/internal/sandbox"
	"github.com/BAGOMBEKA-JOB-DEV/skyl/provider/anthropic"
)

func TestSandboxAnthropic(t *testing.T) {
	srv := httptest.NewServer(sandbox.New())
	t.Cleanup(srv.Close)

	// The Live harness reads the key from the environment; pointing it at the
	// sandbox credential also rules out hitting the real API by accident.
	t.Setenv("ANTHROPIC_API_KEY", sandbox.DefaultAPIKey)

	providertest.Live{
		Name:   "anthropic",
		EnvKey: "ANTHROPIC_API_KEY",
		Model:  "claude-haiku-4-5",
		New: func(key string) skyl.Provider {
			return anthropic.New(key, anthropic.WithBaseURL(srv.URL+"/anthropic"))
		},
	}.Run(t)
}

// TestSandboxAnthropicDefaultMaxTokens checks the adapter supplies max_tokens
// when the caller omits it. The sandbox rejects a request without one, exactly
// as the real API does, so this fails if the default is ever dropped.
func TestSandboxAnthropicDefaultMaxTokens(t *testing.T) {
	srv := httptest.NewServer(sandbox.New())
	t.Cleanup(srv.Close)

	p := anthropic.New(sandbox.DefaultAPIKey, anthropic.WithBaseURL(srv.URL+"/anthropic"))

	resp, err := p.Complete(t.Context(), &skyl.Request{
		Model:    "claude-haiku-4-5",
		Messages: []skyl.Message{skyl.UserText("What is the capital of France?")},
		// MaxTokens deliberately unset.
	})
	if err != nil {
		t.Fatalf("Complete without MaxTokens: %v", err)
	}
	if resp.Text() != "Paris" {
		t.Errorf("Text() = %q, want %q", resp.Text(), "Paris")
	}
}
