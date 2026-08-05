# Validating against real provider APIs

This is the one check the rest of the test suite structurally cannot perform,
and until it has been run the README's headline caveat stands.

Every fake in this repository — the sandbox, the unit-test fixtures, the
contract suite — was written from the same provider documentation as the adapter
it exercises. If a field name is wrong, the fake is wrong in exactly the same
way, the two agree with each other, and CI stays green. Only a real call
settles it.

You need your own credentials, so only you can run this.

---

## What it costs

Roughly **$0.05–0.50 per provider** for a full pass, and the largest cost is the
model you choose rather than the number of calls.

The suite runs **8 checks per provider**, all with small prompts and low
`MaxTokens`. One of them is two-turn, so budget **9 upstream requests** per
provider. Gemini's free tier covers the whole run.

Pick a cheap model. The suite asserts on transport behaviour, not on answer
quality — a small model proves the mapping exactly as well as a large one. The
defaults are already small (`gpt-5.4-nano`, `gemini-3.6-flash`,
`claude-haiku-4-5`); override with `SKYL_TEST_OPENAI_MODEL`,
`SKYL_TEST_GEMINI_MODEL`, `SKYL_TEST_ANTHROPIC_MODEL`.

## Running it

```bash
export GOTOOLCHAIN=go1.26.0

# Whichever you have. Absent keys skip cleanly rather than failing.
export OPENAI_API_KEY=...
export GEMINI_API_KEY=...
export ANTHROPIC_API_KEY=...

# openai, gemini and openaicompat live in the root module.
go test -tags=integration -v -run TestLive ./provider/

# provider/anthropic is its own module.
cd provider/anthropic && go test -tags=integration -v -run TestLive ./...
```

`openaicompat` defaults to a local Ollama at `http://localhost:11434/v1` and is
skipped unless you set a key. To run it — against Ollama, vLLM, OpenRouter,
Groq, anything OpenAI-shaped:

```bash
SKYL_TEST_COMPAT_KEY=local \
SKYL_TEST_COMPAT_BASE_URL=http://localhost:11434/v1 \
SKYL_TEST_COMPAT_MODEL=llama3.3 \
  go test -tags=integration -run TestLiveOpenAICompat ./provider/
```

Run all of this **before** cutting a release tag. A tag is permanent, and a
wrong field name should never appear in a published version.

## Recording cassettes

This is a **separate** run from the live suite — a different, untagged test that
records rather than asserts:

```bash
SKYL_RECORD=1 OPENAI_API_KEY=... GEMINI_API_KEY=... \
  go test -count=1 -run Cassette ./provider/
```

It captures each exchange into `testdata/cassettes/`, so one paid run produces
fixtures that replay free forever afterwards. Recording currently covers
**openai and gemini** only.

Credentials are scrubbed from headers on write — `Authorization`, `X-Api-Key`,
`x-goog-api-key`, `OpenAI-Organization`, `OpenAI-Project` — and a test walks
every committed cassette looking for credential-shaped strings.

⚠️ **Scrubbing covers headers, not bodies.** A credential pasted into a prompt or
into `ProviderOptions` is recorded verbatim. Record with throwaway prompts, and
read a cassette before committing it.

Replay tests are untagged, so committed cassettes begin asserting in ordinary CI
immediately. That is the point: it converts a one-off paid run into a permanent
regression guard.

## What each check proves

| Check | What only a real call can tell you |
|---|---|
| `models` | Discovery works against the real endpoint. skyl has no hardcoded model list ([ADR-0004](adr/0004-model-ids-are-pass-through.md)), so this *is* the model list. |
| `complete` | The request maps, the response parses, usage is non-zero, and the stop reason is one skyl knows. |
| `stream` | SSE framing survives a real network — chunked, split across packets, at the provider's own cadence. |
| `rejects a bogus model` | A typo classifies as `ErrNotFound`/`ErrBadRequest` rather than something unactionable. Pass-through model IDs are only defensible if this holds. |
| `tool call` | The JSON Schema we send is one the provider *accepts*, the call ID is populated, and arguments parse. A schema a fake accepts and a provider rejects is invisible offline. |
| `tool round trip` | The two-turn path: replaying the provider's own assistant message and answering it. Message ordering, role naming and tool-result encoding all have to be right at once, and the adapters differ most here. |
| `streamed tool call` | Argument fragments reassemble into valid JSON when fragmented the way a *real* provider fragments them, not the way our sandbox does. |
| `stops at max tokens` | Truncation reports `StopMaxTokens`. A truncated answer reported as complete is silent data loss. Also asserts the cache-token inclusion rule on [`skyl.Usage`](../response.go). |

## Reading a failure

Not every red run is an adapter bug. Triage in this order:

**Provider-side and not your problem** — retry once before investigating:
`ErrRateLimit` (429), `ErrServer` (5xx), a timeout on a loaded endpoint.

**A model behaviour, not a mapping bug:**

- `tool call` fails with *"no tool call returned despite ToolChoiceRequired"* —
  some models comply poorly with forced tool use. Try a different model before
  suspecting the adapter.
- `tool round trip` skips — the first turn produced no call, so there was
  nothing to answer. Same cause.
- `stops at max tokens` reports `end_turn` — check the model actually had more
  to say. `MaxTokens: 8` on a terse model can legitimately complete.

**A real adapter bug — these are what the run is for:**

- **400 on the tool-call check.** The schema we send is malformed for that
  provider. This is the single most likely genuine finding.
- **`ToolCall.Arguments is not valid JSON`** on the streaming check. Fragment
  accumulation is wrong against real fragmentation.
- **`StopReason` is `unknown`.** The provider reported something not in skyl's
  map — the mapping table needs the new value.
- **`Usage` entirely zero.** Token accounting is not being parsed; the field
  names moved.
- **400 on the round trip.** The tool-result encoding or message ordering is
  wrong. Check `Response.Raw` from the first turn against what was replayed.
- **`CacheReadTokens exceeds InputTokens`.** The provider's cache semantics are
  not what the normalisation in [`skyl.Usage`](../response.go) assumes.

For any of these, `Response.Raw` holds the provider's untouched body and is the
fastest way to see what actually arrived.

## After a green run

1. Commit any cassettes recorded, having read them.
2. Update the status section of [README.md](../README.md) — which currently says
   no adapter has spoken to a real provider — naming which adapters and which
   models were validated, and when.
3. Then, and only then, cut the release tags. See [RELEASING.md](../RELEASING.md).

If it did not go green, that is the run doing its job. Fix what it found and go
again — the finding was always there, it was just invisible.
