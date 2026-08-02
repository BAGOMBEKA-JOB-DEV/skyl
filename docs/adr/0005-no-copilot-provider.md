# ADR-0005: No GitHub Copilot provider

**Status:** Accepted · **Date:** 2026-08-02

## Context

The brief named Copilot alongside Claude and ChatGPT as a provider skyl should
support. Research into what Copilot actually exposes found:

1. **GitHub's REST Copilot endpoints are administrative only** — seat
   assignment, subscription management, organisation metrics, content-exclusion
   policy. There is no completions or chat endpoint.
2. **The [Copilot SDK](https://github.com/github/copilot-sdk)** (public preview,
   April 2026) is an **agent runtime** — session-based, tool-executing,
   long-running. Structurally different from a completions call, and it requires
   a Copilot subscription.
3. **Its BYOK mode forwards to other providers' keys.** A skyl provider built on
   it would call OpenAI or Anthropic with the user's own key — looping straight
   back through skyl.

Copilot is a *product built on* models, not a model API. There is nothing at the
completions layer to adapt.

## Decision

skyl ships **no** `provider/copilot`. The reason is documented in the README, in
[providers.md](../providers.md), and here.

## Consequences

**Good.** No misleading package. A user reading the provider list learns
something true about Copilot's API surface instead of being misinformed by a
package name. We are not on the hook for maintaining a wrapper around a
preview-stage agent runtime.

**Bad.** The brief asked for Copilot and skyl does not have it. A user scanning
the provider list may assume we forgot — which is why it is documented in three
places rather than silently omitted.

## Why not ship it anyway

A `provider/copilot` package could only be implemented as an OpenAI or Anthropic
call relabelled as Copilot. That is not a shortcut; it is a false statement
encoded in an import path.

Users make real decisions on that label — licensing, vendor commitments,
compliance review, "we use Copilot" in an architecture document. Every one of
those would be wrong. A library that lies about where requests go is worse than
one with a gap, because the gap is visible and the lie is not.

## What would change this

If GitHub ships a general-purpose Copilot completions API, this ADR is
superseded and `provider/copilot` becomes a normal native adapter.

Separately, the Copilot **agent** runtime is a genuinely interesting capability
that skyl could support — through an `Agent` interface rather than `Completer`,
since sessions, tool execution, and streamed agent events do not fit a
completions shape. Tracked in the
[project plan](../project-plan.md#under-consideration) as a possible v2. It is
additive and would not disturb anything decided here.
