package skyl

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestRequestValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		req     *Request
		wantErr bool
		wantIn  string
	}{
		{
			name: "minimal valid request",
			req:  &Request{Model: "m", Messages: []Message{UserText("hi")}},
		},
		{
			name: "fully populated request",
			req: &Request{
				Model:      "m",
				System:     "be terse",
				Messages:   []Message{UserText("hi")},
				MaxTokens:  100,
				Tools:      []Tool{{Name: "t"}},
				ToolChoice: &ToolChoice{Mode: ToolChoiceAuto},
			},
		},
		{
			name:    "nil request",
			req:     nil,
			wantErr: true, wantIn: "nil request",
		},
		{
			name:    "missing model",
			req:     &Request{Messages: []Message{UserText("hi")}},
			wantErr: true, wantIn: "model is required",
		},
		{
			name:    "no messages",
			req:     &Request{Model: "m"},
			wantErr: true, wantIn: "at least one message",
		},
		{
			name:    "negative max tokens",
			req:     &Request{Model: "m", Messages: []Message{UserText("hi")}, MaxTokens: -1},
			wantErr: true, wantIn: "max tokens",
		},
		{
			name: "invalid role",
			req: &Request{Model: "m", Messages: []Message{
				{Role: "wizard", Parts: []Part{Text{Text: "hi"}}},
			}},
			wantErr: true, wantIn: "invalid role",
		},
		{
			name:    "message with no parts",
			req:     &Request{Model: "m", Messages: []Message{{Role: RoleUser}}},
			wantErr: true, wantIn: "no parts",
		},
		{
			name: "image without data or URL",
			req: &Request{Model: "m", Messages: []Message{
				{Role: RoleUser, Parts: []Part{Image{MediaType: "image/png"}}},
			}},
			wantErr: true, wantIn: "data or a URL",
		},
		{
			name: "image data without media type",
			req: &Request{Model: "m", Messages: []Message{
				{Role: RoleUser, Parts: []Part{Image{Data: []byte{1, 2}}}},
			}},
			wantErr: true, wantIn: "media type",
		},
		{
			name: "image URL alone is fine",
			req: &Request{Model: "m", Messages: []Message{
				{Role: RoleUser, Parts: []Part{Image{URL: "https://example.com/a.png"}}},
			}},
		},
		{
			name: "tool call without ID",
			req: &Request{Model: "m", Messages: []Message{
				{Role: RoleAssistant, Parts: []Part{ToolCall{Name: "t"}}},
			}},
			wantErr: true, wantIn: "tool call needs",
		},
		{
			name: "tool result without call ID",
			req: &Request{Model: "m", Messages: []Message{
				{Role: RoleTool, Parts: []Part{ToolResult{Content: "42"}}},
			}},
			wantErr: true, wantIn: "tool result needs",
		},
		{
			name: "nil part",
			req: &Request{Model: "m", Messages: []Message{
				{Role: RoleUser, Parts: []Part{nil}},
			}},
			wantErr: true, wantIn: "is nil",
		},
		{
			name: "tool without a name",
			req: &Request{Model: "m", Messages: []Message{UserText("hi")},
				Tools: []Tool{{Description: "no name"}}},
			wantErr: true, wantIn: "tool 0 has no name",
		},
		{
			name: "specific tool choice without a name",
			req: &Request{Model: "m", Messages: []Message{UserText("hi")},
				ToolChoice: &ToolChoice{Mode: ToolChoiceSpecific}},
			wantErr: true, wantIn: "requires a name",
		},
		{
			name: "unknown tool choice mode",
			req: &Request{Model: "m", Messages: []Message{UserText("hi")},
				ToolChoice: &ToolChoice{Mode: "sometimes"}},
			wantErr: true, wantIn: "unknown tool choice mode",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.req.Validate()

			if !tc.wantErr {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatal("Validate() = nil, want an error")
			}
			if !errors.Is(err, ErrBadRequest) {
				t.Errorf("Validate() error = %v, want it to wrap ErrBadRequest", err)
			}
			if !strings.Contains(err.Error(), tc.wantIn) {
				t.Errorf("Validate() error = %q, want it to mention %q", err.Error(), tc.wantIn)
			}
		})
	}
}

