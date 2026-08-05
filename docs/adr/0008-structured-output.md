# ADR-0008: Structured output sends schemas verbatim and parses nothing

**Status:** Accepted · **Date:** 2026-08-05

## Context

Every provider skyl targets can now constrain a reply to a JSON Schema, and it is
the most-used production feature after chat itself. skyl modelled none of it.

The only route a caller had was `ProviderOptions`, and on Gemini that route is a
trap rather than a workaround. The schema lives at
`generationConfig.responseSchema`, `ProviderOptions` is merged with a **shallow**
top-level replace ([`httpx.Merge`](../../internal/httpx/httpx.go)), and it is
applied *last* — so setting `generationConfig` to carry a schema silently
destroys the `temperature`, `topP` and `thinkingConfig` that skyl put there. The
escape hatch removes the thing it is escaping from.

The three wire shapes have nothing in common beyond the schema itself:

| Adapter | Wire |
|---|---|
| openai / openaicompat | `response_format: {type: "json_schema", json_schema: {name, strict, schema}}` |
| gemini | `generationConfig.responseMimeType` + `generationConfig.responseSchema` |
| anthropic | `output_config.format: {type: "json_schema", schema}` |

And, importantly, the *schema dialects* differ too. OpenAI's strict mode demands
`additionalProperties: false` and every property listed in `required`. Gemini
accepts an **OpenAPI 3.0 subset**, not JSON Schema — no `$ref`, a narrower
keyword set. The same schema is not portable across all three, and no amount of
API design makes it so.

## Decision

A typed optional field, a verbatim schema, and no parsing.

```go
type ResponseFormat struct {
	Schema map[string]any // the JSON Schema the reply must satisfy
	Name   string         // OpenAI requires one; the others ignore it
}
```

`Request.ResponseFormat *ResponseFormat`, nil meaning "unconstrained".

**1. A typed field rather than documenting the `ProviderOptions` recipe.**
The escape hatch is the wrong tool precisely where it is most needed: on Gemini
it cannot express this without destroying sibling keys. A typed field composes
with the rest of the request; `ProviderOptions` still overrides it, because it is
still merged last.

**2. The schema is sent verbatim. skyl never rewrites it.**
`Request.Validate` checks that a non-nil `ResponseFormat` carries a non-empty
`Schema`, and nothing more — the same stance it already takes on
`Tool.Parameters`. A schema Gemini rejects comes back as Gemini's own 400.

The alternative — translating between dialects — is skyl guessing at semantics it
does not own, and this repository already has the evidence for how that ends. The
Anthropic adapter reconstructs *tool* schemas rather than passing them through,
and it costs two entries in the "silently ignored" column of the
[feature matrix](../feature-matrix.md): a top-level `type` coerced to `object`,
and non-string `required` entries dropped. Those are exactly the bugs a
translation layer produces — quiet, plausible, and discovered in production.

**3. Nothing is parsed on the way back.**
All three providers return the document in the ordinary assistant text channel,
so it arrives as a `skyl.Text` part and `Response.Text()` returns the JSON.
Callers unmarshal it themselves. skyl does not validate the reply against the
schema it sent, for the same reason it does not validate `ToolCall.Arguments`
against the tool's own schema: the schema is the caller's, the target type is the
caller's, and a partial validator invites the complacency that gets exploited.

## Consequences

**Good.** The feature works on all four adapters through one field, with no
vendor-specific code in the caller and no `ProviderOptions` collateral damage.
Schemas are sent exactly as written, so a caller who has tuned a schema against a
provider's own documentation gets that schema, not skyl's interpretation of it.
There is no dialect-translation layer to maintain as five vendors evolve their
schema support independently — the code that does not exist cannot rot.

**Bad.** A schema is not portable across providers, and skyl now has a field that
looks as though it should be. Someone will write a schema against OpenAI's strict
mode, switch `Model` to a Gemini one — which is the whole promise of the library
— and get a 400. The feature matrix says so explicitly and the field's doc
comment says so, but documentation is a weaker guarantee than a compile error,
and this is the cost of decision 2.

`strict: true` is sent unconditionally on OpenAI. That is the guarantee people
want the feature for, but it means a lax schema that would have worked in JSON
mode now fails. A caller who wants the looser behaviour must reach for
`ProviderOptions` — which here is safe, since OpenAI's key is top-level.

Callers do their own unmarshalling and their own validation. One line of
`json.Unmarshal` is a small tax; skipping the validation is a real risk that
belongs to the caller, who is the only party that knows what the data is for.

## Alternatives considered

- **Document a `ProviderOptions` recipe per provider and add no API.** Zero new
  surface, and it works on OpenAI and Anthropic. Rejected because it does not
  work on Gemini without silently discarding the caller's other generation
  settings, and a documented footgun is still a footgun.
- **Translate schemas per provider — strip `$ref` for Gemini, inject
  `additionalProperties: false` for OpenAI.** Portability, at the price of skyl
  quietly changing the meaning of a caller's schema. The tool-schema
  reconstruction in `provider/anthropic` is the same idea already in the tree,
  and it is on the silently-ignored list. Rejected.
- **Accept a Go type and generate the schema by reflection.** Excellent
  ergonomics, and it is what the vendor SDKs do. Rejected for now: it needs a
  reflection-based schema generator in a module whose defining constraint is zero
  dependencies (rules.md §4.1), and it forecloses nothing — it can be added later
  as a helper that produces the `map[string]any` this field already takes.
- **Parse the reply into `Response`, or add a `Structured` part type.** The
  `Part` set is closed and documented as closed; widening it breaks every
  exhaustive type switch outside this repository. A `Response.Unmarshal` helper
  was also considered and deferred: it saves one line, and every exported symbol
  added before v1.0.0 is one more thing frozen at it.
