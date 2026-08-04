// Replay of recorded provider exchanges.
//
// These tests are deliberately untagged, so they run in ordinary CI. They skip
// when no recording exists, which is the state today — and they start asserting
// the moment somebody with a provider key runs:
//
//	SKYL_RECORD=1 OPENAI_API_KEY=... go test ./provider/ -run Cassette
//
// That is the whole point. Every other fake in this repository was written from
// the same documentation as the adapter it tests, so a wrong field name leaves
// both green. A cassette holds a *real* provider response, so the same wrong
// field name fails for everyone, forever, with no credential needed to notice.
package provider_test

import (
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/BAGOMBEKA-JOB-DEV/skyl"
	"github.com/BAGOMBEKA-JOB-DEV/skyl/internal/cassette"
	"github.com/BAGOMBEKA-JOB-DEV/skyl/internal/testutil"
	"github.com/BAGOMBEKA-JOB-DEV/skyl/provider/gemini"
	"github.com/BAGOMBEKA-JOB-DEV/skyl/provider/openai"
)

// cassetteCase describes one recorded exchange.
type cassetteCase struct {
	name   string
	envKey string
	model  string
	build  func(hc *http.Client, key string) skyl.Provider
}

var cassetteCases = []cassetteCase{
	{
		name: "openai", envKey: "OPENAI_API_KEY", model: "gpt-5.4-nano",
		build: func(hc *http.Client, key string) skyl.Provider {
			return openai.New(key, openai.WithHTTPClient(hc))
		},
	},
	{
		name: "gemini", envKey: "GEMINI_API_KEY", model: "gemini-3.6-flash",
		build: func(hc *http.Client, key string) skyl.Provider {
			return gemini.New(key, gemini.WithHTTPClient(hc))
		},
	},
}

// TestCassetteComplete replays a recorded completion and checks the adapter
// reads a real provider body correctly.
//
// The assertions are the ones that only a real response can settle: that the
// text came out at all, that usage was parsed from whatever field the provider
// actually uses, and that Raw survived.
func TestCassetteComplete(t *testing.T) {
	for _, tc := range cassetteCases {
		t.Run(tc.name, func(t *testing.T) {
			hc, done := cassette.Client(t, tc.name+"/complete")
			defer done()

			// The key is only needed when recording; replay never sends one.
			key := os.Getenv(tc.envKey)
			if cassette.IsRecording() && key == "" {
				t.Skipf("%s is not set, so there is nothing to record", tc.envKey)
			}

			resp, err := tc.build(hc, key).Complete(testutil.Context(t), &skyl.Request{
				Model:     tc.model,
				MaxTokens: 64,
				Messages:  []skyl.Message{skyl.UserText("What is the capital of France?")},
			})
			if err != nil {
				t.Fatalf("Complete() error = %v", err)
			}

			if !strings.Contains(strings.ToLower(resp.Text()), "paris") {
				t.Errorf("Text() = %q, want it to mention Paris", resp.Text())
			}
			if len(resp.Raw) == 0 {
				t.Error("Raw is empty")
			}
			if resp.Model == "" {
				t.Error("Model is empty; it must be read from the response")
			}
			// A real provider always reports usage. Zero here means the field
			// name skyl reads does not match what arrived.
			if resp.Usage.InputTokens == 0 && resp.Usage.OutputTokens == 0 {
				t.Error("usage is entirely zero; the token fields did not parse")
			}
			if resp.StopReason == skyl.StopUnknown {
				t.Errorf("StopReason = %q; the provider's value did not map", resp.StopReason)
			}
		})
	}
}
