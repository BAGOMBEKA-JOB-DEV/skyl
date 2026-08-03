# Changelog

All notable changes to this project are documented here.

Format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/); this
project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

Until v1.0.0, breaking changes may land in minor releases. They will always be
listed here with a migration note.

## [Unreleased]

The initial build of skyl: the repository previously held an unrelated Go
to-do application, which remains archived on the `master` branch.

### Added

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

### Fixed

Both found by widening test coverage before the first release, so neither ever
shipped.

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

[Unreleased]: https://github.com/BAGOMBEKA-JOB-DEV/skyl/commits/main
