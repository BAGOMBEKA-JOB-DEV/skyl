# ADR-0001: Two-module repository layout

**Status:** Accepted · **Date:** 2026-08-02

## Context

skyl ships two things with different audiences: a **client library** that Go
programs import, and an optional **HTTP gateway** that operators deploy. The
gateway needs a router (chi) and server middleware. The library needs neither.

In Go, a module's dependencies are inherited by everyone who imports it. One
module for both would mean every library user carries chi in `go.sum`, in their
vulnerability scans, and in their upgrade schedule — for code they never call.

## Decision

Two modules in one repository:

- `github.com/BAGOMBEKA-JOB-DEV/skyl` — the library
- `github.com/BAGOMBEKA-JOB-DEV/skyl/gateway` — the service, own `go.mod`

The gateway depends on the library. The library never depends on the gateway.

## Consequences

**Good.** `go get` on the library pulls provider SDKs and nothing else. The two
version independently. The dependency rule is structural, so it cannot be
violated by accident — an import of chi from the core module simply won't
compile.

**Bad.** CI must build and test both modules. A change spanning both is two
tagged releases. Contributors must notice which module they're in.

We accept the friction: it is small, mechanical, and caught by CI, whereas an
unnecessary dependency is imposed permanently on every downstream user.

## Alternatives considered

- **Single module.** Simpler to develop; taxes every library user. Rejected.
- **Separate repository for the gateway.** Same isolation, but the shared
  history, docs, and issue tracker are worth more than the extra separation.
- **Build tags.** Does not work — `go.mod` dependencies are not tag-conditional.
