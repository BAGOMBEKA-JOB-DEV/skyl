package sandbox

import (
	"encoding/json"
	"reflect"
	"testing"
)

// The generator is what makes the structured-output round trip observable: an
// adapter that mangles a schema produces a reply that does not match it. That
// only holds if the generator actually reads the schema, so its branches are
// worth pinning directly rather than only through the three protocol handlers.
func TestAnswerForSchema(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		schema map[string]any
		want   any
	}{
		{
			name:   "string",
			schema: map[string]any{"type": "string"},
			want:   "sandbox",
		},
		{
			name:   "integer",
			schema: map[string]any{"type": "integer"},
			want:   float64(1), // JSON numbers decode as float64
		},
		{
			name:   "number",
			schema: map[string]any{"type": "number"},
			want:   1.5,
		},
		{
			name:   "boolean",
			schema: map[string]any{"type": "boolean"},
			want:   true,
		},
		{
			name:   "null",
			schema: map[string]any{"type": "null"},
			want:   nil,
		},
		{
			name:   "no type at all",
			schema: map[string]any{},
			want:   "sandbox",
		},
		{
			// The schema states the answer, so inventing one would be worse.
			name:   "const wins over type",
			schema: map[string]any{"type": "string", "const": "fixed"},
			want:   "fixed",
		},
		{
			name:   "enum takes the first value",
			schema: map[string]any{"type": "string", "enum": []any{"a", "b"}},
			want:   "a",
		},
		{
			// JSON Schema allows a type union; Gemini does not, but the
			// sandbox stays lenient so an adapter's own strictness is what a
			// test observes rather than the sandbox's.
			name:   "union type picks the non-null member",
			schema: map[string]any{"type": []any{"null", "integer"}},
			want:   float64(1),
		},
		{
			name: "properties without a type still means object",
			schema: map[string]any{
				"properties": map[string]any{"a": map[string]any{"type": "string"}},
			},
			want: map[string]any{"a": "sandbox"},
		},
		{
			name: "array of strings",
			schema: map[string]any{
				"type":  "array",
				"items": map[string]any{"type": "string"},
			},
			want: []any{"sandbox"},
		},
		{
			name:   "array without items",
			schema: map[string]any{"type": "array"},
			want:   []any{"sandbox"},
		},
		{
			name: "nested object",
			schema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"inner": map[string]any{
						"type":       "object",
						"properties": map[string]any{"n": map[string]any{"type": "integer"}},
					},
				},
			},
			want: map[string]any{"inner": map[string]any{"n": float64(1)}},
		},
		{
			// A property whose schema is not an object at all — legal JSON,
			// nonsense as a schema. It must not panic.
			name: "malformed property schema",
			schema: map[string]any{
				"type":       "object",
				"properties": map[string]any{"a": "not-a-schema"},
			},
			want: map[string]any{"a": "sandbox"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var got any
			if err := json.Unmarshal([]byte(answerForSchema(tt.schema)), &got); err != nil {
				t.Fatalf("output is not valid JSON: %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got %#v, want %#v", got, tt.want)
			}
		})
	}
}

// The same schema must always produce the same bytes. Map iteration order
// would otherwise make every assertion against the sandbox intermittently
// fail, which is worse than failing outright.
func TestAnswerForSchemaIsDeterministic(t *testing.T) {
	t.Parallel()

	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"alpha": map[string]any{"type": "string"},
			"beta":  map[string]any{"type": "integer"},
			"gamma": map[string]any{"type": "boolean"},
			"delta": map[string]any{"type": "number"},
		},
	}

	first := answerForSchema(schema)
	for range 20 {
		if got := answerForSchema(schema); got != first {
			t.Fatalf("output varies between runs:\n%s\n%s", first, got)
		}
	}
}

// A self-referential schema is legal input and must not hang the server. The
// sandbox accepts whatever a caller sends, so an unbounded recursion here
// would be a denial of service against whoever is testing against it.
func TestAnswerForSchemaStopsRecursing(t *testing.T) {
	t.Parallel()

	schema := map[string]any{"type": "object", "properties": map[string]any{}}
	schema["properties"].(map[string]any)["self"] = schema

	out := answerForSchema(schema)
	if !json.Valid([]byte(out)) {
		t.Fatalf("output is not valid JSON: %s", out)
	}
}

func TestHasSchema(t *testing.T) {
	t.Parallel()

	// An empty map is absent, not a schema: Request.Validate already rejects
	// one, so a request carrying it did not come from this library.
	if hasSchema(nil) || hasSchema(map[string]any{}) {
		t.Error("an empty schema must be treated as absent")
	}
	if !hasSchema(map[string]any{"type": "object"}) {
		t.Error("a populated schema must be treated as present")
	}
}
