# Migrating to skyl

Coming from a vendor SDK or another abstraction. Each section covers the
mapping, what you gain, and what you give up.

## First: should you?

[idea.md](idea.md) is blunt about this and it is worth repeating here.

**If you use exactly one model from one vendor and always will, do not migrate.**
Use that vendor's SDK. It will support their newest feature the day it ships;
skyl will support it whenever an adapter is updated, or immediately through
`ProviderOptions` if you are willing to write vendor-specific code — at which
point you have the SDK's coupling and skyl's indirection at once.

skyl earns its place the moment you have a second model. A cheap classifier
alongside a frontier model, a local Ollama in development and a hosted model in
production, an A/B between vendors, or a fallback when one is down.

---

## From `openai-go` (or `go-openai`)

The closest mapping — skyl's OpenAI adapter speaks the same wire format.

```go
// before
client := openai.NewClient(option.WithAPIKey(key))
resp, err := client.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
    Model:    openai.ChatModelGPT4o,
    Messages: []openai.ChatCompletionMessageParamUnion{
        openai.UserMessage("Explain Go channels."),
    },
})
text := resp.Choices[0].Message.Content

// after
client := skyl.New(skylopenai.New(key))
resp, err := client.Complete(ctx, &skyl.Request{
    Model:     "gpt-5.6",
    MaxTokens: 1024,
    Messages:  []skyl.Message{skyl.UserText("Explain Go channels.")},
})
text := resp.Text()
```

| `openai-go` | skyl |
|---|---|
| `ChatCompletionNewParams` | `skyl.Request` |
| `openai.UserMessage(s)` | `skyl.UserText(s)` |
| `openai.ChatModelGPT4o` (a constant) | `"gpt-5.6"` (a string — see below) |
| `resp.Choices[0].Message.Content` | `resp.Text()` |
| `resp.Choices[0].Message.ToolCalls` | `resp.ToolCalls()` |
| `client.Chat.Completions.NewStreaming` | `client.Stream` |
| `resp.Usage.PromptTokens` | `resp.Usage.InputTokens` |
| any unmodelled field | `Request.ProviderOptions` |
| the whole response | `Response.Raw` |

**Model constants become strings.** This is deliberate
([ADR-0004](adr/0004-model-ids-are-pass-through.md)): a model released after
your skyl build works immediately, and a typo surfaces as the provider's own
not-found error rather than a compile error. You lose compile-time checking and
gain never being blocked on a release. If you want the constants back, declare
them in your own package.

**You gain** retry with jitter, `Retry-After` handling, typed error
classification, and the ability to change vendor by changing one constructor.

**You give up** same-day support for new OpenAI features. `ProviderOptions`
covers the gap.

---

## From `anthropic-sdk-go`

```go
// before
client := anthropic.NewClient(option.WithAPIKey(key))
msg, err := client.Messages.New(ctx, anthropic.MessageNewParams{
    Model:     anthropic.ModelClaudeSonnet4_5,
    MaxTokens: 1024,
    Messages:  []anthropic.MessageParam{
        anthropic.NewUserMessage(anthropic.NewTextBlock("Hello")),
    },
})

// after
client := skyl.New(skylanthropic.New(key))
resp, err := client.Complete(ctx, &skyl.Request{
    Model:     "claude-sonnet-5",
    MaxTokens: 1024,
    Messages:  []skyl.Message{skyl.UserText("Hello")},
})
```

`provider/anthropic` is built on the official SDK, so behaviour is the SDK's.
Three things change:

| | Note |
|---|---|
| `MaxTokens` | still honoured; skyl supplies **4096** if you omit it, because the API requires the field and every other provider does not |
| Thinking | `skyl.Thinking{Enabled: true}` maps to adaptive thinking. ⚠️ **`Effort` is ignored** — the SDK's config has no such field. See the [feature matrix](feature-matrix.md#thinking) |
| Prompt caching | not modelled. Use `ProviderOptions` with a JSON path — this adapter supports nested paths, unlike the others |

