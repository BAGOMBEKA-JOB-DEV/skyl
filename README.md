# skyl

**One Go interface for every AI model.**

skyl is a small, dependency-light Go library that lets you talk to Claude, GPT,
Gemini, and 400+ other models through a single, stable interface — then switch
between them by changing one string.

```go
client := skyl.New(anthropic.New(os.Getenv("ANTHROPIC_API_KEY")))

resp, err := client.Complete(ctx, &skyl.Request{
	Model:    "claude-opus-5",
	Messages: []skyl.Message{skyl.UserText("Explain Go channels in two sentences.")},
})
fmt.Println(resp.Text())
```

Swapping to GPT is a one-line change:

```go
client := skyl.New(openai.New(os.Getenv("OPENAI_API_KEY")))
```

---

## Why skyl

Integrating an AI model into a Go service means writing the same 400 lines every
time: request mapping, SSE parsing, retry with jitter, rate-limit backoff,
token accounting, error classification. Then your provider ships a new model, or
you want to A/B two vendors, and you write it again.

skyl does that once, properly, with tests.

| | |
|---|---|
| **Provider-agnostic** | One `Request`/`Response` shape across every vendor |
| **Zero-day model support** | Model IDs are pass-through strings — new models work the day they launch, with no skyl release |
| **No lock-in** | Every response carries `Raw` — the untouched provider JSON — so you are never blocked by our abstraction |
| **Streaming that works** | Unified SSE with proper context cancellation and no goroutine leaks |
| **Honest errors** | Typed, classified, `errors.Is`/`errors.As`-friendly |
| **Small surface** | The core module has zero external dependencies. No router, no logger, no framework |

## Install

```bash
go get github.com/BAGOMBEKA-JOB-DEV/skyl
```

Requires **Go 1.26+**. The core module has **zero external dependencies**.

The Anthropic adapter is a separate module, because it uses the official
Anthropic SDK and that brings a dozen transitive dependencies nobody else
should pay for ([ADR-0006](docs/adr/0006-anthropic-adapter-is-its-own-module.md)):

```bash
go get github.com/BAGOMBEKA-JOB-DEV/skyl/provider/anthropic
```

## Provider coverage

**Native adapters** — full fidelity, including provider-specific features
(extended thinking, prompt caching, native tool calling):

| Adapter | Reaches |
|---|---|
| `provider/anthropic` ⁽*⁾ | Claude Opus 5, Fable 5, Sonnet 5, Haiku 4.5, all 4.x |
| `provider/openai` | GPT-5.6 (Sol/Terra/Luna), 5.5, 5.4 nano, o-series, gpt-oss |
| `provider/gemini` | Gemini 3.6 Flash, 3.5/3.1 Flash-Lite, 3 Pro |

⁽*⁾ separate module — install it explicitly, as shown above.

**`provider/openaicompat`** — one adapter, configured per host, reaches the
long tail of vendors that speak OpenAI's wire format:

xAI (Grok) · DeepSeek · Mistral · Groq · Together · Fireworks · OpenRouter ·
Perplexity · Cerebras · DeepInfra · Qwen/DashScope · Moonshot (Kimi) ·
Z.ai (GLM) · Nvidia NIM · **Ollama** · **vLLM** · LM Studio · llama.cpp

```go
// Any OpenAI-compatible endpoint, including local ones.
client := skyl.New(openaicompat.New(
	openaicompat.WithBaseURL("http://localhost:11434/v1"),
	openaicompat.WithName("ollama"),
))
```

Full details and the model-discovery story: **[docs/providers.md](docs/providers.md)**.

> **On GitHub Copilot:** skyl deliberately does *not* ship a Copilot provider.
> Copilot has no public completions API — its REST endpoints are administrative
> only, and the Copilot SDK's BYOK mode forwards to other vendors' keys. A
> "Copilot provider" could only be a relabelled OpenAI call, which would be
> dishonest. See [ADR-0005](docs/adr/0005-no-copilot-provider.md).

## The optional gateway

`gateway/` is a **separate module** (its own `go.mod`) that exposes skyl over
HTTP using [go-chi](https://github.com/go-chi/chi) — one REST + SSE endpoint
that fans out to any configured provider. Use it when non-Go services need
model access, or when you want API keys held in exactly one place.

```bash
go run github.com/BAGOMBEKA-JOB-DEV/skyl/gateway/cmd/skyl-gateway
```

Importing the core library never pulls in chi. See
**[docs/gateway.md](docs/gateway.md)** and
[ADR-0003](docs/adr/0003-gateway-as-separate-module.md).

## Documentation

| Document | What it covers |
|---|---|
| [docs/idea.md](docs/idea.md) | The problem, the goals, and the explicit non-goals |
| [docs/getting-started.md](docs/getting-started.md) | Install → first call → streaming → tools |
| [docs/architecture.md](docs/architecture.md) | How the pieces fit and why |
| [docs/providers.md](docs/providers.md) | Provider coverage and model discovery |
| [docs/gateway.md](docs/gateway.md) | The chi HTTP gateway |
| [docs/project-plan.md](docs/project-plan.md) | Milestones, scope, and status |
| [docs/rules.md](docs/rules.md) | Engineering rules every change must satisfy |
| [docs/adr/](docs/adr/) | Architecture decision records |
| [CONTRIBUTING.md](CONTRIBUTING.md) | How to propose and land a change |
| [SECURITY.md](SECURITY.md) | Reporting vulnerabilities; credential handling |

## Status

**Pre-v1, and not yet validated against live provider APIs.** Everything here
is implemented, unit-tested, contract-tested, and CI-green — but every adapter
test replays payloads written from provider documentation, and no adapter has
yet made a real call. Treat it as ready to evaluate, not ready to depend on in
production.

A live test harness ships with the project and is the shortest path to closing
that gap. It needs your own credentials:

```bash
export ANTHROPIC_API_KEY=... OPENAI_API_KEY=... GEMINI_API_KEY=...
go test -tags=integration ./provider/
cd provider/anthropic && go test -tags=integration ./...
```

The API may also change ahead of the v1.0.0 tag. See
[docs/project-plan.md](docs/project-plan.md) for what is done and what is next,
and [CHANGELOG.md](CHANGELOG.md) for release history.

## License

Apache License 2.0 — see [LICENSE](LICENSE).
