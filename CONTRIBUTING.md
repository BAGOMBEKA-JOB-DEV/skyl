# Contributing to skyl

Thanks for your interest. This document covers how to get a change landed.

Read [docs/rules.md](docs/rules.md) first — it is the standard your change will
be reviewed against, and it is short.

## Setup

```bash
git clone https://github.com/BAGOMBEKA-JOB-DEV/skyl.git
cd skyl
go build ./...
go test -race ./...
```

Requires **Go 1.26+**. There are two modules — run the gateway's suite too:

```bash
cd gateway && go test -race ./...
```

## Branches

`main` is the trunk and is always releasable. All work happens on a feature
branch and merges via pull request.

| Prefix | For |
|---|---|
| `feat/` | New capability |
| `fix/` | Bug fix |
| `docs/` | Documentation only |
| `test/` | Tests only |
| `refactor/` | No behaviour change |
| `chore/` | Tooling, CI, dependencies |

```bash
git checkout main && git pull
git checkout -b feat/cohere-provider
```

Never force-push a branch someone else may have pulled.

## Commits

[Conventional Commits](https://www.conventionalcommits.org/), so the changelog
can be assembled mechanically:

```
feat(provider): add Cohere adapter
fix(stream): release response body when Close is called before Next
docs(adr): record why model IDs are pass-through
```

Breaking changes get a `!` and a `BREAKING CHANGE:` footer explaining the
migration.

## Before you open a PR

```bash
gofmt -l .           # must print nothing
go vet ./...
go test -race ./...
```

CI runs the same commands on all three modules, plus `golangci-lint`, a
`go mod tidy` check, and a per-module coverage floor. A red build will not be
merged.

### Live provider tests

Unit tests replay payloads written from provider documentation, which proves
the mapping is self-consistent but not that it is *correct* — a fake echoes our
own assumptions back at us. The `integration`-tagged suite makes real calls:

```bash
export ANTHROPIC_API_KEY=... OPENAI_API_KEY=... GEMINI_API_KEY=...
go test -tags=integration ./provider/
cd provider/anthropic && go test -tags=integration ./...
```

Each provider skips when its key is unset. These cost money, so they never run
in default CI — but CI does `go vet -tags=integration` them, so they cannot rot
uncompiled. Run them before any release, and after any change to an adapter's
request or response mapping.

## What review will ask

Predictable, so you can pre-empt it:

- **Is it tested?** Every exported function has a test; error paths are covered
  as thoroughly as happy paths. See [rules.md §3](docs/rules.md#3-testing).
- **Does it add a core dependency?** That needs an ADR. See
  [§4](docs/rules.md#4-dependencies).
- **Does it drop request data?** Adapters must return `ErrUnsupported` rather
  than silently discarding a part. See [§6.1](docs/rules.md#6-provider-adapters).
- **Are errors classified, not stringified?** See [§2.2](docs/rules.md#2-errors).
- **Can a credential reach a log or an error?** See [§7.2](docs/rules.md#7-security).
- **Are the docs updated in the same PR?** See [§8.1](docs/rules.md#8-documentation).

## Adding a provider adapter

`Provider` is four methods; a new adapter is mostly mapping.

1. `provider/<name>/`, package `<name>`.
2. `New(apiKey string, opts ...Option) *Provider` — functional options, no
   exported config struct.
3. Implement `Complete`, `Stream`, `Models`, `Name`.
4. Map vendor errors onto skyl sentinels. Never branch on message text.
5. Always populate `Response.Raw`, `Response.Provider`, and `Response.Model`
   (the last from the *response*, not echoed from the request).
6. Never validate model IDs — see [ADR-0004](docs/adr/0004-model-ids-are-pass-through.md).
7. Tests against `httptest.Server` with recorded payloads. No network.
8. Run the shared contract suite — add a `providertest.Suite` entry so §6 is
   enforced on your adapter the same as every other.
9. Add a `providertest.Live` entry in the `integration`-tagged file.
10. Add it to [docs/providers.md](docs/providers.md) in the same PR.

**Before writing a native adapter, check whether the vendor serves OpenAI's wire
format** — if it does, it may need only a documented base URL for
`provider/openaicompat` rather than new code.

You do not have to contribute an adapter to use one. `Provider` is small enough
to implement in your own repository, and an out-of-tree adapter is a first-class
citizen — it gets retry, hooks, and the gateway for free.

## Architecture decision records

Anything structural — a new core dependency, a change to `Provider`, a new
module — needs an ADR in `docs/adr/`. Follow the existing format: Context,
Decision, Consequences (**good and bad**), Alternatives considered.

An ADR that lists no downsides has not finished thinking. Every real decision
costs something; say what.

## Reporting bugs

Include the provider and model, the skyl and Go versions, a minimal
reproduction, and what you expected. **Redact your API key** — including from
any error output you paste.

Security vulnerabilities do **not** go in a public issue. See
[SECURITY.md](SECURITY.md).

## License

Contributions are licensed under Apache 2.0, matching the project.
