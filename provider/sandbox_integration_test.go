//go:build sandbox

// Runs the live integration suite against the local sandbox.
//
//	go test -tags=sandbox ./provider/
//
// This is the same suite that runs against real provider APIs under
// -tags=integration, driven against a local server instead. It needs no
// credential and costs nothing, so unlike the live suite it can run anywhere —
// including CI.
//
// What that buys, precisely: a real TCP connection, a real net/http round
// trip, real chunked SSE framing arriving in pieces over a socket, and real
// status codes. It exercises the integration harness itself rather than
// leaving it compiled-but-unrun, and it catches transport regressions that
// in-process tests cannot see.
//
// What it does not buy: any evidence that skyl's field names match what a
// provider actually sends. The sandbox was written from the same documentation
// as the adapters, so it agrees with them by construction. Only -tags=integration
// against a real endpoint settles that, and it needs your own key.
package provider_test

import (
	"net/http/httptest"
	"testing"

	"github.com/BAGOMBEKA-JOB-DEV/skyl"
	"github.com/BAGOMBEKA-JOB-DEV/skyl/internal/providertest"
	"github.com/BAGOMBEKA-JOB-DEV/skyl/internal/sandbox"
	"github.com/BAGOMBEKA-JOB-DEV/skyl/internal/testutil"
	"github.com/BAGOMBEKA-JOB-DEV/skyl/provider/gemini"
	"github.com/BAGOMBEKA-JOB-DEV/skyl/provider/openai"
	"github.com/BAGOMBEKA-JOB-DEV/skyl/provider/openaicompat"
)

// newSandbox starts the sandbox on a real listener and returns its URL.
func newSandbox(t *testing.T) string {
	t.Helper()

	srv := httptest.NewServer(sandbox.New())
	t.Cleanup(srv.Close)
	return srv.URL
}

// The Live harness reads the credential from the environment, so each test
// sets the sandbox's key there. t.Setenv restores it afterwards and rules out
// running these against a real provider by accident.
func useSandboxKey(t *testing.T, envKey string) {
	t.Helper()
	t.Setenv(envKey, sandbox.DefaultAPIKey)
}

func TestSandboxOpenAI(t *testing.T) {
	base := newSandbox(t)
	useSandboxKey(t, "OPENAI_API_KEY")

	providertest.Live{
		Name:   "openai",
		EnvKey: "OPENAI_API_KEY",
		Model:  "gpt-5.6",
		New: func(key string) skyl.Provider {
			return openai.New(key, openai.WithBaseURL(base+"/openai/v1"))
		},
	}.Run(t)
}

func TestSandboxGemini(t *testing.T) {
	base := newSandbox(t)
	useSandboxKey(t, "GEMINI_API_KEY")

	providertest.Live{
		Name:   "gemini",
		EnvKey: "GEMINI_API_KEY",
		Model:  "gemini-3.6-flash",
		New: func(key string) skyl.Provider {
			return gemini.New(key, gemini.WithBaseURL(base+"/gemini/v1beta"))
		},
	}.Run(t)
}

func TestSandboxOpenAICompat(t *testing.T) {
	base := newSandbox(t)
	useSandboxKey(t, "SKYL_TEST_COMPAT_KEY")

	providertest.Live{
		Name:   "openaicompat",
		EnvKey: "SKYL_TEST_COMPAT_KEY",
		Model:  "gpt-5.6",
		New: func(key string) skyl.Provider {
			return openaicompat.New(
				openaicompat.WithBaseURL(base+"/compat/v1"),
				openaicompat.WithAPIKey(key),
				openaicompat.WithName("sandbox-compat"),
			)
		},
	}.Run(t)
}

// TestSandboxNoCredential covers the local-runtime case the compat adapter
// exists for: Ollama, LM Studio, and llama.cpp need no key, and the sandbox
// accepts none so that path is actually executed rather than assumed.
func TestSandboxNoCredential(t *testing.T) {
	base := newSandbox(t)

	p := openaicompat.New(
		openaicompat.WithBaseURL(base+"/compat/v1"),
		openaicompat.WithName("local-runtime"),
	)

	resp, err := p.Complete(testutil.Context(t), &skyl.Request{
		Model:     "gpt-5.6",
		MaxTokens: 64,
		Messages:  []skyl.Message{skyl.UserText("What is the capital of France?")},
	})
	if err != nil {
		t.Fatalf("Complete without a credential: %v", err)
	}
	if resp.Text() != "Paris" {
		t.Errorf("Text() = %q, want %q", resp.Text(), "Paris")
	}
}
