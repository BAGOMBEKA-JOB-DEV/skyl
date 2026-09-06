# Security policy

## Reporting a vulnerability

**Do not open a public issue for a security vulnerability.**

Report it privately through
[GitHub Security Advisories](https://github.com/BAGOMBEKA-JOB-DEV/skyl/security/advisories/new)
on this repository.

Please include the affected version, a description of the impact, and
reproduction steps. We will credit you in the advisory unless you would rather
we didn't.

### What to expect, and the honest limit

- **Acknowledgement within 3 working days.**
- **An assessment within 10 working days** — whether it is accepted, and if so
  the severity and a target for the fix.
- **Disclosure once a fix is released**, or after 90 days, whichever comes
  first. If you would rather disclose sooner, say so and we will work to it.

skyl has **one maintainer** (see [MAINTAINERS.md](MAINTAINERS.md)). Those
windows are what one person can commit to, not what a funded security team would
offer, and there is no rota covering illness or holiday. If a report goes
unanswered past 10 working days, treat that as the process having failed rather
than as the report being dismissed, and disclose on whatever timeline you judge
right — you are not bound by an embargo nobody is upholding.

## Supported versions

Security fixes land on `main` and in the next release, and are backported to the
current minor series as a patch.

**Supported: v1.x.** v0.1.0 is superseded and receives nothing — upgrading to
v1.x breaks no API, so there is no cost to moving off it.

## How skyl handles credentials

Provider API keys are the most sensitive thing skyl touches. The rules are
enforced by [docs/rules.md §7](docs/rules.md#7-security) and by tests.

- **Credentials come from you.** Passed to a constructor or read from an
  environment variable you name. skyl itself never reads a credential file and
  ships no default key.

  One caveat, because a guarantee that is not quite true is worse than none:
  this covers skyl, not everything skyl depends on. `provider/anthropic` is
  built on the official `anthropic-sdk-go`, which on its own initiative reads
  `ANTHROPIC_PROFILE` and profile files under the Anthropic config directory,
  and honours `ANTHROPIC_BASE_URL` and `ANTHROPIC_CUSTOM_HEADERS`. A key you
  pass explicitly wins over anything it finds — but `ANTHROPIC_BASE_URL` will
  redirect your prompts *and* your key to whatever host it names. If that
  matters in your environment, unset those variables or pass
  `anthropic.WithBaseURL` explicitly. The other three adapters have no such
  behaviour; they read nothing you did not give them.
- **Credentials never appear in errors.** `*skyl.Error` carries provider name,
  status code, and message — never the key, and never an `Authorization` header.
- **Credentials are never logged.** The library does no logging at all. The
  gateway logs method, path, status, duration, a request ID and a caller label
  — never headers, never bodies, never the query string.

  Note the caller label is a name *you* choose in `SKYL_AUTH_TOKENS`, and it is
  written to your logs. Do not name a token after its own value.
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
