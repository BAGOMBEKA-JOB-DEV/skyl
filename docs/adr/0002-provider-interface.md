# ADR-0002: A four-method `Provider` interface

**Status:** Accepted · **Date:** 2026-08-02

## Context

The seam between skyl and a vendor has to be drawn somewhere. Draw it too wide
and every adapter reimplements retry, validation, and timeouts — inconsistently.
Draw it too narrow and adapters can't express what their vendor does.

## Decision

```go
type Provider interface {
	Name() string
	Complete(ctx context.Context, req *Request) (*Response, error)
	Stream(ctx context.Context, req *Request) (Stream, error)
	Models(ctx context.Context) ([]ModelInfo, error)
}
```

Cross-cutting behaviour — retry, backoff, timeout, validation, hooks — lives in
`Client`, which wraps a `Provider`. Adapters do translation and nothing else.

## Consequences

**Good.** Retry is written and tested once, so a new adapter inherits
production behaviour for free. The interface is small enough to implement
out-of-tree, so a third-party adapter is a first-class citizen with no changes
to skyl. It is trivial to fake in tests, and `Client`, the gateway, and every
test double compose against the same four methods.

**Bad.** An adapter cannot customise retry semantics. If a vendor ever needs
genuinely different backoff, it must come through `Client` options rather than
the adapter — accepted, because per-adapter retry logic is exactly the
inconsistency we are avoiding.

`Models` is on the interface even though it is not part of a completion, because
live discovery is the honest answer to "what models can I use?" — see
[ADR-0004](0004-model-ids-are-pass-through.md).

## Alternatives considered

- **Adapters own retry.** Rejected: N implementations, N sets of bugs.
- **One `Do(ctx, req)` method with a mode flag.** Collapses streaming and
  non-streaming into a union return type. Worse ergonomics, no real gain.
- **Separate `Streamer` interface with runtime assertion.** Turns a
  compile-time guarantee into a runtime surprise.
