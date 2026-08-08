# skyl documentation

Start with **[idea.md](idea.md)** for why skyl exists, or
**[getting-started.md](getting-started.md)** to make a call in five minutes.

## Guides

| Document | Read it when |
|---|---|
| [idea.md](idea.md) | You want the problem statement, design principles, and non-goals |
| [getting-started.md](getting-started.md) | You want working code: install, first call, streaming, tools, errors |
| [providers.md](providers.md) | You need to know which providers and models are reachable, and how discovery works |
| [gateway.md](gateway.md) | You want to expose skyl over HTTP |
| [skyl_infrastructure ↗](https://github.com/BAGOMBEKA-JOB-DEV/skyl_infrastructure) | You want to run the gateway on Kubernetes — Terraform for AWS, GCP or Azure, and a Helm chart. Separate repository |
| [sandbox.md](sandbox.md) | You want to develop or test without credentials, or to force provider failures |
| [validating.md](validating.md) | You have provider keys and want to close the live-validation gap |
| [architecture.md](architecture.md) | You are contributing, or want to know why it is built this way |
| [project-plan.md](project-plan.md) | You want milestone status and what is coming |
| [rules.md](rules.md) | You are about to open a PR |

## Architecture decision records

Short records of decisions that were not obvious, kept so the reasoning survives
after the discussion is forgotten.

| ADR | Decision |
|---|---|
| [0001](adr/0001-two-module-layout.md) | Separate modules for library and gateway |
| [0002](adr/0002-provider-interface.md) | A four-method `Provider` interface |
| [0003](adr/0003-gateway-as-separate-module.md) | chi belongs to the gateway, not the library |
| [0004](adr/0004-model-ids-are-pass-through.md) | Model IDs are opaque pass-through strings |
| [0005](adr/0005-no-copilot-provider.md) | No GitHub Copilot provider |
| [0006](adr/0006-anthropic-adapter-is-its-own-module.md) | The Anthropic adapter is its own module |
| [0007](adr/0007-otel-is-its-own-module.md) | OpenTelemetry instrumentation is its own module |

New ADRs: copy the format of an existing one — Context, Decision, Consequences
(good *and* bad), Alternatives considered. An ADR that lists no downside has not
finished thinking.

## Repository docs

- [../README.md](../README.md) — project overview
- [../CONTRIBUTING.md](../CONTRIBUTING.md) — how to land a change
- [../SECURITY.md](../SECURITY.md) — vulnerability reporting, credential handling
- [../CHANGELOG.md](../CHANGELOG.md) — release history

## Evaluating skyl

- [feature-matrix.md](feature-matrix.md) — what each adapter supports, and what
  it silently ignores
- [data-handling.md](data-handling.md) — where prompts go
- [threat-model.md](threat-model.md) — trust boundaries and attacker capability
- [benchmarks.md](benchmarks.md) — measured figures
- [migrating.md](migrating.md) — moving from another library
