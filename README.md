# skyl

[![CI](https://github.com/BAGOMBEKA-JOB-DEV/skyl/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/BAGOMBEKA-JOB-DEV/skyl/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/BAGOMBEKA-JOB-DEV/skyl.svg)](https://pkg.go.dev/github.com/BAGOMBEKA-JOB-DEV/skyl)
[![License: Apache 2.0](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](LICENSE)

**One Go interface for every AI model.**

📖 **[skyl-docs.vercel.app](https://skyl-docs.vercel.app/)** — the documentation site.

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

Requires **Go 1.22+**, and the core module has **zero external dependencies** —
so it imposes neither a dependency graph nor a recent toolchain on you. The
adapter modules below need **Go 1.24+**, because the vendor SDKs they wrap do.

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

## Deploying it

The gateway builds as a multi-architecture container image —
`gcr.io/distroless/static:nonroot`, no shell, configuration entirely from the
environment. Build and run it from a clone:

```bash
docker build -t skyl-gateway .
docker run --rm -p 8080:8080 \
  -e SKYL_AUTH_TOKEN="$(openssl rand -hex 32)" \
  -e ANTHROPIC_API_KEY=sk-ant-... \
  skyl-gateway
```

Or with no API key at all — `docker compose up --build` runs it against the
local [sandbox](docs/sandbox.md), which speaks every provider's wire protocol.

`.github/workflows/publish-image.yml` publishes to
`ghcr.io/bagombeka-job-dev/skyl-gateway` on each `gateway/v*` tag, signed with
cosign and carrying an SBOM and a build-provenance attestation. **No image has
been published yet** — the workflow postdates the existing `gateway/v0.1.0` tag,
so it has not had a tag push to run on. Trigger it manually from the Actions tab,
or on the next release.

For a cluster,
**[BAGOMBEKA-JOB-DEV/skyl_infrastructure](https://github.com/BAGOMBEKA-JOB-DEV/skyl_infrastructure)**
is a separate repository holding Terraform that stands up **AWS, GCP or Azure**
behind one interface, and a Helm chart that runs identically on all three. It
encodes the operational details that are easy to get wrong and expensive to
discover in production — liveness on `/healthz` and readiness on `/readyz`, a
40-second termination grace period for the drain described in
[docs/gateway.md](docs/gateway.md), secrets from the cloud's own secret store
rather than from git, and images pinned by digest so a rollback lands on the
bytes that were signed.

Deployment lives in its own repository for the same reason the docs site does:
so that infrastructure changes never touch this repository's release history.

## The three repositories

| Repository | What it is |
|---|---|
| **[skyl](https://github.com/BAGOMBEKA-JOB-DEV/skyl)** | This one — the Go library, the adapters, and the gateway |
| [skyl_docs](https://github.com/BAGOMBEKA-JOB-DEV/skyl_docs) | The [documentation site](https://skyl-docs.vercel.app/) (Next.js) |
| [skyl_infrastructure](https://github.com/BAGOMBEKA-JOB-DEV/skyl_infrastructure) | Deployment: Terraform for three clouds, the Helm chart, CI |

## Documentation

The **[documentation site](https://skyl-docs.vercel.app/)** is the friendliest
way in: a *Learn* track to read in order, and a *Reference* track with one page
per symbol. It is built from
[BAGOMBEKA-JOB-DEV/skyl_docs](https://github.com/BAGOMBEKA-JOB-DEV/skyl_docs) —
a separate repository, so a docs change never touches the library's release
history. Corrections to the site belong there; corrections to the files below
belong here.

The documents in this repository stay authoritative for anything a decision
depends on — the feature matrix, the threat model, the ADRs — because they are
versioned with the code they describe.

| Document | What it covers |
|---|---|
| [docs/idea.md](docs/idea.md) | The problem, the goals, and the explicit non-goals |
| [docs/getting-started.md](docs/getting-started.md) | Install → first call → streaming → tools |
| [docs/architecture.md](docs/architecture.md) | How the pieces fit and why |
| [docs/providers.md](docs/providers.md) | Provider coverage and model discovery |
| [docs/gateway.md](docs/gateway.md) | The chi HTTP gateway |
| [docs/project-plan.md](docs/project-plan.md) | Milestones, scope, and status |
| [docs/roadmap.md](docs/roadmap.md) | What stands between this and production use |
| [docs/feature-matrix.md](docs/feature-matrix.md) | Which adapter supports what — including what each one silently ignores |
| [docs/data-handling.md](docs/data-handling.md) | What leaves your process, what is kept, what is logged |
| [docs/threat-model.md](docs/threat-model.md) | Trust boundaries, and what an authenticated gateway caller can do |
| [docs/benchmarks.md](docs/benchmarks.md) | Measured allocation and throughput figures |
| [docs/migrating.md](docs/migrating.md) | Coming from openai-go, anthropic-sdk-go, or langchaingo |
| [docs/rules.md](docs/rules.md) | Engineering rules every change must satisfy |
| [docs/adr/](docs/adr/) | Architecture decision records |
| [CONTRIBUTING.md](CONTRIBUTING.md) | How to propose and land a change |
| [SECURITY.md](SECURITY.md) | Reporting vulnerabilities; credential handling |

Deployment is documented in the infrastructure repository rather than here:
[architecture and quickstart](https://github.com/BAGOMBEKA-JOB-DEV/skyl_infrastructure#readme),
the [cluster runbook](https://github.com/BAGOMBEKA-JOB-DEV/skyl_infrastructure/blob/main/docs/runbook.md),
and [what each cloud costs](https://github.com/BAGOMBEKA-JOB-DEV/skyl_infrastructure/blob/main/docs/cost.md).

## Status

**Pre-v1, and validated against live provider APIs.** Everything here is
implemented, unit-tested, contract-tested, exercised end to end over real
sockets, and CI-green — and as of **2026-08-05** the integration suite has been
run against the real OpenAI, Anthropic and Gemini endpoints and passes.

That last part is the one that took longest to be able to write. Every fake in
this test suite was written from the same provider documentation as the adapter
it tests, so a wrong field name is wrong identically in both and CI stays green
regardless. Only a real call settles it, and one has now been made against each
adapter: completions, streaming, tool calls, the multi-turn tool round trip,
truncation, and error classification.

**Pre-v1 still means what it says.** The wire mapping is confirmed; the Go API
is not frozen and may change before `v1.0.0` (`docs/rules.md` §1.2). Live
validation is a snapshot, not a subscription — providers change, and
[docs/validating.md](docs/validating.md) is how you re-run it yourself against
your own account and models.

There are three suites, and the third is the one CI cannot run for you:

```bash
go test ./...                  # mapping logic, in process
go test -tags=sandbox ./...    # the full stack over real sockets — no keys needed
go test -tags=integration ./...# real providers — needs your keys, costs money
```

CI runs the first two on every change. The third needs credentials, so it runs
by hand — reproduce it with your own:

```bash
export ANTHROPIC_API_KEY=... OPENAI_API_KEY=... GEMINI_API_KEY=...
go test -tags=integration ./provider/
cd provider/anthropic && go test -tags=integration ./...
```

Eight checks per provider, about nine upstream requests, pennies on a small
model. [docs/validating.md](docs/validating.md) covers what each one proves and
how to tell an adapter bug from a model being unhelpful.

To develop against skyl before you have any key, run the local sandbox — it
speaks all four providers' wire protocols and costs nothing. See
[docs/sandbox.md](docs/sandbox.md).

```bash
go run ./cmd/skyl-sandbox
```

The API may also change ahead of the v1.0.0 tag. See
[docs/project-plan.md](docs/project-plan.md) for what is done and what is next,
and [CHANGELOG.md](CHANGELOG.md) for release history.

## License

Apache License 2.0 — see [LICENSE](LICENSE).
