# Security policy

## Reporting a vulnerability

**Do not open a public issue for a security vulnerability.**

Report it privately through
[GitHub Security Advisories](https://github.com/BAGOMBEKA-JOB-DEV/skyl/security/advisories/new)
on this repository.

Please include the affected version, a description of the impact, and
reproduction steps. You will get an acknowledgement, and we will tell you
whether it is accepted and when a fix is expected. We will credit you in the
advisory unless you would rather we didn't.

## Supported versions

skyl is pre-v1. Fixes land on `main` and in the next release. Once v1.0.0 ships,
this section will state a support window.

## How skyl handles credentials

Provider API keys are the most sensitive thing skyl touches. The rules are
enforced by [docs/rules.md §7](docs/rules.md#7-security) and by tests.

- **Credentials come from you.** Passed to a constructor or read from an
  environment variable you name. skyl never reads a credential file on its own
  initiative and ships no default key.
- **Credentials never appear in errors.** `*skyl.Error` carries provider name,
  status code, and message — never the key, and never an `Authorization` header.
- **Credentials are never logged.** The library does no logging at all. The
  gateway logs method, path, status, and duration — never headers, never bodies.
- **TLS verification cannot be disabled.** There is no option for it. If you
  need a custom trust store, supply your own `*http.Client` through the
  provider's `WithHTTPClient` option.

## Gateway

The gateway proxies **paid** APIs, so a misconfiguration means someone else
spending your money.

- **Authentication is mandatory.** The gateway refuses to start without
  `SKYL_AUTH_TOKEN`. There is deliberately no flag to turn it off — an open
  relay to billed endpoints must not be one environment variable away.
- Tokens are compared with `crypto/subtle.ConstantTimeCompare`.
- Upstream provider errors are classified and re-emitted; raw provider bodies
  are not forwarded verbatim, since they can echo request content back to a
  caller who should not see it.
- **Deploy it on a private network.** It is an internal service. If it must be
  internet-facing, terminate TLS and rate-limit at a reverse proxy in front of
  it.

## Model output is untrusted input

This is not a skyl vulnerability class, but it is the most common way
applications built on libraries like this one get compromised.

Text returned by a model is **untrusted**. It may be shaped by anything in the
context window, including content a third party controls (a fetched web page, a
user upload, a tool result). Treat it exactly as you would treat a form field:

- Never `exec` model output.
- Never interpolate it into SQL, shell commands, or file paths.
- Escape it before rendering it as HTML.
- Gate tool calls with side effects — sending mail, writing to a database,
  spending money — behind validation or human approval.

skyl deliberately does not sanitise model output. It cannot know your context,
and a sanitiser that works most of the time would encourage exactly the
complacency that gets exploited.
