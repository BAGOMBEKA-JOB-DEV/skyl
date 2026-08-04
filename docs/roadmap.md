# Roadmap

Living document. [project-plan.md](project-plan.md) tracks the milestones that built what
exists; this tracks what stands between that and a library a company can adopt.

It is deliberately ordered by *what blocks adoption*, not by what is interesting to build.

---

## Where this came from

An audit in August 2026 went looking for the gap between "CI is green" and "a company can
depend on this". CI was green — three modules, `-race`, coverage above every floor, a
clean linter, a fuzzer that survives millions of executions. The gap was elsewhere, and it
was larger than the test suite could see.

Three things dominate everything else:

1. **skyl cannot be installed.** There are no tags, and the `gateway` and
   `provider/anthropic` modules require the root module at `v0.0.0` behind `replace`
   directives. Go ignores `replace` in non-main modules, so the install command in the
   README does not work for anyone outside this repository.
2. **The `go 1.26` floor is not justified by the code.** Nothing outside `_test.go` files
   uses anything newer than Go 1.22. The floor already forced a workaround in CI, because
   no released golangci-lint can lint a `go 1.26` module.
3. **Some of the rules in [rules.md](rules.md) are not met.** §6.5 requires every adapter
   to honour `ProviderOptions` — the Anthropic adapter never reads it. §8.3 requires
   compiling `Example` functions — there are none. §6.1 forbids silently dropping data —
   `internal/oai` drops assistant content when a host returns a content array.

A rule that is documented and unenforced is worse than no rule, so §6.5 and §6.1 are
treated here as defects rather than as roadmap items.

---

## Phase 0 — Defects

Each of these is a bug with a specific failure mode, not a missing feature. Each gets a
regression test that fails before the fix.

| Defect | Failure mode |
|---|---|
| Anthropic ignores `ProviderOptions` | The only escape hatch to `cache_control`, `top_k`, and beta features is unreachable on Anthropic. Violates §6.5 |
| `Usage.TotalTokens()` double-counts cache reads | OpenAI and Gemini report cached tokens as a *subset* of prompt tokens; Anthropic reports them disjointly. Cost dashboards over-report by up to ~2× on a well-cached agent loop |
| Content arrays silently dropped | A host returning `content: [{type:"text"}]` — vLLM, some Azure and OpenRouter upstreams — yields a successful response with empty text. Violates §6.1 |
| `Retry-After` clamped to `maxDelay` | A standard `Retry-After: 60` is truncated to 30s, so the retry lands inside the still-open rate-limit window and is rejected again |
| Transport errors lose their cause | `errors.Is(err, context.DeadlineExceeded)` is false after a timeout; a permanent TLS misconfiguration is retried as if transient |
| Anthropic tool schemas are rebuilt lossily | Only `properties` and `required` survive; `$defs`, `$ref`, and `oneOf` are dropped, so the same `skyl.Tool` behaves differently across providers — which undercuts the one feature the library is named for |
| Gemini ignores `Request.Thinking` | Gemini has `thinkingConfig`; `&Thinking{Enabled: false}` cannot disable reasoning, so a real cost control silently does nothing |
| Truncated streams look complete | A connection dropped mid-generation returns a partial answer with a nil error |
| Refusals are reported as success | `content_filter` maps to `StopRefusal` but returns no error, so `ErrRefusal` is never produced and the gateway's 422 branch is unreachable |

## Phase 1 — Prove correctness without credentials

The README says it plainly: every fake in the test suite was written from the same
provider documentation as the adapter it tests, so a wrong field name leaves both green.
No adapter has ever called a real provider.

That gap cannot be fully closed without credentials, but most of it can be narrowed
without them.

- **Make the sandbox adversarial rather than agreeable.** It currently simulates no tool
  calls at all, no images, no thinking deltas, no cache-token fields, no truncated
  streams, and no mid-stream errors. The two places it *is* adversarial — rejecting a
  missing `max_tokens`, stripping usage from non-final Gemini frames — are precisely the
  ones that caught real bugs. Extend that principle rather than the lookup table.
