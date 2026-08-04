---
name: Bug report
about: Something behaves differently from what the documentation says
labels: bug
---

**Redact your API key before pasting anything.** Errors from skyl never contain
credentials, but a request body or a shell history line might.

## What happened

## What you expected

## Reproducing it

<!-- The smallest program that shows it. If it needs a provider, say which one
     and which model — skyl passes model IDs through untouched, so behaviour can
     differ between them. -->

```go
```

## Environment

- skyl version (or commit):
- Module: <!-- library / provider/anthropic / gateway / otel -->
- Provider and model:
- `go version`:

## Anything else

<!-- If the provider's own response would help, `Response.Raw` carries it
     untouched. Check it for anything sensitive first. -->