**You give up** the SDK's typed content blocks. Anything skyl does not model is
still in `Response.Raw`.

**Worth knowing:** the SDK adds fingerprinting headers (OS, architecture, Go
version) to every request, and reads `ANTHROPIC_BASE_URL` from the environment.
Both are documented in [data-handling.md](data-handling.md). Neither is skyl's
doing and neither can be turned off from here.

---

## From `langchaingo`

A different kind of move: langchaingo is a framework, skyl is a transport layer.

```go
// before
llm, err := openai.New(openai.WithModel("gpt-4o"))
completion, err := llms.GenerateFromSinglePrompt(ctx, llm, "Explain Go channels.")

// after
client := skyl.New(skylopenai.New(os.Getenv("OPENAI_API_KEY")))
resp, err := client.Complete(ctx, &skyl.Request{
    Model:     "gpt-5.6",
    MaxTokens: 1024,
    Messages:  []skyl.Message{skyl.UserText("Explain Go channels.")},
})
```

**skyl does not replace langchaingo.** It replaces the part of it that talks to
models. It has no chains, no agents, no memory, no prompt templates, no vector
stores, no document loaders — and will not
([idea.md non-goals](idea.md#non-goals)).

Migrate the model-calling layer if you want honest provider abstraction, small
dependencies and real error classification, and you were not using the framework
parts. Keep langchaingo if the chains and retrievers are what you came for. The
two can coexist: skyl underneath, your own orchestration on top.

**You gain** a zero-dependency core, a documented feature matrix, and knowing
exactly what is sent.

**You give up** everything above the transport layer. That is a lot if you use
it, and nothing if you do not.

---

## From a hand-rolled client

The most common starting point, and the easiest move — you are replacing code
you already maintain.

Things worth checking against your implementation, because they are the ones
usually missing:

- **Retry** with exponential backoff and **full jitter**, honouring
  `Retry-After`. Without jitter a fleet retries in lockstep and turns a
  provider's bad minute into its bad hour.
- **Non-retryable classification**: auth failures, bad requests and refusals are
  never retried. A rejected TLS certificate is never retried either — it is a
  misconfiguration, not a blip.
- **Truncation detection**: a stream that ends without its terminal event is an
  error, not a short answer. This is easy to get wrong and expensive to
  discover.
- **Streaming tool calls**: arguments arrive as fragments and must be
  accumulated before the JSON is valid.
- **Cache-token semantics**: providers disagree on whether cached tokens are
  inside or additional to the input count. skyl normalises; a hand-rolled
  client usually does not, and the bill is where you find out.

---

## Common to every migration

**Errors are sentinels, not strings.**

```go
if errors.Is(err, skyl.ErrRateLimit) {
    // already retried with backoff before you saw it
}

var e *skyl.Error
if errors.As(err, &e) {
    log.Printf("%s returned %d", e.Provider, e.StatusCode)
}
```

Never branch on message text — providers reword messages and string matching
breaks silently when they do.

**Two escape hatches, always present.** `Request.ProviderOptions` sends what skyl
does not model; `Response.Raw` reads what it does not parse. You should never
need to fork.

⚠️ On OpenAI and Gemini, `ProviderOptions` is a **shallow** merge — setting one
nested field replaces the whole object. See the
[matrix](feature-matrix.md#provideroptions--read-this-before-using-it).

**Test without a credential.** `go run ./cmd/skyl-sandbox` starts a local server
speaking all four wire protocols, so your migration can be exercised end to end
before you spend anything. See [sandbox.md](sandbox.md).

## Before you commit to it

Read the [feature matrix](feature-matrix.md), particularly the
[silently ignored](feature-matrix.md#silently-ignored-the-important-column)
list. If something you depend on is in it, better to know now.

And read the [status section of the README](../README.md#status). Every adapter
has been exercised against its live provider API as of 2026-08-05, so the wire
mapping is confirmed rather than merely self-consistent. The project is still
pre-v1 and the Go API may change before `v1.0.0` — that is the part that should
factor into your decision now.
