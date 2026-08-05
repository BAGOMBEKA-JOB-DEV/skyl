//go:build sandbox

// Structured output over a real socket, for the Anthropic adapter.
//
//	cd provider/anthropic && go test -tags=sandbox ./...
//
// The sibling suite in provider/sandbox_structured_test.go covers the other
// three adapters; this one lives here because the adapter is its own module.
// It is worth running separately: this is the only adapter that builds a typed
// SDK struct rather than a map, so its schema takes a different route to the
// wire — through anthropic-sdk-go's own marshalling — and is the most likely
// of the four to arrive altered.
package anthropic_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/BAGOMBEKA-JOB-DEV/skyl"
	"github.com/BAGOMBEKA-JOB-DEV/skyl/internal/testutil"
)

func personSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"name": map[string]any{"type": "string"},
			"age":  map[string]any{"type": "integer"},
		},
		"required": []string{"name", "age"},
	}
}

// The sandbox builds its reply from the schema it received, so an adapter that
// puts the schema anywhere but output_config.format comes back with prose.
func TestSandboxAnthropicStructuredOutput(t *testing.T) {
	resp, err := skyl.New(sandboxProvider(t)).Complete(testutil.Context(t), &skyl.Request{
		Model:          "claude-opus-5",
		MaxTokens:      256,
		Messages:       []skyl.Message{skyl.UserText("Describe someone.")},
		ResponseFormat: &skyl.ResponseFormat{Name: "person", Schema: personSchema()},
	})
	if err != nil {
		t.Fatalf("Complete() = %v", err)
	}

	var got struct {
		Name *string `json:"name"`
		Age  *int    `json:"age"`
	}
	if err := json.Unmarshal([]byte(resp.Text()), &got); err != nil {
		t.Fatalf("reply is not JSON: %v\nreply: %q", err, resp.Text())
	}
	if got.Name == nil || got.Age == nil {
		t.Errorf("reply does not match the schema sent: %q", resp.Text())
	}
}

func TestSandboxAnthropicWithoutStructuredOutputStaysProse(t *testing.T) {
	resp, err := skyl.New(sandboxProvider(t)).Complete(testutil.Context(t), &skyl.Request{
		Model:     "claude-opus-5",
		MaxTokens: 256,
		Messages:  []skyl.Message{skyl.UserText("Describe someone.")},
	})
	if err != nil {
		t.Fatalf("Complete() = %v", err)
	}
	if strings.HasPrefix(strings.TrimSpace(resp.Text()), "{") {
		t.Errorf("got JSON for a request with no ResponseFormat: %q", resp.Text())
	}
}

// Effort and the schema share output_config, and this adapter sets them in two
// separate places in buildParams. Sending both proves neither clobbers the
// other on the way through the SDK's marshalling.
func TestSandboxAnthropicStructuredOutputWithEffort(t *testing.T) {
	resp, err := skyl.New(sandboxProvider(t)).Complete(testutil.Context(t), &skyl.Request{
		Model:          "claude-opus-5",
		MaxTokens:      256,
		Messages:       []skyl.Message{skyl.UserText("Describe someone.")},
		Thinking:       &skyl.Thinking{Enabled: true, Effort: skyl.EffortHigh},
		ResponseFormat: &skyl.ResponseFormat{Schema: personSchema()},
	})
	if err != nil {
		t.Fatalf("Complete() = %v", err)
	}
	if !json.Valid([]byte(resp.Text())) {
		t.Errorf("reply is not JSON with effort also set: %q", resp.Text())
	}
}

// A structured reply streams as fragments; only the concatenation is JSON.
func TestSandboxAnthropicStructuredOutputStreams(t *testing.T) {
	stream, err := skyl.New(sandboxProvider(t)).Stream(testutil.Context(t), &skyl.Request{
		Model:          "claude-opus-5",
		MaxTokens:      256,
		Messages:       []skyl.Message{skyl.UserText("Describe someone.")},
		ResponseFormat: &skyl.ResponseFormat{Schema: personSchema()},
	})
	if err != nil {
		t.Fatalf("Stream() = %v", err)
	}
	defer stream.Close() //nolint:errcheck // test cleanup

	var (
		doc    strings.Builder
		deltas int
	)
	for stream.Next() {
		if ev := stream.Event(); ev.Type == skyl.EventTextDelta {
			deltas++
			doc.WriteString(ev.Text)
		}
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("Err() = %v", err)
	}
	if deltas < 2 {
		t.Errorf("got %d text deltas, want the document split across several", deltas)
	}
	if !json.Valid([]byte(doc.String())) {
		t.Errorf("concatenated stream is not valid JSON: %q", doc.String())
	}
}
