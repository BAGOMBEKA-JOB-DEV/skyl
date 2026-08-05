# Providers and models

## How skyl handles models

**skyl does not maintain a list of valid model IDs.** `Request.Model` is an
opaque string passed straight to the provider.

This is the most consequential decision in the project. The alternative — a
curated enum or validation table — guarantees that sooner or later skyl rejects
a model that you are entitled to use, because the model shipped and skyl hasn't
cut a release. Consider a four-week window in mid-2026:

| Model | Released |
|---|---|
| GPT-5.6 Sol | 9 Jul 2026 |
| Gemini 3.6 Flash | 21 Jul 2026 |
| Claude Opus 5 | 24 Jul 2026 |
| Qwen3.7 Flash | 27 Jul 2026 |

Any hardcoded table is wrong within weeks. Pass-through is correct forever.
Recorded as [ADR-0004](adr/0004-model-ids-are-pass-through.md).

### Discovering what's available

For the live list, ask the provider:

```go
models, err := client.Models(ctx)
for _, m := range models {
	fmt.Println(m.ID)
}
```

That hits the provider's real models endpoint. It is always current, because it
is not skyl's opinion — it is the provider's answer.

### Model metadata

Context window, pricing, and modality are genuinely useful and *not* available
from every provider's models endpoint. skyl's plan (M6) is a **generated**
registry: a scheduled CI job queries live provider endpoints and regenerates a
committed JSON + Go file. Generated, never hand-typed, so it cannot silently
rot. Until it lands, `ModelInfo` carries what the provider returns.

---

## Native adapters

Full fidelity, including vendor-specific capabilities.

### `provider/anthropic`

This adapter is a **separate module** — it uses the official Anthropic SDK,
which brings a dozen transitive dependencies that users of other providers
should not inherit ([ADR-0006](adr/0006-anthropic-adapter-is-its-own-module.md)):

```bash
go get github.com/BAGOMBEKA-JOB-DEV/skyl/provider/anthropic
```

```go
import "github.com/BAGOMBEKA-JOB-DEV/skyl/provider/anthropic"

p := anthropic.New(os.Getenv("ANTHROPIC_API_KEY"))
```

Reaches Claude Opus 5, Fable 5, Sonnet 5, Haiku 4.5, and the 4.x family.
Supports system prompts as a top-level field, typed content blocks, tool use,
and extended/adaptive thinking via `Request.Thinking`.

### `provider/openai`

```go
import "github.com/BAGOMBEKA-JOB-DEV/skyl/provider/openai"

p := openai.New(os.Getenv("OPENAI_API_KEY"))
```

Reaches the GPT-5.6 family (Sol, Terra, Luna), GPT-5.5, GPT-5.4 nano, the
o-series, and gpt-oss.

### `provider/gemini`

```go
import "github.com/BAGOMBEKA-JOB-DEV/skyl/provider/gemini"

p := gemini.New(os.Getenv("GEMINI_API_KEY"))
```

Reaches Gemini 3.6 Flash, 3.5/3.1 Flash-Lite, and 3 Pro. The adapter handles
Gemini's structural differences — `contents`/`parts`, the `model` role in place
of `assistant`, and `systemInstruction` — so your `Request` doesn't have to.

---

## `provider/openaicompat` — the long tail

A large part of the industry serves OpenAI's wire format. One adapter reaches
all of it:

```go
import "github.com/BAGOMBEKA-JOB-DEV/skyl/provider/openaicompat"

p := openaicompat.New(
	openaicompat.WithBaseURL("https://api.groq.com/openai/v1"),
	openaicompat.WithAPIKey(os.Getenv("GROQ_API_KEY")),
	openaicompat.WithName("groq"),
)
```

### Verified endpoint reference

| Provider | Base URL |
|---|---|
| xAI (Grok) | `https://api.x.ai/v1` |
| DeepSeek | `https://api.deepseek.com/v1` |
| Mistral | `https://api.mistral.ai/v1` |
| Groq | `https://api.groq.com/openai/v1` |
| Together | `https://api.together.xyz/v1` |
| Fireworks | `https://api.fireworks.ai/inference/v1` |
| OpenRouter | `https://openrouter.ai/api/v1` |
| Perplexity | `https://api.perplexity.ai` |
| Cerebras | `https://api.cerebras.ai/v1` |
| DeepInfra | `https://api.deepinfra.com/v1/openai` |
| Qwen / DashScope | `https://dashscope.aliyuncs.com/compatible-mode/v1` |
| Moonshot (Kimi) | `https://api.moonshot.cn/v1` |
| Z.ai (GLM) | `https://open.bigmodel.cn/api/paas/v4` |
| Nvidia NIM | `https://integrate.api.nvidia.com/v1` |
| Ollama (local) | `http://localhost:11434/v1` |
| vLLM (self-hosted) | `http://localhost:8000/v1` |
| LM Studio (local) | `http://localhost:1234/v1` |
| llama.cpp (local) | `http://localhost:8080/v1` |

**OpenRouter alone brokers 300+ models**, so this single adapter puts the
realistic reachable total in the hundreds — without skyl shipping per-vendor
code, and without a release when any of them adds a model.

### Fidelity caveat

These endpoints implement OpenAI's *format*, not necessarily its *features*.
Tool calling, streaming, and multimodal support vary by host and by model.
skyl surfaces what the endpoint returns; where a host rejects a feature, you
get that host's error, classified, rather than a skyl-invented one.

**Structured output is the sharpest instance.** `Request.ResponseFormat` sends
`response_format: {"type": "json_schema", …}` with `strict: true`, which OpenAI
guarantees. Many compatible hosts implement only the older
`{"type": "json_object"}` — plain JSON mode with no schema — and either reject
`json_schema` outright or, worse, accept it and ignore the schema. Test it
against the host you are using before you depend on it.

Use a native adapter when you want a vendor's deep features. Use
`openaicompat` for reach.

---

## GitHub Copilot

**skyl does not ship a Copilot provider, and this is deliberate.**

The research is unambiguous:

- GitHub's REST Copilot endpoints are **administrative only** — seat
  assignment, org metrics, content-exclusion policy. There is no
  completions endpoint.
- The [Copilot SDK](https://github.com/github/copilot-sdk) (public preview,
  April 2026) is an **agent runtime**, a fundamentally different shape from a
  completions call, and it requires a Copilot subscription.
- Its BYOK mode forwards to **other providers' keys** — so a skyl "Copilot
  provider" built on it would loop straight back through skyl.

A `provider/copilot` package could therefore only be an OpenAI call wearing a
different label. Users would make architectural and licensing decisions based
on that label, and it would be false. See
[ADR-0005](adr/0005-no-copilot-provider.md).

The Copilot **agent** runtime is a real, interesting capability — it just needs
an `Agent` interface rather than `Completer`. Tracked as a possible v2 in the
[project plan](project-plan.md#under-consideration).

---

## Writing your own adapter

`Provider` is four methods, and nothing in skyl privileges the in-tree
adapters. An adapter in your own repository is a first-class citizen:

```go
type Provider interface {
	Name() string
	Complete(ctx context.Context, req *skyl.Request) (*skyl.Response, error)
	Stream(ctx context.Context, req *skyl.Request) (skyl.Stream, error)
	Models(ctx context.Context) ([]skyl.ModelInfo, error)
}
```

Implement it, pass it to `skyl.New`, and you inherit retry, hooks, validation,
and the gateway for free. If you build one worth sharing, see
[CONTRIBUTING.md](../CONTRIBUTING.md).
