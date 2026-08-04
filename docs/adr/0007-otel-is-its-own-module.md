# ADR-0007: OpenTelemetry instrumentation is its own module

**Status:** Accepted
**Date:** 2026-08-04

## Context

skyl's only observability surface is `skyl.Hook`. It is enough to build on, but
nothing in the repository builds on it, so every user who wants model traffic in
their dashboards writes the same mapping — and writes it differently, which
defeats the point of a provider-agnostic library. A skyl user should be able to
compare Claude and GPT latency on one chart without normalising two vendors'
telemetry themselves.

The OpenTelemetry GenAI semantic conventions are the emerging standard for this,
and as of August 2026 no Go multi-provider LLM library implements them. That is
a differentiator available for the taking.

Two things make the placement non-obvious:

1. **The core module has zero external dependencies** (`docs/rules.md` §4.1).
   The OpenTelemetry API alone pulls `go.opentelemetry.io/otel`, `/trace`,
   `/metric`, `go-logr`, and `auto/sdk`. Putting that in the core would impose
   it on everyone who imports skyl, including people who will never emit a span.
2. **OpenTelemetry's Go floor ratchets.** The current release declares
   `go 1.25.0`; a release a year old declares `go 1.20`. Since Go 1.21 the `go`
   directive is a hard requirement, so whatever floor OpenTelemetry sets is
   inherited by whatever module depends on it.

## Decision

Ship the instrumentation as a fourth module, `github.com/BAGOMBEKA-JOB-DEV/skyl/otel`.

It depends on the root module and on the OpenTelemetry API. It is imported only
by users who want it, and by the gateway — which is a server and already takes
server dependencies (`docs/rules.md` §4.3).

It implements the GenAI conventions over the hook surface rather than by
wrapping `Provider`, so it inherits retry, backoff and error classification for
free and works with any adapter, including one written outside this repository.

Prompt and completion content is deliberately **not** recorded. The conventions
permit it and the hook is handed the whole `skyl.Request`, so this is a choice
rather than a limitation: a span is a durable record shipped to a third party,
and putting user conversations there by default is not a decision a library
should make on its user's behalf.

## Consequences

**Good.** The core keeps its zero-dependency guarantee, which is the property
`docs/idea.md` §4 calls a tax on every user. Nobody pays for OpenTelemetry in
packages or in Go version unless they ask for it. The conventions being
implemented once means two providers produce comparable telemetry, which is
exactly what the library exists for.

**Bad.** A fourth module to build, test, tag and release, and a fourth entry in
every list the release process touches. The `go.work` directive rises to 1.25,
because a workspace must be at least as new as every member — so *contributors*
now need Go 1.25 even though the library itself still builds on 1.22. CI is
unaffected: it builds each module with `GOWORK=off`.

**Bad.** The GenAI conventions are still in development. Attribute keys are
written as constants in this package rather than taken from a `semconv` module,
which keeps skyl's release cadence independent of theirs but means a convention
change is a manual edit here. The tests assert the exact keys, so a change is at
least loud.

**Bad.** OpenTelemetry's Go floor will keep ratcheting, and this module ratchets
with it. That is contained: it cannot affect the root module, because the
dependency points the other way.

## Alternatives considered

**Put it in the core module behind a build tag.** Build tags do not remove a
dependency from `go.mod`, so the core's dependency graph would grow for
everyone. Rejected.

**Wrap `Provider` instead of using the hook.** A decorating Provider would see
requests and responses directly, but it would sit *below* `Client` and so miss
retries, backoff, and the classification `Client` performs — the interesting
parts. It would also have to be applied per-provider rather than per-client.
Rejected.

**Take a dependency on `otelhttp` for trace propagation.** It would create its
own HTTP span per request, duplicating the GenAI span this package already
emits, and add a contrib dependency for what is a header injection. A twelve-line
`RoundTripper` does the job. Rejected.

**Emit Prometheus metrics directly.** That is a second instrumentation path to
maintain, and OpenTelemetry has a Prometheus exporter for anyone who wants that
output. Rejected — see the gateway's `/metrics`, which goes through this module.

## See also

- [ADR-0001](0001-two-module-layout.md) — the original split. Its title says
  "two-module" and there are now four; the reasoning it records is what has been
  applied each time.
- [ADR-0006](0006-anthropic-adapter-is-its-own-module.md) — the same argument
  for the Anthropic SDK's dependency graph.
