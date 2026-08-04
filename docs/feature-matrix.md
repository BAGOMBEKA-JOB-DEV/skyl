# Provider feature matrix

What each adapter actually does with each part of a `skyl.Request`, including
what it quietly does not do.

Publishing the gaps is the point. [idea.md §3](idea.md) says it is better to say
"we don't support this" than to ship something that looks supported and quietly
does the wrong thing — and a matrix that lists only the green cells is exactly
the thing that section rejects. The column worth reading is
[Silently ignored](#silently-ignored-the-important-column).

Four states are used throughout:

| | Meaning |
|---|---|
| **✅** | Mapped. Sent to the provider; the wire field is named. |
| **⛔** | Rejected. Returns `ErrUnsupported` before any request is made. |
| **⚠️** | **Silently ignored.** Accepted, then dropped. Nothing tells you. |
| — | Not applicable. |

> This matrix was written by reading the adapters, and it is accurate as of the
> commit that added it. If you find a cell that no longer matches the code, that
> is a bug — please report it. Making drift somebody's problem is the only way
> it gets fixed.

`openai` and `openaicompat` share one implementation (`internal/oai`) and are
listed together. See [where they differ](#openai-versus-openaicompat) — it is
smaller than you would expect.

---

## Request fields

| Field | anthropic | openai / openaicompat | gemini |
|---|---|---|---|
| `Model` | ✅ `model` | ✅ `model` | ✅ in the **URL path**, not the body |
| `System` | ✅ top-level `system` | ✅ leading `system` message | ✅ `systemInstruction` |
| `Messages` | ✅ `messages` | ✅ `messages` | ✅ `contents` |
| `MaxTokens` | ✅ `max_tokens` — **defaults to 4096** when unset | ✅ `max_completion_tokens` / `max_tokens`, omitted when 0 | ✅ `generationConfig.maxOutputTokens`, omitted when 0 |
| `Temperature` | ✅ `temperature` | ✅ `temperature` | ✅ `generationConfig.temperature` |
| `TopP` | ✅ `top_p` | ✅ `top_p` | ✅ `generationConfig.topP` |
| `Stop` | ✅ `stop_sequences` | ✅ `stop` | ✅ `generationConfig.stopSequences` |
| `Tools` | ✅ — schema rebuilt, see [below](#tool-schemas) | ✅ `parameters` verbatim | ✅ `functionDeclarations`, verbatim |
| `ToolChoice` | ✅ all four modes | ✅ all four modes | ✅ all four modes |
| `Thinking` | ⚠️ partly — see [below](#thinking) | ⚠️ mostly — see below | ✅ fully |
| `ProviderOptions` | ✅ JSON-path set | ✅ shallow merge | ✅ shallow merge |

Anthropic is the only adapter that substitutes a `MaxTokens` default: its API
requires the field, so skyl supplies 4096 rather than failing a request every
other provider would accept.

### Tool schemas

Your `Tool.Parameters` is a JSON Schema. Whether it arrives intact differs:

- **openai / openaicompat / gemini** — passed through **verbatim**. `$defs`,
  `$ref`, `oneOf`, `additionalProperties`, everything.
- **anthropic** — **reconstructed**. `properties` and `required` are read into
  the SDK's typed struct and every other key is re-attached, so `$defs`, `$ref`,
  `oneOf` and the rest *do* survive. Two things do not:
  - ⚠️ the top-level **`"type"` is dropped and forced to `"object"`**. A schema
    whose root type is anything else is silently coerced.
  - ⚠️ non-string entries in `required` are dropped.

### Thinking

The least uniform field in the library. Read the row for the provider you use.

| `Thinking` value | anthropic | openai / openaicompat | gemini |
|---|---|---|---|
| `nil` | nothing sent | nothing sent | nothing sent |
| `{Enabled: true}`, no effort | ✅ `thinking: {type: adaptive}` | ⚠️ **ignored** | ✅ budget `-1` (model decides) |
| `{Enabled: false}` | ✅ `thinking: {type: disabled}` | ⚠️ **ignored** | ✅ budget `0` |
| `{Enabled: true, Effort: low}` | ⚠️ effort **ignored**, adaptive sent | ✅ `reasoning_effort: low` | ✅ budget 1024 |
| `… medium` | ⚠️ effort ignored | ✅ `medium` | ✅ 8192 |
| `… high` | ⚠️ effort ignored | ✅ `high` | ✅ 16384 |
| `… max` | ⚠️ effort ignored | ✅ sent as `max` — **OpenAI does not define this value**, so expect a 400 | ✅ 24576 |

Two consequences worth stating plainly:

- **On OpenAI, `&skyl.Thinking{Enabled: false}` does nothing.** If you are
  trying to turn reasoning off to control cost, it will not work there.
- **On Anthropic, `Effort` does nothing.** The SDK's adaptive thinking config
  has no budget or effort field, so there is nothing to map it onto.

---

## Message parts

`skyl.Part` has exactly four implementations. Support varies by part *and* by
the role of the message carrying it.

| Part (role) | anthropic | openai / openaicompat | gemini |
|---|---|---|---|
| `Text` (user, assistant) | ✅ | ✅ | ✅ |
| `Text` (tool role) | ✅ becomes user content | ⛔ *"tool messages may only contain tool results"* | ✅ becomes user content |
| `Image` URL (user) | ✅ `source.type: url` | ✅ `image_url.url` | ⛔ *"Gemini requires inline image data, not a URL"* |
| `Image` data (user) | ✅ base64 source | ✅ synthesised `data:` URI | ✅ `inlineData` |
| `Image` (assistant) | ✅ accepted — the API may not | ⛔ *"images are only supported on user messages"* | ✅ accepted |
| `ToolCall` (assistant) | ✅ `tool_use` — **arguments validated as JSON**, rejected if malformed | ✅ `tool_calls`, arguments opaque | ✅ `functionCall`, arguments opaque |
| `ToolCall` (user) | ✅ accepted | ⛔ *"tool calls are only supported on assistant messages"* | ✅ accepted |
| `ToolResult` (tool role) | ✅ `tool_result` in a user turn | ✅ `tool` role + `tool_call_id` | ✅ `functionResponse` in a user turn |
| `ToolResult.IsError` | ✅ real `is_error` boolean | ⚠️ **lossy** — prefixes `"error: "`, and **drops the flag entirely when content is empty** | ⚠️ **dropped** — never reaches the wire |
| Both `URL` and `Data` set | ⚠️ URL dropped | ⚠️ **Data** dropped | ⚠️ URL dropped |

`ToolResult.IsError` is the one to watch. Its whole purpose is telling the model
a tool failed so it can adapt instead of building on a result that isn't there.
**On Gemini that signal never arrives.**

---

## Responses

| | anthropic | openai / openaicompat | gemini |
|---|---|---|---|
| Text | ✅ | ✅ handles string **and** block-array shapes | ✅ |
| Tool calls | ✅ | ✅ | ✅ — `ToolCall.ID` is set to the **function name**, because Gemini issues no call IDs |
| `Response.ID` | ✅ | ✅ | ✅ `responseId` when present |
| `Response.Model` | ✅ from the response | ✅ from the response | ✅ `modelVersion` |
| `Response.Raw` | ✅ always | ✅ always | ✅ always |
| Reasoning / thinking text | ⚠️ **dropped** | ⚠️ **dropped** | ⚠️ **dropped**, and worse — see below |
| Refusals | returned as `StopRefusal` | ✅ empty refusal becomes `ErrRefusal` | ✅ blocked prompt becomes `ErrRefusal` |

⚠️ **Reasoning content is discarded by all four adapters.** Anthropic thinking
blocks, OpenAI-family `reasoning_content`, and Gemini `thought` parts are all
dropped from `Response.Message`. They remain in `Response.Raw`. On Gemini
specifically, if you enable thought output through `ProviderOptions`, the thought
text arrives as an ordinary `Text` part and is **indistinguishable from the
answer** — do not do that without reading `Raw` yourself.

### Stop reasons

| Provider value | skyl |
|---|---|
| `end_turn`, `stop`, `STOP` | `StopEndTurn` |
| `max_tokens`, `length`, `MAX_TOKENS`, `model_context_window_exceeded` | `StopMaxTokens` |
| `tool_use`, `tool_calls`, `function_call` | `StopToolUse` |
| `stop_sequence` (**anthropic only**) | `StopStopSequence` |
| `refusal`, `content_filter`, `SAFETY`, `RECITATION`, `BLOCKLIST`, `PROHIBITED_CONTENT`, `SPII` | `StopRefusal` |
| anything else, including `pause_turn`, `OTHER`, `MALFORMED_FUNCTION_CALL` | `StopUnknown` |

**`StopStopSequence` only ever comes from Anthropic.** OpenAI reports a
stop-sequence hit as plain `stop`, so it arrives as `StopEndTurn`; Gemini reports
`STOP`. If you branch on a stop sequence having fired, read `Raw`.

On Gemini, a response containing any function call reports `StopToolUse`
regardless of its `finishReason`.

### Usage

`InputTokens` is the **total** input including cache; the cache figures break it
down rather than adding to it. Providers disagree on the wire, so the adapters
normalise:

| | anthropic | openai / openaicompat | gemini |
|---|---|---|---|
| Wire semantics | cache counts are **disjoint** from input | cache counts are **inside** the prompt count | cache counts are **inside** the prompt count |
| `InputTokens` | ✅ cache **added in** by the adapter | ✅ copied | ✅ copied |
| `CacheReadTokens` | ✅ | ✅ | ✅ |
| `CacheWriteTokens` | ✅ | ⚠️ always 0 — no wire field | ⚠️ always 0 — no wire field |
| Reasoning-token counts | ⚠️ not surfaced (they are inside `OutputTokens`) | ⚠️ not surfaced (inside `OutputTokens`) | ⚠️ not surfaced, **and not counted** — Gemini's `thoughtsTokenCount` is excluded from `candidatesTokenCount`, so `OutputTokens` under-reports what you are billed |

---

## Streaming

| | anthropic | openai / openaicompat | gemini |
|---|---|---|---|
| Text deltas | ✅ | ✅ | ✅ |
| Thinking deltas (`EventThinkingDelta`) | ✅ **only adapter that emits these** | ⚠️ never | ⚠️ never |
| Tool calls | ✅ buffered, emitted whole | ✅ accumulated across frames by index | ✅ arrive whole |
| Terminal `EventDone` | ✅ | ✅ | ✅ |
| Usage on the terminal event | ✅ | ✅ — needs the host to honour `stream_options.include_usage`; **zero if it does not** | ✅ |
| Truncation detected | ✅ | ✅ | ✅ |
| Mid-stream errors surfaced | ✅ | ✅ | ✅ |
| `StreamEvent.Raw` | ⚠️ never populated | text deltas only | text and tool-call events |

A stream that ends without its terminal signal is reported as an error on all
three — *"stream ended without a terminal event; the response is truncated"* —
rather than looking like a complete short answer.

---

## Model listing

All four support `Models(ctx)`; none return `ErrUnsupported`.

| | anthropic | openai | openaicompat | gemini |
|---|---|---|---|---|
| `ID`, `Provider`, `Raw` | ✅ | ✅ | ✅ | ✅ |
| `DisplayName` | ✅ | ⚠️ empty — OpenAI's response has no such field | host-dependent (OpenRouter supplies it) | ✅ |
| `ContextWindow` | ✅ | ⚠️ empty | host-dependent | ✅ |
| `MaxOutputTokens` | ✅ | ⚠️ never set | ⚠️ never set | ✅ |

⚠️ Gemini ignores `nextPageToken`; beyond 1000 models the list is silently
truncated.

---

## ProviderOptions — read this before using it

`ProviderOptions` reaches the wire on all four adapters, but by two different
mechanisms, and the difference will bite you.

**openai, openaicompat, gemini — a shallow top-level merge.** Your keys are
copied over the built payload, so you win. It is **not** a deep merge:

```go
// DON'T: this destroys maxOutputTokens, temperature, topP, stopSequences
// and thinkingConfig, because it replaces the whole object.
ProviderOptions: map[string]any{
    "generationConfig": map[string]any{"seed": 7},
}
```

The same trap applies to `messages`, `tools` and `stream_options` on OpenAI. If
you need one nested field, you must restate the whole object including
everything skyl would have set.

**anthropic — JSON paths.** Options are applied to the encoded body by path, so
a nested field can be set without disturbing its siblings:

```go
ProviderOptions: map[string]any{"thinking.budget_tokens": 4096}
```

It is the only adapter where that works — and the only one where a top-level key
containing a `.` behaves surprisingly.

`ProviderOptions` never sets HTTP headers on any adapter. Headers are fixed at
construction, via each adapter's `WithHeader`-style option.

---

## openai versus openaicompat

They share one implementation. The complete list of differences:

| | openai | openaicompat |
|---|---|---|
| Output-cap field | `max_completion_tokens` | `max_tokens` |
| Provider name in responses, errors, hooks | `openai` | `openai-compatible`, or whatever `WithName` says |
| Base URL | defaults to OpenAI | **required**; `New` panics without it |
| API key | positional argument | optional — omit it for Ollama, LM Studio, llama.cpp |
| Extra headers | `WithOrganization`, `WithProject` | generic `WithHeader` |

Everything else — request mapping, response parsing, streaming, error
classification — is byte-identical.

---

## Silently ignored: the important column

Every ⚠️ above, in one place. These are the cases where skyl accepts something
and does not do it, which is the failure mode that costs you an afternoon.

**Per adapter**

1. `Thinking.Effort` — anthropic.
2. `Thinking` entirely, unless `Enabled` **and** `Effort` are both set — openai,
   openaicompat. So an explicit "off" does nothing.
3. `ToolResult.IsError` — gemini always; openai/openaicompat when content is
   empty.
4. A tool schema's top-level `type`, coerced to `object` — anthropic.
5. Non-string entries in a tool's `required` array — anthropic.
6. `Image.URL` when `Data` is also set — anthropic, gemini. `Image.Data` when
   `URL` is also set — openai, openaicompat.
7. `nextPageToken` on model listing beyond 1000 entries — gemini.
8. Streaming tool calls whose `name` never arrived — openai, openaicompat.
9. Non-`text` blocks in a response content array — openai, openaicompat.

**All adapters**

10. Reasoning/thinking content in non-streaming responses.
11. Reasoning-token counts.
12. `StreamEvent.Raw` on the terminal `EventDone`.
13. `Usage.CacheWriteTokens` — anthropic is the only source.
14. Sibling keys of any object a `ProviderOptions` top-level key replaces —
    openai, openaicompat, gemini.

**Deliberately not silent:** unparseable SSE frames are skipped rather than
treated as fatal, because providers interleave keep-alives and vendor-specific
records. That one is a design decision, not an oversight.

---

## When a gap matters

Everything in the ⚠️ list is reachable another way:

- **Response-side gaps** — `Response.Raw` carries the provider's untouched body.
  Nothing is lost, only unmodelled.
- **Request-side gaps** — `ProviderOptions` sends anything skyl does not model,
  subject to the shallow-merge caveat above.

That is the deal [idea.md §2](idea.md) makes: skyl unifies the common 90% and
gets out of the way for the rest. A gap in this table should cost you a few
lines, never a fork.
