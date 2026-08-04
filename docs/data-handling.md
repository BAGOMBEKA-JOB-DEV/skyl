# Data handling

skyl's entire function is sending your users' text to a third party. This
document says exactly what leaves your process, what is kept, and what is
written down — derived from the code rather than from intent.

It is written for the person who has to answer "where does the prompt go?" in a
privacy review. Short version:

- Everything you put in a `Request` is transmitted to the provider you chose.
- The library keeps nothing after a call returns and logs nothing, ever.
- The gateway logs metadata only — with **one** exception, named below.
- Nothing is written to disk unless you explicitly turn on cassette recording.

The thing skyl cannot tell you is what the **provider** does with the prompt
once it arrives. That is their retention policy, their jurisdiction, their
sub-processors, and their training-data terms. Read those. skyl chooses none of
it for you and cannot mitigate it.

---

## What leaves your process

### Everything in the Request

Every field of `skyl.Request` is transmitted: `System`, `Messages` (all text,
images, tool calls and tool results), `Tools` (names, descriptions and full JSON
Schemas), `Model`, sampling parameters, and `ProviderOptions` verbatim.

Tool *descriptions* and *schemas* are worth noticing. They are prompt content —
if your tool descriptions name internal systems, those names go to the provider
on every request that declares the tool.

### The credential

One header per provider: `Authorization: Bearer` (OpenAI-compatible),
`X-Api-Key` (Anthropic), `x-goog-api-key` (Gemini). Gemini's key is deliberately
a header rather than a URL parameter, so it does not land in proxy access logs.

### Two negotiation headers

`Content-Type` and `Accept`. That is the complete list skyl adds — no
User-Agent, no client identifier, no request ID.

### One exception: the Anthropic adapter

`provider/anthropic` is built on the official `anthropic-sdk-go`, which adds
headers skyl never asked for and cannot remove:

- `X-Stainless-OS`, `X-Stainless-Arch`, `X-Stainless-Runtime-Version` — your
  operating system, CPU architecture and exact Go toolchain version;
- `X-Stainless-Lang`, `X-Stainless-Package-Version`, `X-Stainless-Retry-Count`;
- `User-Agent`.

This is host fingerprinting, sent on every request. If that matters to you, it
is a reason to reach Claude through `provider/openaicompat` against a
gateway you control instead — or to accept it, which is what using the vendor's
own SDK normally implies.

### Things in your environment that redirect traffic

- **`HTTP_PROXY` / `HTTPS_PROXY` / `NO_PROXY`** are honoured by Go's default
  transport. A proxy set in the environment sees every prompt.
- **`ANTHROPIC_BASE_URL`** is read by the Anthropic SDK and will send prompts
  *and your API key* to whatever host it names, unless you pass
  `anthropic.WithBaseURL` explicitly.
- **`ANTHROPIC_PROFILE`** and profile files under the Anthropic config directory
  are read by that SDK when no key is passed explicitly.

The other three adapters read nothing from the environment.

---

## What is kept in memory

| What | Contains | How long |
|---|---|---|
| `Response.Raw` | The provider's **untouched response body**. Always populated. | As long as you hold the `*Response`. skyl keeps no reference. |
| `StreamEvent.Raw` | Per-event provider payload | The life of the event |
| `ModelInfo.Raw` | Per-model provider entry | As long as you hold it |
| `Error.Body` | Up to 2 KB of a failed response body — **providers often quote your input here** | The life of the error |
| `HookEvent.Request` | **A pointer to the whole request, prompts included** | The duration of the hook call — but see below |

`Response.Raw` is deliberate: it is the escape hatch that stops skyl's
abstraction from being the reason you cannot ship. It also means a `*Response`
holds a full copy of the provider's reply, so do not stash them in a long-lived
cache without thinking about it.

**`HookEvent.Request` deserves attention.** Every registered hook receives the
whole request, prompts included. A hook that logs the event verbatim ships
conversation content wherever your logs go. That is a decision you make when you
write the hook — skyl passes the request because the OpenTelemetry conventions
need the sampling parameters, and it says so in the field's doc comment.

For streaming, the wrapper holds that pointer until the stream is closed. A
stream you abandon without closing keeps the prompt alive for as long as the
object is reachable. Close your streams.

