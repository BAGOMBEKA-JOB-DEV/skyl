# Changelog

All notable changes to this project are documented here.

Format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/); this
project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

Until v1.0.0, breaking changes may land in minor releases. They will always be
listed here with a migration note.

## [0.1.0] — unreleased

The first release. Everything below is the initial build of skyl: the repository
previously held an unrelated Go to-do application, which remains archived on the
`master` branch.

Release mechanics live in [RELEASING.md](RELEASING.md). The order is not
optional, and tags on the module proxy are immutable.

### Supported Go versions

- **Library** (`github.com/BAGOMBEKA-JOB-DEV/skyl`) — **Go 1.22+**. It has no
  dependencies, so it imposes no toolchain of its own.
- **`provider/anthropic` and `gateway`** — **Go 1.24+**, inherited from
  `anthropic-sdk-go`. Since Go 1.21 the `go` directive is a hard requirement, so
  this is not a choice; it is the floor the SDK sets.

CI builds every module against its own floor, so a directive that drifts from
what the code needs fails the build rather than reaching a user.

### Added

- **`skyl/otel`** — OpenTelemetry instrumentation as a fourth module,
  implementing the GenAI semantic conventions. One line to wire up:
  `skyl.New(provider, otel.Hook())`. Prompt content is never recorded.
  ([ADR-0007](docs/adr/0007-otel-is-its-own-module.md))
- **Streaming token usage is observable.** A terminal `stream_end` hook event
  reports it, including for a stream the caller abandoned — those tokens were
  generated and billed regardless, and reporting nothing made that spend
  invisible.
- **Gateway**: `/readyz` distinct from `/healthz`, `/metrics` in Prometheus
  format, graceful drain on SIGTERM, concurrency limiting, rotatable auth
  tokens with per-caller labels, CORS, SSE keep-alive frames,
  `X-Accel-Buffering: no`, an echoed `X-Request-Id`, and env vars for every
  `skyl.Option` — retries and timeouts were previously fixed at their defaults
  with no way for an operator to see or change them.
- **Supply chain**: `govulncheck`, CodeQL, OpenSSF Scorecard, Dependabot, SBOM
  and signed build provenance on release, digest-pinned actions, DCO
  enforcement, and a single `ci-ok` check for branch protection.
- **Governance**: `NOTICE`, per-module `LICENSE` files, `CODEOWNERS`,
  `MAINTAINERS.md`, `CODE_OF_CONDUCT.md`, and issue and PR templates.

**Core library** (`github.com/BAGOMBEKA-JOB-DEV/skyl`)

- `Provider` interface — `Name`, `Complete`, `Stream`, `Models`
- `Client` wrapping any provider with validation, retry, timeouts, and hooks
- Conversation model: `Message` with typed `Part`s — `Text`, `Image`,
  `ToolCall`, `ToolResult`
- `Request` / `Response` / `Usage` / `StopReason`, with `ProviderOptions` and
  `Response.Raw` as escape hatches
- Typed error classification: `ErrAuth`, `ErrRateLimit`, `ErrNotFound`,
  `ErrBadRequest`, `ErrServer`, `ErrUnsupported`, `ErrRefusal`, carried on
  `*Error` with provider, status, and retry-after
- Exponential backoff with full jitter, honouring `Retry-After`; only rate
  limits, server errors, and connection failures are retried
- `Stream` pull iterator with leak-free cancellation and early `Close()`
- Internal SSE reader

**Providers**

- `provider/anthropic` — Claude, native; a **separate module** built on the
  official Anthropic SDK, so its dependencies stay off everyone else's graph
- `provider/openai` — GPT, native
- `provider/gemini` — Gemini, native
- `provider/openaicompat` — generic adapter reaching ~18 OpenAI-compatible
  hosts including xAI, DeepSeek, Mistral, Groq, Together, Fireworks,
  OpenRouter, Ollama, vLLM, and LM Studio

