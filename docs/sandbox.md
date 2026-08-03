# The sandbox

`skyl-sandbox` serves every provider's wire protocol locally. No credentials,
no cost, no network.

```bash
go run ./cmd/skyl-sandbox
```

```
skyl sandbox listening on http://127.0.0.1:8099
  api key       sandbox-key
  anthropic     http://127.0.0.1:8099/anthropic
  openai        http://127.0.0.1:8099/openai/v1
  gemini        http://127.0.0.1:8099/gemini/v1beta
  openaicompat  http://127.0.0.1:8099/compat/v1
```

Point an adapter at a mount and it behaves exactly as it would against the real
host:

```go
p := openai.New("sandbox-key",
    openai.WithBaseURL("http://127.0.0.1:8099/openai/v1"))

resp, err := skyl.New(p).Complete(ctx, &skyl.Request{
    Model:    "gpt-5.6",
    Messages: []skyl.Message{skyl.UserText("What is the capital of France?")},
})
// resp.Text() == "Paris"
```

## What it is for

Two things.

**Developing without a key.** Build and run your integration against skyl
before you have provisioned anything, and without a real credential sitting in
a development environment.

**Testing the parts a unit test cannot reach.** Every adapter unit test in this
repository uses an in-process fake. That covers the mapping, but it skips the
socket — and a surprising amount lives on the socket: chunked SSE arriving in
pieces, connection reuse, status codes, `Retry-After`, cancellation landing
mid-backoff. The sandbox makes all of that real.

## What it is not

**It is not evidence that skyl talks to real providers correctly.**

The sandbox was written from the same provider documentation as the adapters.
If skyl has a field name wrong, the sandbox almost certainly has it wrong in
exactly the same way, and both agree while both are wrong. This is the central
limitation and no amount of sandbox testing removes it.

Only the live suite settles that question, and it needs your own key:

```bash
export ANTHROPIC_API_KEY=... OPENAI_API_KEY=... GEMINI_API_KEY=...
go test -tags=integration ./provider/
cd provider/anthropic && go test -tags=integration ./...
```

There is also no model here. Replies come from a lookup table, and token counts
are word counts — enough to prove usage is parsed and carried, useless for
reasoning about cost or quality.

## The three test suites

| Command | Needs | Runs in CI | Proves |
|---|---|---|---|
| `go test ./...` | nothing | yes | mapping logic, in process |
| `go test -tags=sandbox ./...` | nothing | yes | the full stack over real sockets |
| `go test -tags=integration ./...` | real API keys, money | no | that the field names are actually right |

The first two are the ladder CI climbs. The third is the one only you can run,
and it is the one that decides whether skyl is ready to depend on.

## Forcing failures

A model ID of `sandbox-status-<code>` returns that HTTP status in the
provider's own error shape:

```go
_, err := client.Complete(ctx, &skyl.Request{
    Model:    "sandbox-status-429",
    Messages: []skyl.Message{skyl.UserText("hi")},
})
// errors.Is(err, skyl.ErrRateLimit) == true
```

This is how the retry path is exercised on demand rather than by waiting for a
provider to rate-limit you. `429` also carries a `Retry-After` header, so
backoff's preference for the provider's own hint is covered too.

Anything outside 100–599, or a non-numeric suffix, is treated as an ordinary
model name and gets the usual not-found response.

## Model catalogue

Each mount serves a small fixed list, and rejects anything else with that
provider's 404. That is deliberate: skyl passes model IDs through unvalidated
([ADR-0004](adr/0004-model-ids-are-pass-through.md)), so the provider's own
not-found error is the only thing standing between a typo and an unactionable
failure — and a sandbox that accepted every string would never exercise it.

| Mount | Models |
|---|---|
| anthropic | `claude-opus-5`, `claude-sonnet-5`, `claude-haiku-4-5` |
| openai, compat | `gpt-5.6`, `gpt-5.4-nano` |
| gemini | `gemini-3.6-flash`, `gemini-3.6-pro` |

## Credentials

The sandbox accepts `sandbox-key` by default; override with `-api-key`. Each
mount checks the header its real counterpart uses — `x-api-key` for Anthropic,
`Authorization: Bearer` for OpenAI, `x-goog-api-key` for Gemini — so the
adapters' credential handling is genuinely exercised.

The OpenAI-compatible mount also accepts **no credential at all**, because that
is how Ollama, LM Studio, and llama.cpp behave and `provider/openaicompat`
exists to reach them.

## A warning

This is a development tool. It authenticates nothing meaningfully, and it
binds to loopback by default for that reason. Do not expose it to a network you
do not control.
