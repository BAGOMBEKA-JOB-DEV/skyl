# Maintainers

| Name | GitHub | Scope |
|---|---|---|
| BAGOMBEKA JOB | [@BAGOMBEKA-JOB-DEV](https://github.com/BAGOMBEKA-JOB-DEV) | everything |

## Bus factor

**One.** Every commit in this repository is from one person, and every pull
request has been self-reviewed.

That is worth stating plainly rather than leaving to be discovered. If you are
evaluating skyl as a dependency, it is the single most important thing to know
about the project's continuity, and it is not something documentation, test
coverage or CI can compensate for.

What reduces the risk, concretely:

- The core library has **no external dependencies**, so it cannot rot through
  someone else's abandonment.
- `docs/adr/` records why each load-bearing decision was made, so a new
  maintainer inherits the reasoning and not just the code.
- `docs/rules.md` states the standards a change must meet, and CI enforces most
  of them.
- The Apache-2.0 licence permits a fork without asking anyone.

## Becoming a maintainer

There is no process yet, because there has been no candidate. If you are
contributing regularly and want one, open an issue and it will be written.

## If this project becomes unmaintained

The honest failure mode for a one-person project is silence rather than an
announcement. If there has been no response to an issue or a security report in
90 days, treat skyl as unmaintained and fork it. The licence allows it and
nothing here is designed to make it difficult.
