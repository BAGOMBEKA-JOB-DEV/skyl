# Project plan

Living document. Updated as milestones land; the status table is the source of
truth for what actually exists.

## Milestones

### M0 — Foundation ✅

Repository, governance, and the decisions everything else depends on.

- [x] Rename `GOLANG-_APP` → `skyl`; module path `github.com/BAGOMBEKA-JOB-DEV/skyl`
- [x] `docs/` with idea, architecture, rules, providers, gateway, getting-started
- [x] ADRs 0001–0006 covering the load-bearing decisions
- [x] `CONTRIBUTING.md`, `SECURITY.md`, `CHANGELOG.md`
- [x] `main` as trunk; feature-branch workflow

### M1 — Core library ✅

The provider-agnostic types and the client that drives them.

- [x] `Message` / `Part` conversation model (text, image, tool call, tool result)
- [x] `Request` / `Response` / `Usage` / `StopReason`
- [x] `Provider` interface — the seam
- [x] `Client` with validation, retry, hooks, timeouts
- [x] Typed error classification (`ErrAuth`, `ErrRateLimit`, …)
- [x] Exponential backoff with full jitter and `Retry-After` support
- [x] `Stream` pull-iterator with leak-free cancellation
- [x] Internal SSE reader

### M2 — Provider adapters ✅

- [x] `provider/anthropic` — Claude, native (separate module, official SDK)
- [x] `provider/openai` — GPT, native
- [x] `provider/gemini` — Gemini, native
- [x] `provider/openaicompat` — generic, reaches the long tail
- [x] Live model discovery (`Models(ctx)`) on all four

### M3 — Test suite ✅

- [x] Table-driven unit tests, `httptest` fakes, no network
- [x] Error-path coverage per adapter
- [x] Goroutine-leak tests on streaming
- [x] `-race` clean

### M4 — Gateway ✅

- [x] Separate module with its own `go.mod`
- [x] chi v5 router, REST + SSE
- [x] Bearer auth on by default, request ID, panic recovery, structured logs
- [x] Provider wiring from environment
- [x] Handler tests

### M5 — CI ✅

- [x] Build, vet, test, race across all three modules
- [x] `gofmt` and `go mod tidy` enforcement
- [x] `golangci-lint` config
- [x] Coverage reported per module
- [ ] A coverage floor that fails the build
- [ ] Scheduled model-registry refresh job

### M6 — Hardening (next)

- [ ] `provider/cohere` — needs its own adapter, non-OpenAI wire format
- [ ] AWS Bedrock, Azure OpenAI, Vertex AI adapters
- [ ] Generated model registry with context window / pricing / modality
- [ ] Prompt caching surfaced uniformly where providers support it
- [ ] Benchmarks and allocation budgets on hot paths
- [ ] Fuzz tests on the SSE reader

### M7 — v1.0.0

- [ ] API frozen and reviewed
- [ ] Every adapter validated against live APIs
- [ ] Documentation complete, all examples compiling
- [ ] Semantic-versioning commitment published

## Under consideration

Not scheduled. Listed so the decisions are visible rather than implied.

| Idea | Status |
|---|---|
| `Agent` interface (Copilot SDK, Managed Agents) | Plausible v2. Genuinely different shape from `Completer` — see [ADR-0005](adr/0005-no-copilot-provider.md) |
| Router provider (failover, cost-based model selection) | Wanted. Composes as a `Provider` that wraps others, so it needs no core change |
| Embeddings | Different interface; likely `Embedder`, not bolted onto `Provider` |
| Middleware for caching responses | Fits the hook system already in `Client` |

## Explicitly out of scope

Agent orchestration and planning loops · prompt templating · vector storage and
RAG · fine-tuning management · a CLI chat client.

Rationale in [idea.md](idea.md#non-goals).

## Risks

| Risk | Mitigation |
|---|---|
| **Provider APIs change under us** | Pass-through model IDs and `ProviderOptions`/`Raw` escape hatches mean most vendor changes need no skyl release at all |
| **Abstraction can't express a needed feature** | Same escape hatches; `Provider` is small enough to implement out-of-tree |
| **Crowded field** — several Go multi-provider libraries exist | Differentiate on the things most of them skip: real tests, real docs, honest scope, no hidden dependencies |
| **Adapter drift as vendors add features** | Recorded-payload tests per adapter make drift a test failure rather than a production surprise |
| **Maintainer bandwidth** | Small core surface; the OpenAI-compatible adapter covers the long tail without per-vendor code |
