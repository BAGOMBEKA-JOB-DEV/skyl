package skyl

import (
	"encoding/json"
	"testing"
)

func TestMessageText(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		parts []Part
		want  string
	}{
		{"no parts", nil, ""},
		{"single text", []Part{Text{Text: "hello"}}, "hello"},
		{"single non-text", []Part{Image{URL: "http://x/y.png"}}, ""},
		{
			"several text parts are concatenated",
			[]Part{Text{Text: "a"}, Text{Text: "b"}, Text{Text: "c"}},
			"abc",
		},
		{
			"non-text parts are skipped",
			[]Part{Text{Text: "a"}, ToolCall{ID: "1", Name: "t"}, Text{Text: "b"}},
			"ab",
		},
		{
			"only tool calls yields empty",
			[]Part{ToolCall{ID: "1", Name: "t"}, ToolCall{ID: "2", Name: "u"}},
			"",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := Message{Role: RoleAssistant, Parts: tc.parts}.Text()
			if got != tc.want {
				t.Errorf("Text() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestMessageToolCalls(t *testing.T) {
	t.Parallel()

	m := Message{Role: RoleAssistant, Parts: []Part{
		Text{Text: "let me check"},
		ToolCall{ID: "a", Name: "first", Arguments: json.RawMessage(`{"x":1}`)},
		ToolCall{ID: "b", Name: "second"},
	}}

	calls := m.ToolCalls()
	if len(calls) != 2 {
		t.Fatalf("ToolCalls() returned %d calls, want 2", len(calls))
	}
	if calls[0].ID != "a" || calls[1].ID != "b" {
		t.Errorf("ToolCalls() lost ordering: got %q then %q", calls[0].ID, calls[1].ID)
	}

	// A message with no calls must return nil so `range` is safe without a
	// length check.
	if got := UserText("hi").ToolCalls(); got != nil {
		t.Errorf("ToolCalls() on a text-only message = %v, want nil", got)
	}
}

func TestMessageConstructors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		msg      Message
		wantRole Role
		wantText string
	}{
		{"UserText", UserText("hi"), RoleUser, "hi"},
		{"AssistantText", AssistantText("yes"), RoleAssistant, "yes"},
		{"ToolResultMessage", ToolResultMessage("c1", "42"), RoleTool, ""},
		{"ToolErrorMessage", ToolErrorMessage("c1", "boom"), RoleTool, ""},
		{"UserImage with caption", UserImage("image/png", []byte{1}, "what?"), RoleUser, "what?"},
		{"UserImage without caption", UserImage("image/png", []byte{1}, ""), RoleUser, ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if tc.msg.Role != tc.wantRole {
				t.Errorf("role = %q, want %q", tc.msg.Role, tc.wantRole)
			}
			if got := tc.msg.Text(); got != tc.wantText {
				t.Errorf("Text() = %q, want %q", got, tc.wantText)
			}
			if len(tc.msg.Parts) == 0 {
				t.Error("constructor produced a message with no parts")
			}
		})
	}
}

func TestToolErrorMessageMarksError(t *testing.T) {
	t.Parallel()

	m := ToolErrorMessage("c1", "failed")
	tr, ok := m.Parts[0].(ToolResult)
	if !ok {
		t.Fatalf("part is %T, want ToolResult", m.Parts[0])
	}
	if !tr.IsError {
		t.Error("IsError = false; a tool error must be flagged so the model can adapt")
	}

	ok2 := ToolResultMessage("c1", "fine").Parts[0].(ToolResult)
	if ok2.IsError {
		t.Error("ToolResultMessage marked a successful result as an error")
	}
}

func TestRoleValid(t *testing.T) {
	t.Parallel()

	for _, r := range []Role{RoleUser, RoleAssistant, RoleTool} {
		if !r.Valid() {
			t.Errorf("Role(%q).Valid() = false, want true", r)
		}
	}
	for _, r := range []Role{"", "system", "model", "bogus"} {
		if r.Valid() {
			t.Errorf("Role(%q).Valid() = true, want false", r)
		}
	}
}

func TestUsageAddAndTotal(t *testing.T) {
	t.Parallel()

	a := Usage{InputTokens: 10, OutputTokens: 5, CacheReadTokens: 2, CacheWriteTokens: 1}
	b := Usage{InputTokens: 1, OutputTokens: 2, CacheReadTokens: 3, CacheWriteTokens: 4}

	sum := a.Add(b)
	want := Usage{InputTokens: 11, OutputTokens: 7, CacheReadTokens: 5, CacheWriteTokens: 5}
	if sum != want {
		t.Errorf("Add() = %+v, want %+v", sum, want)
	}
	if got := a.TotalTokens(); got != 18 {
		t.Errorf("TotalTokens() = %d, want 18", got)
	}
}

func TestResponseAccessorsOnNil(t *testing.T) {
	t.Parallel()

	// Callers frequently write resp.Text() before checking err; a nil
	// receiver must not panic.
	var r *Response
	if got := r.Text(); got != "" {
		t.Errorf("nil Response.Text() = %q, want empty", got)
	}
	if got := r.ToolCalls(); got != nil {
		t.Errorf("nil Response.ToolCalls() = %v, want nil", got)
	}
}
