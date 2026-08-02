# The idea behind skyl

## The problem

Every Go team that adds an AI feature writes the same code.

Not similar code — the *same* code. Request struct mapping. SSE frame parsing.
Exponential backoff that respects `Retry-After`. Distinguishing a rate limit
from a bad API key from a model that no longer exists. Token accounting for the
finance dashboard. Context cancellation that doesn't leak the goroutine reading
the stream.

That is roughly 400–600 lines before you write a single line of product logic.
Then one of four things happens:

1. **A better model ships.** You want to try it. Your code has the old model ID
   baked into three files and a config struct.
2. **You want to compare vendors.** Claude for reasoning, a cheap model for
   classification. Now you maintain two integrations that drift apart.
3. **Pricing changes.** You want to move 80% of traffic to a cheaper provider.
   Your request-building code is coupled to one vendor's JSON shape.
4. **You need to run locally.** Ollama on a laptop for development, a hosted
   frontier model in production. Two code paths, one of which is under-tested.

Each of those is a rewrite, and each rewrite reintroduces the same bugs.

## The goal

**Write your AI integration once. Change models with a string.**

skyl is the layer that absorbs vendor differences so your application code
doesn't have to. A `skyl.Request` looks the same whether it ends up at
Anthropic, OpenAI, Google, or a container running on your own hardware.

## Design principles

These are the commitments that shape every decision in this repository. When a
tradeoff is unclear, the principle higher in this list wins.

### 1. Never block the user from a model

A library that maintains a hardcoded list of valid model IDs will, inevitably,
reject a model that its user is entitled to use — because the model shipped
last Tuesday and the library hasn't cut a release.

**Model IDs are opaque strings that skyl passes straight through.** A model
released after your skyl version was built works immediately. This is the single
most important design decision in the project, and
[ADR-0004](adr/0004-model-ids-are-pass-through.md) records it.

Metadata *about* models (context window, pricing, modality) is genuinely useful,
so skyl offers it — but as an optional, **generated** registry refreshed from
live provider endpoints by CI, never a hand-typed table that silently rots.

### 2. The abstraction must be escapable

Every abstraction over a fast-moving API is wrong somewhere. If skyl's
`Request` can't express the thing you need, skyl must not be the reason you
can't ship.

Two escape hatches, always present:

- `Request.ProviderOptions` — arbitrary vendor-specific fields, merged into the
  outbound payload.
- `Response.Raw` — the untouched provider JSON, so you can read a field skyl
  doesn't model yet.

You should never have to fork skyl to use a feature.

### 3. Honesty over coverage

It is better to say "we don't support this" than to ship something that looks
supported and quietly does the wrong thing.

This is why there is no Copilot provider
([ADR-0005](adr/0005-no-copilot-provider.md)): Copilot has no completions API,
so any "Copilot provider" would secretly be an OpenAI call wearing a different
name. Users would make architectural decisions based on a lie.

### 4. Dependencies are a tax on every user

Every dependency in the core module is imposed on everyone who imports it,
forever, including their security scanners and their upgrade schedule.

The core module takes only the provider SDKs it actually needs. No logging
framework, no config library, no HTTP router. The chi-based gateway is
genuinely useful, which is exactly why it lives in a
[separate module](adr/0003-gateway-as-separate-module.md) with its own
`go.mod` — so that people who want a library get a library.

### 5. Tested, or it doesn't exist

An "enterprise-grade" library is not one with the word *enterprise* in the
README. It is one where every adapter has tests against recorded provider
responses, error paths are tested as thoroughly as success paths, and the
suite runs clean under `-race`.

See [rules.md](rules.md) for the standards a change must meet.

## Non-goals

Being explicit about what skyl is *not* keeps the surface small and honest.

| Not a goal | Why |
|---|---|
| **An agent framework** | Planning loops, memory, and orchestration are opinionated and change fast. skyl is the transport layer they sit on. |
| **A prompt-template engine** | Go has `text/template`. |
| **A vector database or RAG stack** | Different problem, different library. |
| **Hiding provider differences entirely** | Some differences are real and matter. skyl unifies the common 90% and exposes the rest rather than pretending it away. |
| **Supporting every model at full fidelity** | Native adapters get deep support; the long tail is reached through the OpenAI-compatible adapter at whatever fidelity that endpoint offers. Documented, not disguised. |

## Who this is for

- Go teams adding AI features who don't want to own vendor plumbing.
- Teams running **multi-provider** setups for cost, latency, or redundancy.
- Anyone who develops against a local model and deploys against a hosted one.
- Platform teams who need a **single audited egress point** for model traffic —
  that is what the gateway is for.

## Who this is not for

If you use exactly one model from exactly one vendor and always will, use that
vendor's official SDK. It will always support their newest feature first. skyl
earns its place the moment you have a second model.
