# Engineering rules

These are the standards every change to skyl must meet. They are not
aspirational: CI enforces most of them, and review enforces the rest.

If a rule blocks something genuinely necessary, change the rule in a PR and say
why. Do not route around it silently.

---

## 1. Public API

**1.1 — Every exported symbol has a doc comment**, starting with its name, in
full sentences. `go vet` and the linter enforce the form; review enforces that
it says something useful. "Client is a client." is not a doc comment.

**1.2 — No breaking changes to exported API after v1.0.0** without a major
version bump and a migration note in `CHANGELOG.md`. Before v1, breaking changes
are allowed but must appear in the changelog.

**1.3 — Accept interfaces, return structs.** Constructors return concrete types
so callers can see what they have; parameters take the narrowest interface that
works.

**1.4 — Functional options for anything optional.** `New(required, ...Option)`.
Never a growing positional parameter list, and never an exported config struct
that can't gain a field without breaking users.

**1.5 — `context.Context` is the first parameter** of every function that does
I/O, and it must be honoured — passed to the request, not accepted and ignored.

**1.6 — No `panic` in library code.** Return an error. The only permitted panics
are for programmer error that cannot be recovered from (a nil `Provider` passed
to `New`), and those are documented on the function.

---

## 2. Errors

**2.1 — Wrap with `%w`.** Callers must be able to `errors.Is` through every
layer.

**2.2 — Classify, don't stringify.** Adapters map vendor errors to a sentinel
(`ErrRateLimit`, `ErrAuth`, …). Nothing in skyl may branch on error message
text — that breaks the moment a vendor rewords a message.

**2.3 — Error strings are lowercase and unpunctuated**, per Go convention, and
prefixed `skyl:` at the boundary.

**2.4 — Never discard an error.** `_ =` requires a comment explaining why the
error genuinely cannot matter.

**2.5 — Errors never contain credentials.** API keys must not reach an error
string, a log line, or `Error.Body`. There is a test for this.

---

## 3. Testing

**3.1 — Every exported function has a test.** No exceptions for "obvious" code;
obvious code is where the embarrassing bugs live.

**3.1a — Every adapter runs the shared contract suite** in
`internal/providertest`. It enforces §6 uniformly, so a rule added there is
enforced everywhere at once and no adapter can regress behind another's tests.

**3.2 — Table-driven tests** with named cases. The name is printed on failure,
so it must identify the case: `"rate limit with retry-after header"`, not
`"case 3"`.

**3.3 — No network access in unit tests.** Use `httptest.Server`. A test suite
that needs the internet is a test suite that doesn't run.

**3.4 — Test the error paths.** Every branch of a classifier, every malformed
payload, every truncated stream. Coverage of happy paths only is theatre.

**3.5 — `t.Parallel()` where safe**, and the suite must pass under `-race`.

**3.6 — Streaming tests assert no goroutine leaks**, including on early
`Close()` and on context cancellation mid-stream. Use
`testutil.CheckNoGoroutineLeaks`, and take the baseline *after* any test server
is up — otherwise you measure `net/http`, not skyl. `-race` does not catch a
leak: a goroutine merely blocked forever is not a data race.

**3.7 — Integration tests are build-tagged `integration`** and never run in
default CI. They cost real money. CI does run `go vet -tags=integration` so
they cannot rot uncompiled. To run them:

```bash
export ANTHROPIC_API_KEY=... OPENAI_API_KEY=... GEMINI_API_KEY=...
go test -tags=integration ./provider/
cd provider/anthropic && go test -tags=integration ./...
```

Each provider skips when its key is unset, so a partial key set still runs
what it can.

**3.8 — Fix the code, not the test.** A test changed to match broken behaviour
must be justified explicitly in the PR.

---

## 4. Dependencies

**4.1 — The core module has zero external dependencies.** Standard library
only. No logger, no config loader, no router, no assertion library. An adapter
that needs a vendor SDK goes in its own module, as `provider/anthropic` does
([ADR-0006](adr/0006-anthropic-adapter-is-its-own-module.md)).

