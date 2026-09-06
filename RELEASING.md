# Releasing skyl

skyl is four Go modules in one repository:

| Module | Path | Tag prefix |
|---|---|---|
| library | `github.com/BAGOMBEKA-JOB-DEV/skyl` | `v0.1.0` |
| Anthropic adapter | `.../skyl/provider/anthropic` | `provider/anthropic/v0.1.0` |
| OpenTelemetry | `.../skyl/otel` | `otel/v0.1.0` |
| gateway | `.../skyl/gateway` | `gateway/v0.1.0` |

Run `scripts/release.sh vX.Y.Z`. It performs every edit below and stops between
modules so you can confirm each tag landed. It never tags and never pushes.

The rest of this document is why the process has the shape it does. Read it
once; the ordering is not a style preference and getting it wrong publishes a
module that nobody can install.

## Why a module in a subdirectory needs a prefixed tag

Go finds a module in a subdirectory by looking for a tag whose prefix is that
subdirectory. `provider/anthropic/v0.1.0` publishes the adapter;
a bare `v0.1.0` publishes only the root module. Tagging one does not tag the
other, and the version numbers do not have to move together — though keeping
them aligned is far easier to reason about.

## Why `replace` must go, and why it is not the real problem

Every module except the root carries a `replace` pointing at a sibling directory
so the repository builds during development. **A `replace` directive is honoured only
in the main module.** When somebody else runs `go get`, their module is the main
module, so ours is ignored entirely.

That means the `replace` is not what breaks an install — it is what *hides* the
break. The `require` line is the only thing a consumer's build sees, and while
it says `v0.0.0` the resolve fails against a version that will never exist.

Locally this repository uses a [`go.work`](go.work) workspace instead, which Go
never consults when skyl is somebody's dependency. It cannot leak.

CI builds every module with `GOWORK=off` for the same reason: with the workspace
active, a broken `require` resolves from the local directory and nobody notices
until a user tries to install a published version.

## Why the order is not optional

Each module's `go.sum` must contain a checksum for the version it depends on,
and a checksum can only be computed for a version the proxy can already serve.
So:

1. Tag and push the **root** module. Nothing else can be prepared until this
   version is resolvable.
2. Point `provider/anthropic` at it, tidy, commit, tag, push.
3. Point `otel` at it, tidy, commit, tag, push. It depends only on the root, so
   it does not have to wait for the adapter — but it is sequenced after it so
   the script has one linear path to follow.
4. Point `gateway` at the root, the adapter **and** `otel`, tidy, commit, tag,
   push. It goes last because it is the only module depending on other
   submodules — and it depends on both of them. Miss one and the published
   module carries a `v0.0.0` require that nobody outside this repository can
   resolve.

Between steps 1 and 2 the tree does not build with `GOWORK=off`, because the new
root version has to be fetched from the proxy. That is inherent to the layout,
not a fault in it — the workspace covers ordinary development throughout.

Note that the submodules' `go.sum` files today contain no entry for the root
module at all. That is the expected consequence of a directory `replace`:
replaced modules get no checksums. They gain one at release, which is a useful
signal that the rewrite actually happened.

## Before tagging

- CI green on `main`, including the tagged suites.
- `CHANGELOG.md` has a section for the version, with a comparison link.
- Any breaking change since the last tag is listed there with a migration note
  (`docs/rules.md` §1.2).
- The `go` directive in each `go.mod` still matches what the code needs. CI
  builds each module against its declared floor, so a stale directive shows up
  as a build failure rather than as a user's bug report.

## After tagging

Verify from outside the repository, because that is the only test that reflects
what a user experiences:

```bash
cd $(mktemp -d) && go mod init check
go get github.com/BAGOMBEKA-JOB-DEV/skyl/provider/anthropic@vX.Y.Z
go get github.com/BAGOMBEKA-JOB-DEV/skyl/otel@vX.Y.Z
go get github.com/BAGOMBEKA-JOB-DEV/skyl/gateway@vX.Y.Z
```

**Tags on the module proxy are immutable.** Deleting and re-pushing a tag does
not help: the proxy has already cached the original content and will keep
serving it. A broken release is fixed forward with a new patch version, never by
retagging.

## After v1.0.0

The exported API is frozen. A breaking change needs a **major** version, which
for a Go module means a new import path — `.../skyl/v2` — and a `/v2` directory
or branch. That cost is the point: it makes breaking a deliberate act rather
than an oversight.

Everything additive is a minor release; a fix is a patch. Both are listed in
`CHANGELOG.md`. See `docs/rules.md` §1.2.