---

## What is written to disk

**One path, and it is opt-in.**

`internal/cassette` records real provider exchanges so contributors without API
keys can replay them. Recording happens only when `SKYL_RECORD` is set, and the
files land under `testdata/cassettes/`. A recording contains the **request body
verbatim** — so whatever prompt you recorded with is in that file, permanently,
in whatever repository you commit it to.

Credentials are scrubbed on write: `Authorization`, `X-Api-Key`,
`x-goog-api-key`, `OpenAI-Organization`, `OpenAI-Project` are replaced with
`REDACTED`, and account-identifying response headers are dropped. A test walks
every committed cassette looking for credential-shaped strings.

⚠️ **Scrubbing covers headers, not bodies.** A credential embedded in a request
body or in `ProviderOptions` is not removed. Record with throwaway prompts.

No cassettes are currently committed to this repository.

Nothing else in skyl writes to disk. No cache, no spool, no crash dump.

---

## What is logged

### The library: nothing

`skyl`, the adapters and the internal packages contain no logging statements at
all. This is verifiable — grep for `log.` or `fmt.Print` in non-test code and
you will find only doc-comment examples. Observability is opt-in through
`WithHook`, and what a hook does is yours.

### The gateway: metadata, plus one exception

One structured line per request, to stdout:

`method`, `path`, `status`, `bytes`, `duration`, `request_id`, `tenant`

No headers, no bodies, and **not the query string** — only `r.URL.Path`, so a
`?provider=` parameter is not logged.

Two things to know:

1. ⚠️ **`tenant` is a name you choose.** It comes from the label side of
   `SKYL_AUTH_TOKENS` (`label:token`). The token never appears; the label
   always does. Do not name a token after its own value.

2. ⚠️ **Upstream error messages are logged, and providers quote your input.**
   When a provider rejects a request, the gateway logs its error message at WARN
   and returns it to the caller. Providers commonly include the offending
   content — *"invalid content in messages[3]: …"*, or a moderation rejection
   quoting the text. **This is the one channel through which prompt fragments
   can reach your logs.** If that is unacceptable, filter WARN lines with
   `"upstream error"` at your log shipper, or run the gateway with a log
   processor that redacts them.

The provider's raw response body is **not** forwarded to gateway callers unless
you set `SKYL_INCLUDE_RAW=true`, precisely because it can echo request content
back to someone who should not see it.

Panics are recovered by chi's middleware, which prints a stack trace to stderr —
outside skyl's control, and stack traces can contain string contents.

### Metrics

`/metrics` carries counts, durations, and labels for operation, provider, model,
error type and token direction. **No prompt content.** The OpenTelemetry
integration records the *shape* of a request — `max_tokens`, `temperature`,
`top_p` — and never its content, which is a deliberate choice documented in that
package: a span is a durable record shipped to a third party, and putting
conversations there by default is not a library's decision to make.

Note `/metrics` is unauthenticated. It exposes model names, token volumes and
error rates for your whole estate to anyone who can reach the port. Keep it on a
private network.

---

## For your privacy review

**Personal data leaving the EU/your jurisdiction:** yes, if the prompt contains
it and your provider is outside it. skyl transmits to whichever endpoint you
configure; it has no view of where that is.

**Sub-processors:** whichever providers you configure. skyl adds none.

**Retention by skyl:** none. In-memory for the life of a call; nothing on disk
without `SKYL_RECORD`.

**Retention by the provider:** their policy, not ours. Check whether your tier
trains on inputs — several vendors differ between consumer and enterprise
agreements.

**Right to erasure:** skyl holds nothing to erase. Requests against the provider
go through the provider.

**Encryption in transit:** TLS, always. Verification cannot be disabled — there
is no option for it, by rule. If you need a custom trust store, supply your own
`*http.Client`.

**Data minimisation:** the practical lever is `Response.Raw` and `Error.Body`,
which hold provider output you may not need. If you are storing responses, store
`Text()` rather than the whole `*Response`.

## See also

- [SECURITY.md](../SECURITY.md) — credential handling and vulnerability reporting
- [threat-model.md](threat-model.md) — trust boundaries and what an attacker can do
- [feature-matrix.md](feature-matrix.md) — what each adapter transmits
