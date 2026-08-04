# Threat model

What skyl assumes, what it defends, and what it leaves to you.

The point of writing this down is that the last category is the dangerous one.
A library that lists only its defences invites you to assume the rest is covered.

This complements [SECURITY.md](../SECURITY.md), which covers credential handling
and how to report a vulnerability, and [data-handling.md](data-handling.md),
which covers where prompts go.

---

## Trust boundaries

```
   your code
       │  B1
       ▼
   skyl library ───── B2 ────▶ provider API
       ▲
       │  B3
   gateway client ─── B4 ────▶ provider API
```

### B1 — your code → the library

**The caller is trusted.** skyl does not sanitise prompt content and does not
validate `ProviderOptions` — that is the point of an escape hatch.

`Request.Validate()` checks structure only: a model is set, there is at least one
message, `MaxTokens` is not negative, roles are known, parts are well-formed,
tools have names. It does **not** bound message count, total size, image size or
schema depth. A library caller can build an arbitrarily large request; the only
limit is the provider's.

If your application accepts untrusted input that ends up in a `Request`, bounding
it is your job. skyl will faithfully send whatever you assemble.

### B2 — the library → the provider

**Assumed:** the endpoint is the one you configured, and TLS protects the
connection.

**Defended:**
- TLS verification cannot be disabled. There is no option, by rule
  ([rules.md §7.3](rules.md)). Supply your own `*http.Client` if you need a
  custom trust store.
- A rejected certificate is **never retried** — it is a misconfiguration, not a
  blip, and retrying would delay the error you need to see.
- Credentials never reach an error, a log or a `String()`. A contract test
  asserts this for every adapter against several failure modes.

**Not defended, and worth knowing:**
- ⚠️ **The endpoint is whatever you point it at.** `WithBaseURL` on any adapter,
  and `ANTHROPIC_BASE_URL` in the environment for the Anthropic SDK, redirect
  prompts *and the credential*. An attacker with environment access to your
  process has an exfiltration channel that looks like normal operation.
- ⚠️ **`HTTP_PROXY`/`HTTPS_PROXY` are honoured.** Same shape of risk.
- ⚠️ **There is no cap on a successful response body.** The error path is capped
  at 64 KB, but a success is read with `io.ReadAll` and no limit. A hostile or
  compromised endpoint can exhaust memory. This matters only if your endpoint is
  untrusted — which is exactly the case when the two bullets above are in play.

### B3 — a gateway client → the gateway

**Assumed:** anyone holding a valid bearer token is authorised to spend your
provider budget. Read that sentence again; the rest of this section unpacks it.

**Defended:**
- Authentication is **mandatory**. The gateway refuses to start without a token
  — there is no flag to disable it, because an open relay to paid APIs must not
  be one misconfiguration away.
- Tokens are compared in constant time, with **no early exit**, so timing does
  not reveal which token matched.
- chi's `RealIP` middleware is deliberately **not** used: it rewrites the client
  address from spoofable headers.
- Request bodies are capped at 16 MB, and unknown JSON fields are rejected.

**Unauthenticated by design:** `/healthz`, `/readyz`, `/metrics`, and CORS
preflight. `/metrics` exposes model names, token volumes and error rates for
your whole estate. Keep it on a private network.

### B4 — the gateway → the provider

The gateway holds your keys; callers never see them. Raw provider bodies are not
forwarded by default, and upstream errors are reclassified rather than proxied.

---

## What an authenticated gateway caller can do

This is the section to read before exposing the gateway to anyone you do not
fully trust.

> ### There is no tenant isolation
>
> `SKYL_AUTH_TOKENS` lets you issue several tokens with labels. **The label is
> used for logging and nothing else.** It is never consulted when resolving a
> provider. Every valid token reaches the same provider clients, the same API
> keys and the same quota.
>
> Multiple tokens exist so you can **rotate** them without downtime, and so logs
> can attribute traffic. They are not a permission system. Do not hand a token
> to a party you would not hand your API key's spending power to.

Concretely, a caller with any valid token can:

1. **Spend your budget without limit.** There is no per-token rate limit, no
   spend cap and no request accounting. `SKYL_MAX_CONCURRENT` caps *concurrency*,
   not rate — a caller can run one request at a time, forever, on the most
   expensive model you have configured.
