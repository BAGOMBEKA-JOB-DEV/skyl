# ADR-0006: The Anthropic adapter is its own module

**Status:** Accepted · **Date:** 2026-08-02
**Amends:** [ADR-0001](0001-two-module-layout.md)

## Context

The Anthropic adapter is built on the official
[anthropic-sdk-go](https://github.com/anthropics/anthropic-sdk-go), because
using a vendor's own SDK is the supported path and avoids re-deriving a wire
format by hand.

Adding it to the core module pulled in **eleven transitive dependencies** —
including a JSON-schema generator and a YAML parser. In Go, a module's
requirements land in the graph of everyone who imports it, so a user who only
wanted `provider/openai` would inherit all of them: in `go.sum`, in their
vulnerability scans, and in their upgrade schedule.

That directly contradicts [rules.md §4.1](../rules.md#4-dependencies) and
[idea.md principle 4](../idea.md#4-dependencies-are-a-tax-on-every-user), both
written before we measured the cost.

The other three adapters are built on `net/http` and the standard library.
`provider/openai` and `provider/openaicompat` in particular share one internal
implementation of the OpenAI chat-completions format, since that format is what
the generic adapter must speak anyway — so OpenAI is `openaicompat` with a
fixed base URL and a couple of extras, not a second codebase.

## Decision

`provider/anthropic` is its own Go module:

```
github.com/BAGOMBEKA-JOB-DEV/skyl                     core — standard library only
github.com/BAGOMBEKA-JOB-DEV/skyl/provider/anthropic  official SDK lives here
github.com/BAGOMBEKA-JOB-DEV/skyl/gateway             chi lives here
```

Users install it explicitly:

```bash
go get github.com/BAGOMBEKA-JOB-DEV/skyl/provider/anthropic
```

## Consequences

**Good.** The core module has **zero external dependencies** — a much stronger
claim than "dependency-light", and one users can verify in one command. Anyone
using OpenAI, Gemini, or any OpenAI-compatible host pays nothing for the
Anthropic SDK. Claude users still get the officially supported client, with its
typed parameters and streaming accumulator. The dependency boundary is
structural, so it cannot be eroded by an absent-minded import.

**Bad.** Three modules to build, test, tag, and release; CI runs a matrix over
them. Installing the Anthropic adapter is a second `go get`, which is
unusual enough to need saying in the README and the package doc. A change
spanning core and the adapter is two releases, and the adapter's `go.mod`
carries a `replace` for local development that must be dropped at tag time.

We accept this. The friction is mechanical, falls on maintainers, and is caught
by CI. An unnecessary dependency is permanent and falls on every user.

## Alternatives considered

- **Anthropic SDK in the core module.** Simplest layout; taxes every user with
  eleven dependencies for one adapter. Rejected.
- **Hand-rolled Anthropic client over `net/http`,** matching the other three.
  Would keep one module and zero dependencies, and is perfectly feasible — the
  Messages API is stable and well documented. Rejected because using a vendor's
  official SDK is the supported path, gets fixes and new features without work
  from us, and is what a Claude user reviewing this adapter would expect to
  find.
- **A module per adapter, uniformly.** Consistent, but four modules of
  ceremony to solve a problem only one adapter has. The rule we actually want
  is "isolate an adapter when it brings dependencies", and today that is
  exactly one.
