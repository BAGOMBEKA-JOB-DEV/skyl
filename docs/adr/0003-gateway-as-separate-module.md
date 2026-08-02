# ADR-0003: chi belongs to the gateway, not the library

**Status:** Accepted · **Date:** 2026-08-02

## Context

The project brief asked for skyl to use [go-chi](https://github.com/go-chi/chi).
chi is an HTTP **router**: it dispatches inbound requests to handlers.

skyl's core job is the opposite. A library that "helps developers integrate AI
models into their projects" is an outbound HTTP **client** — it builds requests
and parses responses. There is no inbound traffic for a router to route.

Taken literally, "use chi" and "build a client library" are in tension. Rather
than resolve it by dropping either, we identified a component where chi is
genuinely the right tool.

## Decision

chi is used — in the **gateway module**, which is a real HTTP service:
`POST /v1/chat`, `POST /v1/chat/stream` (SSE), `GET /v1/models`, plus auth,
request-ID, recovery, and logging middleware. That is exactly chi's job.

The core library has **no router dependency**. Enforced structurally by the
[two-module layout](0001-two-module-layout.md), not by convention.

## Consequences

**Good.** chi is used where it earns its place. Library users get a library:
importing skyl pulls in provider SDKs and nothing else. Gateway users get a
service with proper middleware. Neither audience subsidises the other.

The gateway is independently valuable — non-Go services get model access,
credentials live in one place, and the estate gets a single audited egress
point for model traffic.

**Bad.** Two modules to build, test, and release. Someone expecting chi in the
core module has to read this ADR to find out why it isn't there — which is why
the README, the architecture doc, and the gateway doc all link here.

## Alternatives considered

- **chi in the core module.** Satisfies the brief most literally and is worst
  for users: a router in the dependency graph of every consumer, used by none.
  Rejected.
- **Gateway as the primary deliverable.** Makes skyl a deployable proxy rather
  than an importable library. Contradicts "reusable library for other
  developers", which was the actual goal.
- **Drop chi, use `net/http.ServeMux`.** Go 1.22+ routing is capable and would
  work. Rejected: chi was explicitly requested, its middleware ecosystem is
  genuinely better here, and once the gateway is a separate module the cost of
  the dependency falls on operators who already accept server dependencies.
