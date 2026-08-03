//go:build integration

// Live tests against the real Anthropic API.
//
// Excluded from the default build, and therefore from CI, because they cost
// money and need a real key. Run them yourself:
//
//	export ANTHROPIC_API_KEY=sk-ant-...
//	cd provider/anthropic && go test -tags=integration -v ./...
package anthropic_test

import (
	"os"
	"testing"

	"github.com/BAGOMBEKA-JOB-DEV/skyl"
	"github.com/BAGOMBEKA-JOB-DEV/skyl/internal/providertest"
	"github.com/BAGOMBEKA-JOB-DEV/skyl/provider/anthropic"
)

func TestLiveAnthropic(t *testing.T) {
	model := os.Getenv("SKYL_TEST_ANTHROPIC_MODEL")
	if model == "" {
		model = "claude-haiku-4-5"
	}

	providertest.Live{
		Name:   "anthropic",
		EnvKey: "ANTHROPIC_API_KEY",
		Model:  model,
		New:    func(key string) skyl.Provider { return anthropic.New(key) },
	}.Run(t)
}