**Gateway** (`github.com/BAGOMBEKA-JOB-DEV/skyl/gateway`, separate module)

- chi v5 router: `POST /v1/chat`, `POST /v1/chat/stream` (SSE),
  `GET /v1/models`, `GET /v1/providers`, `GET /healthz`
- Mandatory bearer auth with constant-time comparison; the server refuses to
  start without a token
- Request ID, panic recovery, structured `log/slog` logging, timeouts
  (chi's spoofable `RealIP` is deliberately excluded)
- Provider registration from environment variables

**Sandbox** (`cmd/skyl-sandbox`)

- A local server speaking all four providers' wire protocols — Anthropic
  Messages, OpenAI chat-completions, Gemini generateContent, and the
  OpenAI-compatible shape — with no credentials and no cost
- Streaming, model discovery, per-provider auth headers, and per-provider error
  shapes, so adapters behave against it as they would against the real host
- On-demand failures: a model ID of `sandbox-status-429` returns that status,
  which is how the retry and classification paths are exercised
- A third test suite, `-tags=sandbox`, running the same checks as the live
  suite over real sockets. It needs no credential, so CI runs it on every
  change — but it cannot prove field names are correct, because the sandbox was
  written from the same documentation as the adapters
  ([docs/sandbox.md](docs/sandbox.md))

**Documentation**

- `docs/`: idea, architecture, getting-started, providers, gateway,
  project plan, engineering rules
- ADRs 0001–0006 recording the load-bearing decisions
- `CONTRIBUTING.md`, `SECURITY.md`

### Changed

- **The gateway's chat wire format is redesigned.** `ChatMessage.content` was a
  string and is now a list of typed parts; a text-only turn may use the new
  `text` shorthand instead.

  **Migration:** `{"role":"user","content":"hi"}` becomes
  `{"role":"user","text":"hi"}`. A tool result becomes an explicit part:
  `{"role":"tool","content":[{"type":"tool_result","tool_call_id":"c1","content":"..."}]}`.

  This is what makes a tool-calling loop possible at all. The old format could
  not express an assistant turn containing tool calls, so a client received one
  in the response and had no way to send it back — and every provider rejects a
  tool result that does not follow the call it answers. The response now also
  carries `message`, the assistant's turn in the same shape a request takes, so
  it can be appended and replayed verbatim. `tool_choice`, `thinking`, images
  and tool-error results are reachable over HTTP for the first time.

  Note `DisallowUnknownFields` is still on: a *new* client against an *old*
  gateway gets a 400 rather than a silent ignore, so upgrade gateways first.

- **`HookEvent` gained fields and a new operation.** `ResponseID`,
  `ResponseModel`, `StopReason`, `Completed` and `Request`, plus the
  `stream_end` operation. Existing hooks keep working; they simply see one more
  event per stream. `Request` carries the prompt — a hook that logs it verbatim
  ships conversation content wherever the logs go.

- **The gateway module's Go floor rises to 1.25**, inherited from the
  OpenTelemetry SDK by way of `skyl/otel`. The library itself is unchanged at
  1.22, which is the point of the module split.

- **`Usage` now defines its inclusion semantics, and `Usage.TotalTokens()` no
  longer double-counts cached tokens.** `InputTokens` is the total input
  *including* anything served from or written to a cache; `CacheReadTokens` and
  `CacheWriteTokens` are a breakdown of it rather than an addition to it.
  `TotalTokens()` is therefore `InputTokens + OutputTokens`.

  This is a behavioural change for anyone already reading `TotalTokens()`.
  Previously OpenAI and Gemini over-reported — their wire formats put cached
  tokens inside the prompt count, so the cache figure was added twice — while
  Anthropic under-reported `InputTokens`, because its wire format excludes the
  cache counters and skyl passed them straight through. The same cached
  conversation reported a different billable input depending on which provider
  served it. **Migration:** if you were computing
  `InputTokens + CacheReadTokens` yourself, drop the addition.

- **A provider's `Retry-After` is no longer clamped to the backoff ceiling.**
  It now has its own bound, configurable with `WithRetryAfterCap` and
  defaulting to 5 minutes. `WithRetryDelay`'s max continues to cap only skyl's
  computed backoff.

- **`*Error` now wraps its underlying cause** as well as its sentinel, so
  `errors.Is(err, context.DeadlineExceeded)` works on a request that timed out.
  `Unwrap` consequently returns `[]error` rather than `error`; `errors.Is` and
  `errors.As` are unaffected.

### Added

- **`skyl/otel`** — OpenTelemetry instrumentation as a fourth module,
  implementing the GenAI semantic conventions. One line to wire up:
  `skyl.New(provider, otel.Hook())`. Prompt content is never recorded.
  ([ADR-0007](docs/adr/0007-otel-is-its-own-module.md))
- **Streaming token usage is observable.** A terminal `stream_end` hook event
  reports it, including for a stream the caller abandoned — those tokens were
  generated and billed regardless, and reporting nothing made that spend
  invisible.
- **Gateway**: `/readyz` distinct from `/healthz`, `/metrics` in Prometheus
  format, graceful drain on SIGTERM, concurrency limiting, rotatable auth
  tokens with per-caller labels, CORS, SSE keep-alive frames,
  `X-Accel-Buffering: no`, an echoed `X-Request-Id`, and env vars for every
  `skyl.Option` — retries and timeouts were previously fixed at their defaults
  with no way for an operator to see or change them.
- **Supply chain**: `govulncheck`, CodeQL, OpenSSF Scorecard, Dependabot, SBOM
  and signed build provenance on release, digest-pinned actions, DCO
  enforcement, and a single `ci-ok` check for branch protection.
- **Governance**: `NOTICE`, per-module `LICENSE` files, `CODEOWNERS`,
  `MAINTAINERS.md`, `CODE_OF_CONDUCT.md`, and issue and PR templates.

- **The sandbox calls tools**, on all three wire protocols, streaming and not.
  Arguments are split mid-token across frames, so an adapter that fails to
  accumulate them cannot pass; `tool_choice` is honoured, and the second turn
  answers using the tool's own output. Tool calling was the least validated
  path in the library — the entire Tools and ToolChoice mapping blocks in
  `internal/oai` and `provider/gemini` had never been executed by any test.
- **Mid-stream faults.** Two model IDs, `sandbox-stream-truncate` and
  `sandbox-stream-error`, make a stream fail *after* it has started — which
  `sandbox-status-NNN` structurally cannot, since the status is fixed once the
  SSE header is written. Every adapter's mid-stream error and truncation
  handling was previously unreachable.
- `internal/cassette` — records real provider HTTP exchanges and replays them,
  standard library only. One contributor with a key records; everyone else
  replays offline, forever. Credentials are scrubbed on write (rules.md §7.2),
  and a test walks every committed fixture looking for credential-shaped
  strings. Replay tests are untagged, so they begin asserting in ordinary CI as
  soon as a recording lands.
- **`Example` functions** (rules.md §8.3, previously zero) — twelve runnable,
  with verified output.
- **Benchmarks** on the per-token paths: SSE frame parsing, per-chunk JSON
  decode, tool-argument accumulation, and payload construction.
- `docs/roadmap.md` — what stands between this and production use, from an
  audit of the gap between "CI is green" and "a company can adopt this".
- `RELEASING.md` and `scripts/release.sh` — the multi-module release process,
  and a script that performs the go.mod rewrites without ever tagging or
  pushing. CI refuses a tag whose modules still carry a `replace` directive or
  a `v0.0.0` require: either one publishes a module nobody can install, and the
  proxy will serve it forever.
- `go.work` — a workspace for local development across the three modules,
  replacing the `replace` directives. Go never consults it when skyl is a
  dependency, so unlike a `replace` it cannot leak into a published module. CI
  builds with `GOWORK=off` so each module is proven to resolve on its own.
- `WithRetryAfterCap` bounds how long a provider may hold a retry.
- The shared contract suite now asserts that every adapter honours
  `Request.ProviderOptions`, so the rule cannot be met by one adapter and
  quietly missed by another.
- `Request.Thinking` is mapped for Gemini, including `&Thinking{Enabled:false}`
  as a zero thinking budget.

### Fixed

- **`provider/anthropic` silently ignored `Request.ProviderOptions`**, in
  violation of the rule that every adapter must honour it (rules.md §6.5). The
  adapter builds a typed SDK params struct, so there was no map to merge into
  and the field was simply never read — leaving `cache_control`, `top_k`, and
  every beta feature unreachable on Anthropic, with no workaround. Options are
  now applied to the encoded body, so caller values override skyl's.
- **`provider/anthropic` rebuilt tool schemas lossily**, keeping only
  `properties` and `required`. `$defs`, `$ref`, `oneOf`, and
  `additionalProperties` were dropped, so a schema generated from a Go struct
  or an OpenAPI document reached Anthropic with dangling references while
  reaching OpenAI intact — the same `skyl.Tool` meaning two different things
  depending on the provider.
- **The OpenAI-format adapters dropped assistant text when a host returned
  content as an array of blocks** rather than a bare string. vLLM, some Azure
  deployments, and several OpenRouter upstreams do exactly that, and the result
  was a successful response with empty text on both the completion and
  streaming paths.
- **A stream cut short was indistinguishable from a complete one.** A
  connection dropped mid-generation reaches EOF with no reader error, so all
  three adapters emitted a clean terminal event over a partial answer. They now
  report a truncated response, while still delivering the text read so far.
- **A refusal with no content was reported as success.** `content_filter` and
  `refusal` mapped to `StopRefusal` but returned no error, so `ErrRefusal` was
  never produced by any adapter and the gateway's 422 branch was unreachable. A
  refusal that *does* carry text is still returned normally.
- **Certificate failures were retried.** A rejected certificate is a
  misconfiguration, not a blip; retrying spent the whole budget to receive the
  same answer and delayed the error the operator needed to see.
- **Streaming tool-call arguments accumulated quadratically.** Fragments were
  joined with `+=`, which reallocates and copies the whole accumulated string
  on every frame — and providers send a frame every few characters. A call with
  512 fragments allocated 2.2 MB to assemble a few kilobytes. It now uses a
  `strings.Builder`: 34 KB and 19 allocations for the same input, and linear
  rather than quadratic. Found by the new benchmark, which stays as the guard.

Both of the following were found by widening test coverage before the first
release, so neither ever shipped.

- `provider/anthropic` omitted `input_schema` from a tool declared without
  parameters. The SDK's schema struct drops itself when every field is zero,
  and the API rejects a tool without a schema — so any parameterless tool
  failed the whole request with a 400.
- `provider/anthropic` silently dropped a tool schema's `required` list unless
  it was a Go `[]string`. A schema loaded from JSON yields `[]any`, so callers
  reading their schemas from a file lost every required-argument constraint.

### Notes

- **Model IDs are opaque pass-through strings.** skyl ships no model-name
  constants and never validates a model against a list, so models released
  after your skyl build work immediately.
  ([ADR-0004](docs/adr/0004-model-ids-are-pass-through.md))
- **There is no GitHub Copilot provider.** Copilot exposes no completions API;
  a provider package could only be a relabelled call to another vendor.
  ([ADR-0005](docs/adr/0005-no-copilot-provider.md))
- **chi is used in the gateway module only.** Importing the core library does
  not pull in a router.
  ([ADR-0003](docs/adr/0003-gateway-as-separate-module.md))

[0.1.0]: https://github.com/BAGOMBEKA-JOB-DEV/skyl/commits/main