func TestValidateAcceptsAnyModelString(t *testing.T) {
	t.Parallel()

	// ADR-0004: model IDs are opaque. Validation must never reject one, or
	// skyl becomes the reason a user cannot reach a newly released model.
	for _, model := range []string{
		"claude-opus-5",
		"gpt-5.6-sol",
		"gemini-3.6-flash",
		"some-model-released-tomorrow",
		"llama3.3:70b-instruct-q4_K_M",
		"accounts/fireworks/models/deepseek-v3",
	} {
		req := &Request{Model: model, Messages: []Message{UserText("hi")}}
		if err := req.Validate(); err != nil {
			t.Errorf("Validate() rejected model %q: %v", model, err)
		}
	}
}

func TestShouldRetry(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"rate limit", NewError("p", 429, ErrRateLimit, "", nil), true},
		{"server error", NewError("p", 500, ErrServer, "", nil), true},
		{"auth", NewError("p", 401, ErrAuth, "", nil), false},
		{"bad request", NewError("p", 400, ErrBadRequest, "", nil), false},
		{"refusal", NewError("p", 200, ErrRefusal, "", nil), false},
		{"context canceled", context.Canceled, false},
		{"deadline exceeded", context.DeadlineExceeded, false},
		{"unclassified transport error", errors.New("connection reset by peer"), true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := shouldRetry(tc.err); got != tc.want {
				t.Errorf("shouldRetry(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestBackoffGrowsAndIsBounded(t *testing.T) {
	t.Parallel()

	p := retryPolicy{baseDelay: 100 * time.Millisecond, maxDelay: 2 * time.Second}

	// Full jitter means any single sample can be small, so assert the bound
	// rather than monotonic growth — the bound is the invariant that matters.
	for attempt := range 12 {
		for range 50 {
			d := p.backoff(attempt, 0)
			if d < 0 {
				t.Fatalf("backoff(%d) = %v, want non-negative", attempt, d)
			}
			if d > p.maxDelay {
				t.Fatalf("backoff(%d) = %v, want <= maxDelay %v", attempt, d, p.maxDelay)
			}
		}
	}
}

func TestBackoffJitters(t *testing.T) {
	t.Parallel()

	p := retryPolicy{baseDelay: time.Second, maxDelay: time.Minute}

	seen := make(map[time.Duration]struct{})
	for range 50 {
		seen[p.backoff(5, 0)] = struct{}{}
	}
	// Without jitter a fleet reconverges into a thundering herd, so a fixed
	// delay here would be a real bug.
	if len(seen) < 5 {
		t.Errorf("backoff produced %d distinct delays over 50 samples; want jitter", len(seen))
	}
}

func TestBackoffHonoursRetryAfter(t *testing.T) {
	t.Parallel()

	p := retryPolicy{baseDelay: time.Millisecond, maxDelay: time.Minute}

	got := p.backoff(0, 5*time.Second)
	if got != 5*time.Second {
		t.Errorf("backoff with Retry-After = %v, want exactly 5s", got)
	}

	// A hint longer than maxDelay must still be capped, so a provider cannot
	// wedge the caller for an hour.
	if got := p.backoff(0, time.Hour); got != p.maxDelay {
		t.Errorf("backoff with a 1h hint = %v, want it capped at %v", got, p.maxDelay)
	}
}

func TestBackoffUsesDefaultsWhenUnset(t *testing.T) {
	t.Parallel()

	var p retryPolicy // zero value
	if d := p.backoff(0, 0); d < 0 || d > defaultMaxDelay {
		t.Errorf("zero-value policy backoff = %v, want a sane bounded delay", d)
	}
}

func TestRetryAfterFrom(t *testing.T) {
	t.Parallel()

	e := NewError("p", 429, ErrRateLimit, "", nil)
	e.RetryAfter = 3 * time.Second

	if got := retryAfterFrom(e); got != 3*time.Second {
		t.Errorf("retryAfterFrom() = %v, want 3s", got)
	}
	if got := retryAfterFrom(errors.New("plain")); got != 0 {
		t.Errorf("retryAfterFrom(plain error) = %v, want 0", got)
	}
}
