package providertest

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/BAGOMBEKA-JOB-DEV/skyl"
)

// liveTimeout bounds a single live call. Generous, because reasoning models
// legitimately take a while — but bounded, so a hung provider fails the test
// rather than the whole suite.
const liveTimeout = 90 * time.Second

// Live exercises an adapter against a provider's real API.
//
// This is the check that unit tests structurally cannot perform. Every adapter
// test in this repository replays payloads written from provider
// documentation, so it proves the mapping logic is self-consistent — but a
// fake echoes our own assumptions back at us. If a field name is wrong, the
// fake is wrong in exactly the same way and the test still passes. Only a real
// call can catch that.
//
// Live tests are build-tagged `integration` and never run in default CI: they
// cost money and need real credentials.
type Live struct {
	// Name identifies the adapter in test output.
	Name string

	// EnvKey is the environment variable holding the credential. The test
	// skips when it is unset, so a partial key set still runs what it can.
	EnvKey string

	// New builds the adapter from the credential.
	New func(apiKey string) skyl.Provider

	// Model is the model to call. Keep it small and cheap.
	Model string
}

// Run executes the live checks, skipping if the credential is absent.
func (l Live) Run(t *testing.T) {
	t.Helper()

	key := os.Getenv(l.EnvKey)
	if key == "" {
		t.Skipf("%s not set; skipping live %s checks", l.EnvKey, l.Name)
	}

	p := l.New(key)

	t.Run(l.Name+"/models", func(t *testing.T) { l.testModels(t, p) })
	t.Run(l.Name+"/complete", func(t *testing.T) { l.testComplete(t, p) })
	t.Run(l.Name+"/stream", func(t *testing.T) { l.testStream(t, p) })
	t.Run(l.Name+"/rejects a bogus model", func(t *testing.T) { l.testBogusModel(t, p) })
}

func ctxWithTimeout(t *testing.T) (context.Context, context.CancelFunc) {
	t.Helper()
	return context.WithTimeout(context.Background(), liveTimeout)
}

// Model discovery is the mechanism skyl offers in place of a hardcoded model
// list (ADR-0004), so it has to actually work against the real endpoint.
func (l Live) testModels(t *testing.T, p skyl.Provider) {
	ctx, cancel := ctxWithTimeout(t)
	defer cancel()

	models, err := p.Models(ctx)
	if errors.Is(err, skyl.ErrUnsupported) {
		t.Skip("provider exposes no models endpoint")
	}
	if err != nil {
		t.Fatalf("Models() error = %v", err)
	}
	if len(models) == 0 {
		t.Error("Models() returned nothing; discovery is how callers find a model ID")
	}
	for _, m := range models {
		if m.ID == "" {
			t.Error("a model came back with an empty ID")
		}
		if m.Provider != p.Name() {
			t.Errorf("model %q reports provider %q, want %q", m.ID, m.Provider, p.Name())
		}
	}
}

func (l Live) testComplete(t *testing.T, p skyl.Provider) {
	ctx, cancel := ctxWithTimeout(t)
	defer cancel()

	// A question with one obvious short answer, so the assertion is about the
	// transport working rather than about model quality.
	resp, err := p.Complete(ctx, &skyl.Request{
		Model:     l.Model,
		System:    "Reply with exactly one word and no punctuation.",
		MaxTokens: 64,
		Messages:  []skyl.Message{skyl.UserText("What is the capital of France?")},
	})
	if err != nil {
		t.Fatalf("Complete() error = %v", err)
	}

	if resp.Text() == "" {
		t.Error("Complete() returned no text")
	}
	if !strings.Contains(strings.ToLower(resp.Text()), "paris") {
		t.Errorf("Text() = %q, want it to contain %q — the request may not be mapping correctly",
			resp.Text(), "paris")
	}
	if len(resp.Raw) == 0 {
		t.Error("Raw is empty on a live response")
	}
	if resp.Model == "" {
		t.Error("Model is empty; it must be read from the response")
	}
	if resp.Usage.InputTokens == 0 && resp.Usage.OutputTokens == 0 {
		t.Error("Usage is entirely zero; token accounting is not being parsed")
	}
	if resp.StopReason == skyl.StopUnknown {
		t.Errorf("StopReason = %q — the provider reported something skyl does not map",
			resp.StopReason)
	}
}

func (l Live) testStream(t *testing.T, p skyl.Provider) {
	ctx, cancel := ctxWithTimeout(t)
	defer cancel()

	stream, err := p.Stream(ctx, &skyl.Request{
		Model:     l.Model,
		System:    "Reply with exactly one word and no punctuation.",
		MaxTokens: 64,
		Messages:  []skyl.Message{skyl.UserText("What is the capital of France?")},
	})
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	defer stream.Close() //nolint:errcheck // test cleanup

	var (
		text   strings.Builder
		deltas int
		sawEnd bool
	)
	for stream.Next() {
		ev := stream.Event()
		switch ev.Type {
		case skyl.EventTextDelta:
			deltas++
			text.WriteString(ev.Text)
		case skyl.EventDone:
			sawEnd = true
		}
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("Err() = %v", err)
	}

	if deltas == 0 {
		t.Error("stream produced no text deltas; SSE parsing is not working against the real API")
	}
	if !strings.Contains(strings.ToLower(text.String()), "paris") {
		t.Errorf("streamed text = %q, want it to contain %q", text.String(), "paris")
	}
	if !sawEnd {
		t.Error("stream never emitted a terminal event, so usage is unreachable")
	}
}

// skyl passes model IDs through unvalidated (ADR-0004), which is only
// defensible if the provider's own not-found error classifies correctly —
// otherwise a typo surfaces as something unactionable.
func (l Live) testBogusModel(t *testing.T, p skyl.Provider) {
	ctx, cancel := ctxWithTimeout(t)
	defer cancel()

	_, err := p.Complete(ctx, &skyl.Request{
		Model:     "skyl-definitely-not-a-real-model",
		MaxTokens: 16,
		Messages:  []skyl.Message{skyl.UserText("hi")},
	})
	if err == nil {
		t.Fatal("Complete() with a bogus model returned no error")
	}
	if !errors.Is(err, skyl.ErrNotFound) && !errors.Is(err, skyl.ErrBadRequest) {
		t.Errorf("err = %v, want ErrNotFound or ErrBadRequest so callers can act on it", err)
	}
}
