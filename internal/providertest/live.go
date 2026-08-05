package providertest

import (
	"context"
	"encoding/json"
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
	t.Run(l.Name+"/tool call", func(t *testing.T) { l.testToolCall(t, p) })
	t.Run(l.Name+"/tool round trip", func(t *testing.T) { l.testToolRoundTrip(t, p) })
	t.Run(l.Name+"/streamed tool call", func(t *testing.T) { l.testStreamingToolCall(t, p) })
	t.Run(l.Name+"/stops at max tokens", func(t *testing.T) { l.testMaxTokensStop(t, p) })
	t.Run(l.Name+"/structured output", func(t *testing.T) { l.testStructuredOutput(t, p) })
}

// weatherTool is the fixture for the tool-calling checks. A single required
// string parameter keeps the schema trivial, so a failure means the tool
// *mapping* is wrong rather than that the model was confused by the schema.
func weatherTool() skyl.Tool {
	return skyl.Tool{
		Name:        "get_weather",
		Description: "Get the current weather for a city. Call this whenever the user asks about weather.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"city": map[string]any{
					"type":        "string",
					"description": "The city name, e.g. Paris",
				},
			},
			"required": []string{"city"},
		},
	}
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

// Tool calling is the least verifiable path offline: the request carries a
// JSON Schema the provider validates, and the reply carries an ID format and an
// argument encoding that no fake can confirm. A schema our fake accepts and the
// provider rejects is exactly the class of defect this suite exists to find.
func (l Live) testToolCall(t *testing.T, p skyl.Provider) {
	ctx, cancel := ctxWithTimeout(t)
	defer cancel()

	resp, err := p.Complete(ctx, &skyl.Request{
		Model:      l.Model,
		MaxTokens:  256,
		Messages:   []skyl.Message{skyl.UserText("What is the weather in Paris?")},
		Tools:      []skyl.Tool{weatherTool()},
		ToolChoice: &skyl.ToolChoice{Mode: skyl.ToolChoiceRequired},
	})
	if err != nil {
		t.Fatalf("Complete() with a tool = %v", err)
	}

	calls := resp.ToolCalls()
	if len(calls) == 0 {
		t.Fatalf("no tool call returned despite ToolChoiceRequired; StopReason=%q text=%q",
			resp.StopReason, resp.Text())
	}
	call := calls[0]

	if call.ID == "" {
		t.Error("ToolCall.ID is empty; without it the result cannot be correlated back")
	}
	if call.Name != "get_weather" {
		t.Errorf("ToolCall.Name = %q, want %q", call.Name, "get_weather")
	}

	// The arguments must be a JSON object matching the declared schema. skyl
	// never validates this (by design), so a live check is the only place the
	// round trip is proven to produce usable JSON.
	var args struct {
		City string `json:"city"`
	}
	if err := json.Unmarshal(call.Arguments, &args); err != nil {
		t.Fatalf("ToolCall.Arguments is not valid JSON: %v (raw: %s)", err, call.Arguments)
	}
	if args.City == "" {
		t.Errorf("tool arguments carry no city: %s", call.Arguments)
	}
	if resp.StopReason != skyl.StopToolUse {
		t.Errorf("StopReason = %q, want %q — stop-reason mapping is per-provider and easy to get wrong",
			resp.StopReason, skyl.StopToolUse)
	}
}

// The multi-turn half. Sending a tool *result* back is where message ordering,
// role naming and the tool_result encoding all have to be right at once, and
// where the adapters differ most from each other.
func (l Live) testToolRoundTrip(t *testing.T, p skyl.Provider) {
	ctx, cancel := ctxWithTimeout(t)
	defer cancel()

	first, err := p.Complete(ctx, &skyl.Request{
		Model:      l.Model,
		MaxTokens:  256,
		Messages:   []skyl.Message{skyl.UserText("What is the weather in Paris?")},
		Tools:      []skyl.Tool{weatherTool()},
		ToolChoice: &skyl.ToolChoice{Mode: skyl.ToolChoiceRequired},
	})
	if err != nil {
		t.Fatalf("first Complete() = %v", err)
	}
	calls := first.ToolCalls()
	if len(calls) == 0 {
		t.Skip("provider returned no tool call; the round trip has nothing to answer")
	}

	// Replay the assistant turn verbatim, then answer it. Replaying the
	// provider's own message is the point: a lossy Message round trip shows up
	// here as a 400 and nowhere else.
	second, err := p.Complete(ctx, &skyl.Request{
		Model:     l.Model,
		MaxTokens: 256,
		Messages: []skyl.Message{
			skyl.UserText("What is the weather in Paris?"),
			first.Message,
			skyl.ToolResultMessage(calls[0].ID, `{"temp_c": 18, "condition": "cloudy"}`),
		},
		Tools: []skyl.Tool{weatherTool()},
	})
	if err != nil {
		t.Fatalf("second Complete() with a tool result = %v", err)
	}
	if second.Text() == "" {
		t.Error("no text after the tool result; the model was given the answer and said nothing")
	}
	if !strings.Contains(second.Text(), "18") && !strings.Contains(strings.ToLower(second.Text()), "cloud") {
		t.Errorf("reply = %q, want it to reflect the tool result — the result may not have reached the model",
			second.Text())
	}
}

