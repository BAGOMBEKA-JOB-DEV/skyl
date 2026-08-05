//go:build sandbox

// Structured output, end to end over a real socket, for every adapter.
//
//	go test -tags=sandbox ./provider/
//
// The load-bearing assertion in this file is that the *reply matches the schema
// the caller sent*. The sandbox builds its answer from the schema it received,
// so an adapter that puts the schema under the wrong key — or drops it — comes
// back with prose and every case here fails. A canned JSON reply would have
// proved nothing.
//
// The three wire shapes have nothing in common (response_format.json_schema,
// output_config.format, generationConfig.responseSchema + responseMimeType), so
// this is the only place that failure mode is visible without a credential.
//
// The usual caveat holds: this proves the round trip is self-consistent and
// survives real HTTP, not that the field names match what a provider sends.
// Only -tags=integration settles that.
package provider_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/BAGOMBEKA-JOB-DEV/skyl"
	"github.com/BAGOMBEKA-JOB-DEV/skyl/internal/testutil"
)

// personSchema is the fixture every case sends. It has more than one property,
// and a non-string type, so an adapter that mangles the schema in transit
// produces an observably wrong reply rather than a coincidentally right one.
func personSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"name": map[string]any{"type": "string"},
			"age":  map[string]any{"type": "integer"},
		},
		"required":             []string{"name", "age"},
		"additionalProperties": false,
	}
}

func TestSandboxStructuredOutput(t *testing.T) {
	base := newSandbox(t)

	for name, p := range toolTargets(t, base) {
		t.Run(name, func(t *testing.T) {
			resp, err := p.Complete(testutil.Context(t), &skyl.Request{
				Model:          toolModel(name),
				MaxTokens:      256,
				Messages:       []skyl.Message{skyl.UserText("Describe someone.")},
				ResponseFormat: &skyl.ResponseFormat{Name: "person", Schema: personSchema()},
			})
			if err != nil {
				t.Fatalf("Complete() = %v", err)
			}

			// The reply arrives as ordinary assistant text (ADR-0008), so
			// Text() is the JSON document.
			var got struct {
				Name *string `json:"name"`
				Age  *int    `json:"age"`
			}
			if err := json.Unmarshal([]byte(resp.Text()), &got); err != nil {
				t.Fatalf("reply is not JSON: %v\nreply: %q", err, resp.Text())
			}
			if got.Name == nil {
				t.Errorf("reply has no \"name\": %q — the schema did not reach the wire", resp.Text())
			}
			if got.Age == nil {
				t.Errorf("reply has no \"age\": %q — the schema was truncated in transit", resp.Text())
			}
		})
	}
}

// Without a ResponseFormat the reply must stay prose. If the sandbox returned
// JSON either way, the test above would pass for an adapter that never sent
// the schema at all.
func TestSandboxWithoutStructuredOutputStaysProse(t *testing.T) {
	base := newSandbox(t)

	for name, p := range toolTargets(t, base) {
		t.Run(name, func(t *testing.T) {
			resp, err := p.Complete(testutil.Context(t), &skyl.Request{
				Model:     toolModel(name),
				MaxTokens: 256,
				Messages:  []skyl.Message{skyl.UserText("Describe someone.")},
			})
			if err != nil {
				t.Fatalf("Complete() = %v", err)
			}
			if strings.HasPrefix(strings.TrimSpace(resp.Text()), "{") {
				t.Errorf("got JSON for a request with no ResponseFormat: %q", resp.Text())
			}
		})
	}
}

// A structured reply streams as fragments like any other text, and only the
// concatenation is valid JSON. This is the documented consequence of returning
// the document in the ordinary text channel, and it is worth pinning: a caller
// that unmarshals each delta must fail here rather than in production.
func TestSandboxStructuredOutputStreams(t *testing.T) {
	base := newSandbox(t)

	for name, p := range toolTargets(t, base) {
		t.Run(name, func(t *testing.T) {
			stream, err := p.Stream(testutil.Context(t), &skyl.Request{
				Model:          toolModel(name),
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

			// More than one delta, or the fragmentation this test exists for
			// never happened.
			if deltas < 2 {
				t.Errorf("got %d text deltas, want the document split across several", deltas)
			}
			if !json.Valid([]byte(doc.String())) {
				t.Errorf("concatenated stream is not valid JSON: %q", doc.String())
			}
		})
	}
}
