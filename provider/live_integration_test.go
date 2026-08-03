//go:build integration

// Live tests against real provider APIs.
//
// These are excluded from the default build — and therefore from CI — because
// they cost money and need real credentials. Run them yourself:
//
//	export OPENAI_API_KEY=sk-...
//	export GEMINI_API_KEY=...
//	go test -tags=integration -v ./provider/
//
// Each provider skips when its key is unset, so a partial key set still runs
// what it can.
//
// This is the check unit tests structurally cannot perform: every other
// adapter test replays payloads written from provider documentation, so it
// proves the mapping is self-consistent but not that it is *correct*. If a
// field name is wrong, the fake is wrong the same way and stays green.
package provider_test

import (
	"os"
	"testing"

	"github.com/BAGOMBEKA-JOB-DEV/skyl"
	"github.com/BAGOMBEKA-JOB-DEV/skyl/internal/providertest"
	"github.com/BAGOMBEKA-JOB-DEV/skyl/provider/gemini"
	"github.com/BAGOMBEKA-JOB-DEV/skyl/provider/openai"
	"github.com/BAGOMBEKA-JOB-DEV/skyl/provider/openaicompat"
)

func TestLiveOpenAI(t *testing.T) {
	providertest.Live{
		Name:   "openai",
		EnvKey: "OPENAI_API_KEY",
		Model:  envOr("SKYL_TEST_OPENAI_MODEL", "gpt-5.4-nano"),
		New:    func(key string) skyl.Provider { return openai.New(key) },
	}.Run(t)
}

func TestLiveGemini(t *testing.T) {
	providertest.Live{
		Name:   "gemini",
		EnvKey: "GEMINI_API_KEY",
		Model:  envOr("SKYL_TEST_GEMINI_MODEL", "gemini-3.6-flash"),
		New:    func(key string) skyl.Provider { return gemini.New(key) },
	}.Run(t)
}

// TestLiveOpenAICompat points the generic adapter at whatever host you
// configure. Defaults to a local Ollama, which needs no credential — set
// SKYL_TEST_COMPAT_KEY to any non-empty value to run against it:
//
//	SKYL_TEST_COMPAT_KEY=local SKYL_TEST_COMPAT_MODEL=llama3.3 \
//	  go test -tags=integration -run TestLiveOpenAICompat ./provider/
func TestLiveOpenAICompat(t *testing.T) {
	baseURL := envOr("SKYL_TEST_COMPAT_BASE_URL", "http://localhost:11434/v1")

	providertest.Live{
		Name:   "openaicompat",
		EnvKey: "SKYL_TEST_COMPAT_KEY",
		Model:  envOr("SKYL_TEST_COMPAT_MODEL", "llama3.3"),
		New: func(key string) skyl.Provider {
			// The sentinel "local" means "no credential", for runtimes such as
			// Ollama and vLLM that need none.
			if key == "local" {
				key = ""
			}
			return openaicompat.New(
				openaicompat.WithBaseURL(baseURL),
				openaicompat.WithAPIKey(key),
				openaicompat.WithName("compat-live"),
			)
		},
	}.Run(t)
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