// Streaming tool calls arrive as argument fragments that must be accumulated
// before the JSON is valid. The accumulation is unit-tested against our own
// fragmentation; only a real provider fragments the way a real provider does.
func (l Live) testStreamingToolCall(t *testing.T, p skyl.Provider) {
	ctx, cancel := ctxWithTimeout(t)
	defer cancel()

	stream, err := p.Stream(ctx, &skyl.Request{
		Model:      l.Model,
		MaxTokens:  256,
		Messages:   []skyl.Message{skyl.UserText("What is the weather in Paris?")},
		Tools:      []skyl.Tool{weatherTool()},
		ToolChoice: &skyl.ToolChoice{Mode: skyl.ToolChoiceRequired},
	})
	if err != nil {
		t.Fatalf("Stream() with a tool = %v", err)
	}
	defer stream.Close() //nolint:errcheck // test cleanup

	var calls []skyl.ToolCall
	for stream.Next() {
		if ev := stream.Event(); ev.Type == skyl.EventToolCall && ev.ToolCall != nil {
			calls = append(calls, *ev.ToolCall)
		}
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("Err() = %v", err)
	}
	if len(calls) == 0 {
		t.Fatal("stream produced no tool call despite ToolChoiceRequired")
	}
	for _, c := range calls {
		if !json.Valid(c.Arguments) {
			t.Errorf("streamed tool %q has invalid JSON arguments: %s — fragments did not reassemble",
				c.Name, c.Arguments)
		}
	}
}

// Stop reasons are provider-specific strings mapped to skyl's own set, and a
// truncation that reports StopEndTurn is indistinguishable from a complete
// answer. Forcing the truncation is the only way to prove the mapping.
func (l Live) testMaxTokensStop(t *testing.T, p skyl.Provider) {
	ctx, cancel := ctxWithTimeout(t)
	defer cancel()

	resp, err := p.Complete(ctx, &skyl.Request{
		Model:     l.Model,
		MaxTokens: 8,
		Messages:  []skyl.Message{skyl.UserText("Count slowly from one to one hundred in words.")},
	})
	if err != nil {
		t.Fatalf("Complete() = %v", err)
	}
	if resp.StopReason != skyl.StopMaxTokens {
		t.Errorf("StopReason = %q, want %q — a truncated answer reported as complete is silent data loss",
			resp.StopReason, skyl.StopMaxTokens)
	}

	// The inclusion rule documented on skyl.Usage. Every adapter normalises to
	// it, and no fake can prove the provider's own numbers obey it.
	u := resp.Usage
	if u.CacheReadTokens > u.InputTokens {
		t.Errorf("CacheReadTokens %d exceeds InputTokens %d; cache figures must be a breakdown of input, not an addition",
			u.CacheReadTokens, u.InputTokens)
	}
	if u.CacheWriteTokens > u.InputTokens {
		t.Errorf("CacheWriteTokens %d exceeds InputTokens %d; see the inclusion semantics on skyl.Usage",
			u.CacheWriteTokens, u.InputTokens)
	}
}

// Structured output is the check with the widest gap between what a fake can
// prove and what only a provider can. The three wire shapes share no key, and
// each provider validates the schema itself: OpenAI's strict mode rejects a
// schema missing "additionalProperties": false, and Gemini accepts an OpenAPI
// subset rather than JSON Schema. A schema our sandbox happily echoes can be a
// 400 here, which is precisely the finding this suite exists to surface.
func (l Live) testStructuredOutput(t *testing.T, p skyl.Provider) {
	ctx, cancel := ctxWithTimeout(t)
	defer cancel()

	// Deliberately strict-mode-clean: every property required,
	// additionalProperties false. A schema that fails on OpenAI for want of
	// those would be testing our fixture rather than the adapter.
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"city":    map[string]any{"type": "string"},
			"country": map[string]any{"type": "string"},
		},
		"required":             []string{"city", "country"},
		"additionalProperties": false,
	}

	resp, err := p.Complete(ctx, &skyl.Request{
		Model:     l.Model,
		MaxTokens: 256,
		Messages:  []skyl.Message{skyl.UserText("What is the capital of France, and which country is it in?")},
		ResponseFormat: &skyl.ResponseFormat{
			Name:   "location",
			Schema: schema,
		},
	})
	if err != nil {
		t.Fatalf("Complete() with a response format = %v", err)
	}

	// The document arrives in the ordinary text channel (ADR-0008), so this is
	// also the assertion that it is not hidden somewhere skyl does not read.
	var got struct {
		City    string `json:"city"`
		Country string `json:"country"`
	}
	if err := json.Unmarshal([]byte(resp.Text()), &got); err != nil {
		t.Fatalf("reply is not valid JSON despite a schema: %v\nreply: %q", err, resp.Text())
	}
	// Both properties were declared required, so both must be present. What
	// the model *said* is not asserted: the complete and stream checks already
	// prove the prompt lands, and pinning an answer here would be testing the
	// model rather than the adapter — and would make this the one check the
	// sandbox could not also run.
	if got.City == "" || got.Country == "" {
		t.Errorf("reply does not satisfy the schema it was given: %q", resp.Text())
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
