# Getting started

## Requirements

- **Go 1.26 or later** (`go version`)
- An API key for at least one provider — or [Ollama](https://ollama.com)
  running locally, which needs no key at all

## Install

```bash
go get github.com/BAGOMBEKA-JOB-DEV/skyl
```

## Your first call

```go
package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/BAGOMBEKA-JOB-DEV/skyl"
	"github.com/BAGOMBEKA-JOB-DEV/skyl/provider/anthropic"
)

func main() {
	client := skyl.New(anthropic.New(os.Getenv("ANTHROPIC_API_KEY")))

	resp, err := client.Complete(context.Background(), &skyl.Request{
		Model:     "claude-opus-5",
		MaxTokens: 1024,
		Messages: []skyl.Message{
			skyl.UserText("Explain Go channels in two sentences."),
		},
	})
	if err != nil {
		log.Fatal(err)
	}

	fmt.Println(resp.Text())
	fmt.Printf("%d in / %d out\n", resp.Usage.InputTokens, resp.Usage.OutputTokens)
}
```

## Switching providers

The only line that changes is the constructor.

```go
client := skyl.New(openai.New(os.Getenv("OPENAI_API_KEY")))     // GPT
client := skyl.New(gemini.New(os.Getenv("GEMINI_API_KEY")))     // Gemini
```

Or run entirely locally, with no API key:

```go
client := skyl.New(openaicompat.New(
	openaicompat.WithBaseURL("http://localhost:11434/v1"),
	openaicompat.WithName("ollama"),
))

resp, err := client.Complete(ctx, &skyl.Request{
	Model:    "llama3.3",
	Messages: []skyl.Message{skyl.UserText("Hello")},
})
```

Selecting a provider at runtime is ordinary Go — `Provider` is an interface:

```go
func pick(name string) (skyl.Provider, error) {
	switch name {
	case "anthropic":
		return anthropic.New(os.Getenv("ANTHROPIC_API_KEY")), nil
	case "openai":
		return openai.New(os.Getenv("OPENAI_API_KEY")), nil
	default:
		return nil, fmt.Errorf("unknown provider %q", name)
	}
}
```

## System prompts and multi-turn

```go
req := &skyl.Request{
	Model:  "claude-opus-5",
	System: "You are a terse Go expert. Answer in one sentence.",
	Messages: []skyl.Message{
		skyl.UserText("What is a nil map?"),
		skyl.AssistantText("A map that is declared but not allocated."),
		skyl.UserText("Can I read from one?"),
	},
}
```

`System` is a dedicated field because providers place it differently — a
top-level parameter for Anthropic, a message for OpenAI, `systemInstruction`
for Gemini. skyl puts it where each provider expects.

## Streaming

```go
stream, err := client.Stream(ctx, req)
if err != nil {
	log.Fatal(err)
}
defer stream.Close()

for stream.Next() {
	if ev := stream.Event(); ev.Type == skyl.EventTextDelta {
		fmt.Print(ev.Text)
	}
}
if err := stream.Err(); err != nil {
	log.Fatal(err)
}
```

Always `defer stream.Close()`. Abandoning a stream early is safe — `Close()`
releases the connection, and the reader is bound to the request context — but
closing is what makes it prompt rather than eventual.

## Tool calling

```go
req := &skyl.Request{
	Model:    "claude-opus-5",
	Messages: []skyl.Message{skyl.UserText("What's the weather in Kampala?")},
	Tools: []skyl.Tool{{
		Name:        "get_weather",
		Description: "Get the current weather for a city.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"city": map[string]any{"type": "string"},
			},
			"required": []string{"city"},
		},
	}},
}

resp, _ := client.Complete(ctx, req)

for _, call := range resp.ToolCalls() {
	result := runTool(call.Name, call.Arguments)

	req.Messages = append(req.Messages,
		resp.Message,
		skyl.ToolResultMessage(call.ID, result),
	)
}

final, _ := client.Complete(ctx, req) // loop until no tool calls remain
```

## Errors

Branch on classification, never on message text:

```go
resp, err := client.Complete(ctx, req)
switch {
case err == nil:
	// ok
case errors.Is(err, skyl.ErrRateLimit):
	// Client already retried; this means it kept failing.
case errors.Is(err, skyl.ErrAuth):
	log.Fatal("bad API key")
case errors.Is(err, skyl.ErrNotFound):
	log.Fatal("no such model for this provider")
}
```

For detail, unwrap to `*skyl.Error`:

```go
var e *skyl.Error
if errors.As(err, &e) {
	log.Printf("%s returned %d: %s", e.Provider, e.StatusCode, e.Message)
}
```

## Configuring the client

```go
client := skyl.New(p,
	skyl.WithMaxRetries(5),
	skyl.WithTimeout(90*time.Second),
	skyl.WithHook(func(ctx context.Context, ev skyl.HookEvent) {
		metrics.Record(ev.Provider, ev.Model, ev.Duration, ev.Err)
	}),
)
```

Supplying your own `*http.Client` is a **provider** option, since the client
is what performs HTTP — `openai.WithHTTPClient(...)`, `gemini.WithHTTPClient(...)`,
and so on.

Retries use exponential backoff with full jitter and honour `Retry-After`.
Only rate limits, server errors, and connection failures are retried — retrying
an auth failure or a malformed request just burns quota.

## Reaching a feature skyl doesn't model

Two escape hatches, so skyl is never the reason you can't ship.

**Send something extra:**

```go
req.ProviderOptions = map[string]any{
	"top_k": 40,
}
```

**Read something extra:**

```go
var full map[string]any
json.Unmarshal(resp.Raw, &full) // untouched provider JSON
```

## Next

- [providers.md](providers.md) — every reachable provider and model discovery
- [gateway.md](gateway.md) — expose skyl over HTTP with chi
- [architecture.md](architecture.md) — how it works and why
