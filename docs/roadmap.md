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

1. ~~**skyl cannot be installed.**~~ *(fixed in Phase 2)* There were no tags, and the
   `gateway` and `provider/anthropic` modules required the root module at `v0.0.0` behind
   `replace` directives. Go ignores `replace` in non-main modules, so the install command
   in the README did not work for anyone outside this repository.
2. ~~**The `go 1.26` floor is not justified by the code.**~~ *(fixed in Phase 2)* Nothing
   outside `_test.go` files used anything newer than Go 1.22. The floor had already forced
   a workaround in CI, because no released golangci-lint can lint a `go 1.26` module.
3. **Some of the rules in [rules.md](rules.md) are not met.** §6.5 required every adapter
   to honour `ProviderOptions` — the Anthropic adapter never read it. §6.1 forbids
   silently dropping data — `internal/oai` dropped assistant content when a host returned
   a content array. Both fixed in Phase 0, the first with a contract-suite check so it
   cannot regress in one adapter while passing in another. §8.3 requires compiling
   `Example` functions — there are still none; that is Phase 1.

A rule that is documented and unenforced is worse than no rule, so §6.5 and §6.1 were
treated as defects rather than as roadmap items.

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

## Phase 1 — Prove correctness without credentials ✅

Done, with the exception noted at the end.

- **The sandbox calls tools**, on all three wire protocols, streaming and not, with
  arguments split mid-token across frames so an adapter that never accumulates cannot
  pass. It honours `tool_choice`, answers the second turn using the tool's own output, and
  refuses to call a tool the caller forbade.
- **Streams can fail mid-flight.** Two fault-injecting model IDs —
  `sandbox-stream-truncate` and `sandbox-stream-error` — fire after the response has begun,
  which `sandbox-status-NNN` structurally could not. Every adapter's mid-stream error and
  truncation handling was dead code before this.
- **The dead mapping blocks are covered.** `buildPayload` in both `internal/oai` and
  `provider/gemini` went from ~60% to 100%; tools, tool choice, images, `TopP`,
  `Temperature`, `Stop`, `reasoning_effort` and request-side `ToolCall` all have
  assertions now.
- **`internal/cassette`** records real provider exchanges and replays them, standard
  library only. Credentials are scrubbed on write, and a test walks committed fixtures
  looking for anything credential-shaped.
- **Examples and benchmarks exist** (rules.md §8.3, and the project-plan item). Ten
  runnable examples with verified output; benchmarks on the per-token paths.

Coverage: root 85.4% → 89.0%, `provider/anthropic` 92.6% → 94.6%. Floors raised to match.

**The honest limit:** cassettes cannot be recorded here, because that needs a provider
key. The replay half is tested; the record half is not. Until somebody runs
`SKYL_RECORD=1` with a real credential, no adapter has still ever spoken to a real
provider, and the caveat in the README stands unchanged.

## Phase 1 — original scope

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

## Phase 2 — Make it installable ✅

Done, except for the tagging itself, which is a human decision.

- **Go floors lowered to what the code actually needs**: 1.22 for the library, 1.24 for
  the two adapter modules. The second number is not a choice — `anthropic-sdk-go` declares
  `go 1.24`, and since Go 1.21 that is a hard requirement rather than advice, so a module
  declaring less cannot build it. That split is the right shape anyway: it is the same
  principle as [ADR-0001](adr/0001-two-module-layout.md), applied to toolchains instead of
  dependencies. Nobody pays for a vendor SDK they did not ask for, in packages or in Go
  versions.
- **CI builds each module against its own floor.** A floor that is only ever built with
  the newest toolchain is a guess.
- **`replace` directives replaced by a [`go.work`](../go.work) workspace**, and CI builds
  with `GOWORK=off`. The `replace` was never the bug — Go ignores it in a non-main module,
  so it silently *hid* the bug, which is the `require` line naming a version that will
  never exist.
- **[RELEASING.md](../RELEASING.md) and `scripts/release.sh`** encode the ordering, which
  is not optional: each module's `go.sum` needs a checksum for the version below it, and a
  checksum can only be computed for a version the proxy already serves.
- **A CI gate refuses to publish a tag carrying a `replace` or a `v0.0.0` require.**
  [ADR-0006](adr/0006-anthropic-adapter-is-its-own-module.md) anticipated the replaces
  being dropped at tag time; nothing enforced it, and proxy tags are immutable.

Verified by tagging `v0.1.0` in a disposable clone and installing all three modules from a
scratch module through a real git resolution — the command in the README, which does not
work today, then does.

**Remaining:** cut the actual tags. See RELEASING.md.

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