2. **Choose any model on any configured provider.** Model IDs are pass-through
   by design ([ADR-0004](adr/0004-model-ids-are-pass-through.md)), so a token
   you intended for a cheap model can invoke the flagship.
3. **Override anything you configured, via `provider_options`.** It is forwarded
   verbatim and applied *last*, so it wins over values the gateway set.
4. **Enumerate your entitlements.** `GET /v1/models` calls the provider live
   with your key; the result is an account fingerprint. `GET /v1/providers`
   lists what you configured.
5. **Get a provider to fetch a URL of their choosing.** An image URL in an
   Anthropic request is fetched *by Anthropic* — SSRF at one remove, executed
   from their network, not yours. Gemini rejects image URLs outright.
6. **Put prompt fragments in your logs**, by inducing an upstream 4xx whose
   message quotes the input.
7. **Hold connections open.** `/v1/chat/stream` has **no request timeout** — its
   only bound is the 10-minute per-attempt timeout inside the client.

### If you need real multi-tenancy

Run one gateway per tenant, each with its own provider keys and its own budget
at the provider. That is the only boundary that currently exists. Putting
per-tenant limits in front of a shared gateway does not restrict what a token
can reach, only how often it can reach it.

---

## Denial of service

**Bounded:**

| | Limit |
|---|---|
| Gateway request body | 16 MB |
| Provider error body read | 64 KB |
| Error body retained | 2 KB |
| Single SSE event | 8 MB |
| Gateway read-header timeout | 15 s |
| Gateway idle timeout | 120 s |
| Per-request upstream timeout | 120 s default — **not on `/v1/chat/stream`** |
| Per-attempt timeout (library) | 10 min |
| Retries | 3, exponential with full jitter |

**Unbounded, deliberately or otherwise:**

- ⚠️ **Successful response bodies** — no cap (see B2).
- ⚠️ **SSE event count** — each event is capped; the number is not. An endless
  stream ends when your context does.
- ⚠️ **Request rate** — there is no rate limiting anywhere in the gateway.
  Put a reverse proxy in front if you need it.
- ⚠️ **`/v1/chat/stream` duration** — no server-side deadline.
- ⚠️ **Concurrency** — `SKYL_MAX_CONCURRENT` defaults to unlimited.
- ⚠️ **Hooks run synchronously** on the calling goroutine. A slow hook is a
  self-inflicted DoS on every request.

There is no `WriteTimeout` on the gateway's HTTP server. That is deliberate —
a write deadline would sever streams mid-generation — and it means a slow
request *body* is bounded only by the 16 MB cap.

---

## Model output is untrusted input

[SECURITY.md](../SECURITY.md) covers this at length: model text may be shaped by
anything in the context window, including content you did not write, and should
be treated like a form field. Never `exec` it, never interpolate it into SQL or
shell commands, escape it before rendering, and gate side-effecting tool calls.

Two extensions specific to this document:

**Tool arguments are never validated against your schema.** `ToolCall.Arguments`
is raw `json.RawMessage` exactly as the model produced it. skyl does not check it
against the `Tool.Parameters` schema you declared — it cannot, because the schema
is yours and the semantics are yours. A model under indirect prompt injection can
emit arguments your tool never expected. **Validate arguments in your tool
handler, against your own schema, every time.** This is the highest-value
mitigation in this document.

**`Response.Raw` is provider-controlled.** It is an unparsed blob and it is
tempting to unmarshal into a convenient struct. Treat it as hostile input: it is
shaped by the model and the provider, not by you.

---

## Out of scope

- **The provider's own security.** Their infrastructure, retention and
  training-data policies are theirs. See [data-handling.md](data-handling.md).
- **Prompt injection prevention.** skyl does not sanitise model output. A
  partly-effective sanitiser encourages exactly the complacency that gets
  exploited.
- **Your application's authorisation.** skyl has no concept of a user.
- **Supply chain of the Go toolchain and dependencies.** Mitigated by
  `govulncheck`, CodeQL, Dependabot, digest-pinned actions and SBOM generation
  in CI — but those reduce risk, they do not eliminate it.

## Reporting

Found something wrong here, or something this document misses? See
[SECURITY.md](../SECURITY.md). Do not open a public issue for a vulnerability.