**4.2 — Adding a core dependency requires an ADR.** Justify it, name what you
considered, and say what it costs every downstream user.

**4.3 — The gateway module may take what a server needs** (chi, and little
else). It is separate precisely so those choices don't leak into the library.

**4.4 — Prefer the standard library.** `net/http`, `encoding/json`, `log/slog`,
`testing`. They are already in every user's dependency graph.

---

## 5. Concurrency

**5.1 — `Client` and all adapters are goroutine-safe.** Document it; test it
under `-race`.

**5.2 — Every goroutine has a defined exit.** Tied to a context or a closed
channel. A goroutine with no exit path is a leak, and leaks are bugs even when
they're small.

**5.3 — Never start a goroutine a caller can't stop.**

**5.4 — Streams are single-consumer** and say so in their doc comment.

---

## 6. Provider adapters

**6.1 — Never silently drop request data.** If an adapter cannot represent
something, it returns `ErrUnsupported` naming the field. Dropping a part quietly
produces a bug the user cannot diagnose.

**6.2 — Always populate `Response.Raw`.** It is the user's escape hatch.

**6.3 — Never validate model IDs against a list.** Pass them through. See
[ADR-0004](adr/0004-model-ids-are-pass-through.md).

**6.4 — Always set `Response.Provider` and `Response.Model`** from the *actual*
response, not echoed from the request — providers can and do serve a different
model than the one asked for.

**6.5 — Honour `Request.ProviderOptions`**, merged into the outbound payload
without skyl second-guessing the contents.

---

## 7. Security

**7.1 — Credentials come from the caller or the environment.** Never a default,
never a file skyl reads on its own initiative, never hardcoded.

**7.2 — Credentials never appear in errors, logs, or `String()` output.**
Redact at construction, not at print time.

**7.3 — TLS verification is never disabled**, and no option exists to disable
it. A user who needs a custom trust store supplies their own `*http.Client`.

**7.4 — Respect `context` deadlines** so a hung provider can't exhaust the
caller's resources.

**7.5 — The gateway authenticates by default.** It must not be possible to
start it accidentally as an open relay to paid APIs.

---

## 8. Documentation

**8.1 — A user-visible change updates the docs in the same PR.** Documentation
written "later" is documentation written never.

**8.2 — Architectural decisions get an ADR.** Cheap to write, invaluable in
eighteen months when someone asks why.

**8.3 — Examples must compile.** They live in `_test.go` files as `Example`
functions so CI breaks when they rot.

**8.4 — Document the limitation next to the feature.** If an adapter doesn't
support streaming tool calls, that belongs in its doc comment, not only in a
release note.

---

## 9. Git

**9.1 — `main` is the trunk** and is always releasable.

**9.2 — All work happens on feature branches** — `feat/`, `fix/`, `docs/`,
`refactor/`, `test/`, `chore/` — and merges via PR.

**9.3 — Conventional Commits** (`feat:`, `fix:`, `docs:`, …) so the changelog
can be assembled mechanically.

**9.4 — Never force-push a shared branch.**

**9.5 — CI must be green to merge.** Not "green except that one flake" — a
flaky test is a broken test.

**9.6 — Every commit carries a `Signed-off-by` line.** Add one with
`git commit -s`, or let the hook in `.githooks/` do it — see
[CONTRIBUTING.md](../CONTRIBUTING.md).

The line is not a formality and not a signature. It is the
[Developer Certificate of Origin](https://developercertificate.org): by adding
it you state that you wrote the change, or that you have the right to submit it
under Apache 2.0. That is the difference between this project *asserting* its
contributions are Apache-2.0 and each contributor actually saying so.

A DCO is deliberately lighter than a CLA: no signature, no paperwork, no
account with a third party. CI enforces it per commit. If you forget, the fix
is one command and the failing check prints it:
`git rebase --signoff <base>`.

---

## 10. Formatting and tooling

Enforced in CI; run them before pushing:

```bash
gofmt -l .          # must print nothing
go vet ./...
go test -race ./...
```