- **Add a record/replay cassette harness**, so that one contributor with a key runs one
  command and everyone else replays those fixtures forever. This is how the credential
  constraint gets lifted permanently, by someone who has one.
- **Close the mapping gaps the unit tests miss**: the entire tool and tool-choice blocks
  in `internal/oai` and `provider/gemini` are uncovered, as are both image paths and every
  adapter's mid-stream error handling.
- **Meet §8.3 and add benchmarks.** No competing Go library publishes allocation figures;
  measuring a hot path is both a rule and an argument.

## Phase 2 — Make it installable

The cheapest adoption work in this document.

- Lower the Go directive to what the code actually needs.
- Cut `v0.1.0`, with a release process that drops the `replace` directives and pins real
  versions at tag time. [ADR-0006](adr/0006-anthropic-adapter-is-its-own-module.md)
  anticipates this; nothing currently enforces it, so a tag cut today would publish
  permanently broken modules.
- Add a CI check that refuses to tag a commit containing a `replace` directive.

## Phase 3 — What an enterprise review asks for

**Observability.** `Hook` is the only surface, and it cannot report streaming token usage
at all — the stream event fires at handshake, before a single token exists. Streaming is
the dominant mode for chat products, so the primary cost signal is missing from the
primary observability surface. There is also no response ID, no status code on success,
and no per-call correlation ID, so concurrent retry sequences cannot be reassembled from
logs.

The fix is to extend `HookEvent` and emit a terminal event when a stream drains, then ship
`skyl/otel` as a separate module implementing the OpenTelemetry GenAI semantic
conventions. Those conventions are still in development, but they are the emerging
standard and no Go multi-provider library implements them yet.

**The gateway.** It is a working demonstration, not yet a production service. Its wire
format cannot express an assistant turn containing tool calls, so the tool-calling loop it
advertises dead-ends after one round. Streams block graceful shutdown for the full grace
period and then die anyway. There is no readiness probe distinct from liveness, no
metrics, no rate limiting, one shared auth token and therefore no tenancy model, and no
SSE keep-alive — which means a default nginx will buffer the stream and destroy the
property the endpoint exists for.

**Supply chain.** Actions are pinned by tag rather than digest, and there is no
`govulncheck`, SAST, dependency bot, SBOM, or signed release. The Anthropic module
transitively depends on a release candidate that nothing scans.

**Governance.** The Apache-2.0 licence appendix has no copyright holder filled in. There
is no CODEOWNERS, code of conduct, issue template, or DCO, and the bus factor is one.

## Phase 4 — Documentation that survives an evaluation

The developer documentation is strong. The operator, security, and legal documentation
does not exist.

- A provider feature-support matrix — which adapter supports what, as a grid. The honesty
  positioning makes publishing the gaps a differentiator rather than an admission.
- A data-handling statement. For a library whose entire function is transmitting customer
  text to third parties, there is no statement of what leaves the process, what the
  vendors retain, or that `Response.Raw` holds full payloads in memory.
- A gateway runbook and container image. The documentation currently stops at `go run`.
- A threat model, published benchmarks, migration guides, and a security response window.

## Deferred, deliberately

Embeddings, structured output, prompt-caching control, batch APIs, token counting, and
failover each need an ADR before code. Embeddings in particular do not belong on
`Provider` — a different shape deserves a different interface.

Prompt caching is the sharpest of these: skyl *reports* `CacheWriteTokens` while offering
no way to *request* caching, and until the Anthropic `ProviderOptions` defect is fixed
there is no workaround either.

## What this is not

The non-goals in [idea.md](idea.md#non-goals) still hold. Nothing here adds an agent
framework, a prompt templating engine, or a vector store. The gaps above are all in the
transport layer skyl already claims to be.
