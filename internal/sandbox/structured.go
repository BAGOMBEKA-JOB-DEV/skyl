package sandbox

import (
	"encoding/json"
	"sort"
)

// Structured output, shared by all three protocols.
//
// The wire shapes have nothing in common — OpenAI nests a schema two levels
// deep under response_format.json_schema, Anthropic puts it under
// output_config.format, Gemini splits it into generationConfig.responseMimeType
// plus responseSchema — but what the server has to *do* is identical, so it is
// decided once here and rendered three ways. Same reasoning as tools.go.
//
// Without this the sandbox would accept a structured-output request and reply
// with prose, and `go test -tags=sandbox` would prove only that the adapter did
// not crash. A test that cannot fail is not a test.

// answerForSchema builds a reply that satisfies the requested schema.
//
// It is a deliberately shallow generator: object properties are filled with a
// plausible value per declared type, and anything it does not understand
// becomes a string. That is enough to prove the round trip — the schema left
// the caller, survived the adapter's mapping, arrived here, and shaped the
// reply — which is the only thing the sandbox can honestly demonstrate.
//
// It is emphatically not a JSON Schema implementation. A real provider
// constrains generation with a grammar; this fills in blanks. Tests should
// assert the shape came back, not that the sandbox is a validator.
func answerForSchema(schema map[string]any) string {
	value := valueForSchema(schema, 0)
	out, err := json.Marshal(value)
	if err != nil {
		// valueForSchema only ever produces marshallable values, so this is
		// unreachable — but the sandbox must not panic on a caller's input.
		return `{}`
	}
	return string(out)
}

// maxSchemaDepth stops a self-referential or pathological schema from
// recursing forever. The sandbox takes untrusted input from whoever is testing
// against it, and a hang is a worse failure than a shallow answer.
const maxSchemaDepth = 8

func valueForSchema(schema map[string]any, depth int) any {
	if depth > maxSchemaDepth {
		return nil
	}

	// A const or a single-valued enum is the one case where the schema states
	// the answer outright, so honour it rather than inventing something.
	if c, ok := schema["const"]; ok {
		return c
	}
	if enum, ok := schema["enum"].([]any); ok && len(enum) > 0 {
		return enum[0]
	}

	switch schemaType(schema) {
	case "object":
		out := map[string]any{}
		props, _ := schema["properties"].(map[string]any)

		// Sorted, so a given schema always produces byte-identical output.
		// Map iteration order would make the sandbox non-deterministic and
		// every assertion against it flaky.
		names := make([]string, 0, len(props))
		for name := range props {
			names = append(names, name)
		}
		sort.Strings(names)

		for _, name := range names {
			sub, ok := props[name].(map[string]any)
			if !ok {
				sub = map[string]any{}
			}
			out[name] = valueForSchema(sub, depth+1)
		}
		return out

	case "array":
		items, ok := schema["items"].(map[string]any)
		if !ok {
			items = map[string]any{}
		}
		// One element: enough to prove the array is an array and that item
		// schemas are honoured, without inflating the payload.
		return []any{valueForSchema(items, depth+1)}

	case "integer":
		return 1
	case "number":
		return 1.5
	case "boolean":
		return true
	case "null":
		return nil
	default:
		// Unknown or absent type. A string is the safest filler: it marshals
		// cleanly and reads obviously as sandbox output in a failure message.
		return "sandbox"
	}
}

// schemaType reads the declared type, tolerating the array form JSON Schema
// allows (`"type": ["string", "null"]`), which Gemini does not accept but
// OpenAI does — the sandbox is lenient so an adapter's own strictness is what
// a test observes.
func schemaType(schema map[string]any) string {
	switch t := schema["type"].(type) {
	case string:
		return t
	case []any:
		for _, v := range t {
			if s, ok := v.(string); ok && s != "null" {
				return s
			}
		}
	}
	// No type, but properties present, means object in every dialect we serve.
	if _, ok := schema["properties"]; ok {
		return "object"
	}
	return ""
}

// hasSchema reports whether a decoded schema is usable.
//
// An empty map is treated as absent: skyl's Request.Validate already rejects
// that, so a request carrying one did not come from this library and the
// sandbox has nothing to shape a reply around.
func hasSchema(schema map[string]any) bool {
	return len(schema) > 0
}
