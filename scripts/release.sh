#!/usr/bin/env bash
#
# Prepare a skyl release.
#
# skyl is four modules in one repository, and three of them depend on the first.
# Go resolves those dependencies through the module proxy, which means a
# submodule's `require` line must name a version that has actually been tagged.
# This script rewrites those lines, removes any `replace` directive, and tidies
# — one module at a time, in dependency order, because each step needs the
# previous version to be resolvable before its go.sum can be computed.
#
# It never creates a tag and never pushes. It prints the commands to run, and
# stops between modules so a human confirms each tag landed. See RELEASING.md
# for why the order is not optional.
#
#   scripts/release.sh v0.1.0
#
set -euo pipefail

VERSION="${1:-}"
if [[ -z "$VERSION" ]]; then
	echo "usage: scripts/release.sh vX.Y.Z" >&2
	exit 2
fi
if [[ ! "$VERSION" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]]; then
	echo "error: $VERSION is not a semantic version tag (want vX.Y.Z)" >&2
	exit 2
fi

ROOT_MODULE="github.com/BAGOMBEKA-JOB-DEV/skyl"
cd "$(dirname "$0")/.."

# A dirty tree means the rewrite below would be committed alongside whatever
# else is in flight, and a release commit must contain only the release.
if [[ -n "$(git status --porcelain)" ]]; then
	echo "error: working tree is dirty; commit or stash first" >&2
	git status --short >&2
	exit 1
fi

if git rev-parse "$VERSION" >/dev/null 2>&1; then
	echo "error: tag $VERSION already exists" >&2
	exit 1
fi

# The workspace makes every module resolve from the local directory, which is
# exactly what must NOT happen here: the point of this script is to prove each
# module resolves through the proxy.
export GOWORK=off

# Pin the toolchain unless the caller has an opinion.
#
# The modules declare floors of 1.22, 1.24 and 1.25.0. With GOTOOLCHAIN=auto and
# an older `go` on PATH, Go tries to fetch a toolchain named by the `go`
# directive — `go1.24`, which is not a release name — and fails with
# "toolchain not available" in the middle of a release. Naming a real patch
# release avoids that, and keeps every module on one toolchain besides.
export GOTOOLCHAIN="${GOTOOLCHAIN:-go1.25.0}"
if ! go version >/dev/null 2>&1; then
	echo "error: GOTOOLCHAIN=$GOTOOLCHAIN is not usable here" >&2
	echo "hint: set GOTOOLCHAIN to a toolchain you have, at least go1.25.0" >&2
	exit 1
fi

step() { printf '\n\033[1m==> %s\033[0m\n' "$1"; }

step "1/4  root module — $ROOT_MODULE@$VERSION"
echo "The root module has no intra-repo dependencies, so it needs no edit."
echo "Verify, then tag it:"
echo
echo "    go test ./... && git tag $VERSION && git push origin $VERSION"
echo
read -r -p "Press enter once $VERSION is pushed and resolvable, or ctrl-c to stop. "

# Wait for the proxy to serve it. Without this the tidy below fails with a
# confusing 'unknown revision' that looks like a broken tag rather than a cold
# cache.
#
# The lookup runs against a THROWAWAY module cache. Using the real one made this
# check answer from local state: a tag that had been created, fetched and later
# deleted still resolved from the module cache and from Go's VCS clone under
# $GOMODCACHE/cache/vcs, so the script reported "resolved." for a version that
# did not exist on the proxy at all — and then failed two steps later, somewhere
# that looked unrelated. A release gate that can pass on a phantom is worse than
# no gate.
step "waiting for the proxy to serve $ROOT_MODULE@$VERSION"
probe_cache="$(mktemp -d)"
trap 'chmod -R u+w "$probe_cache" 2>/dev/null; rm -rf "$probe_cache"' EXIT
for attempt in $(seq 1 30); do
	# GOTOOLCHAIN=local keeps the probe from re-downloading a toolchain into
	# the empty cache on every attempt — resolving a version needs no
	# particular language version, and thirty downloads would be absurd.
	if GOMODCACHE="$probe_cache" GOFLAGS=-mod=mod GOTOOLCHAIN=local \
		go list -m "$ROOT_MODULE@$VERSION" >/dev/null 2>&1; then
		echo "resolved."
		break
	fi
	if [[ $attempt -eq 30 ]]; then
		echo "error: $ROOT_MODULE@$VERSION is still not resolvable" >&2
		echo "hint: confirm the tag is pushed —" >&2
		echo "      git ls-remote --tags origin | grep $VERSION" >&2
		exit 1
	fi
	sleep 10
done

# retarget rewrites one module's dependency on another to a real version and
# drops every replace directive, which is the whole substance of a release here.
retarget() {
	local dir="$1" dep="$2"
	( cd "$dir"
	  go mod edit -require="$dep@$VERSION"
	  # A replace is ignored by consumers, so leaving one in a published module
	  # means the require line is the only thing that resolves — and a wrong
	  # require is invisible until someone outside this repo installs it.
	  go mod edit -dropreplace="$dep"
	)
}

step "2/4  provider/anthropic"
retarget provider/anthropic "$ROOT_MODULE"
( cd provider/anthropic && go mod tidy && go build ./... && go test ./... )
git --no-pager diff --stat provider/anthropic/
echo
echo "    git commit -am 'chore: release provider/anthropic $VERSION'"
echo "    git tag provider/anthropic/$VERSION && git push origin provider/anthropic/$VERSION"
echo
read -r -p "Press enter once provider/anthropic/$VERSION is pushed, or ctrl-c to stop. "

# otel depends only on the root, so it can be prepared as soon as the root tag
# is resolvable — it does not have to wait for the adapter.
step "3/4  otel"
retarget otel "$ROOT_MODULE"
( cd otel && go mod tidy && go build ./... && go test ./... )
git --no-pager diff --stat otel/
echo
echo "    git commit -am 'chore: release otel $VERSION'"
echo "    git tag otel/$VERSION && git push origin otel/$VERSION"
echo
read -r -p "Press enter once otel/$VERSION is pushed, or ctrl-c to stop. "

# gateway goes last: it is the only module that depends on other submodules, and
# it depends on BOTH of them. Missing one leaves a v0.0.0 require behind a
# replace that consumers never see, which publishes a module nobody can install
# — and the tag is immutable by the time anyone finds out.
step "4/4  gateway"
retarget gateway "$ROOT_MODULE"
retarget gateway "$ROOT_MODULE/provider/anthropic"
retarget gateway "$ROOT_MODULE/otel"

# Belt and braces: the loop above is easy to leave stale when a module is added,
# which is exactly what happened when otel was introduced. Fail loudly here
# rather than at the immutable tag.
if grep -qE '^\s*replace\s' gateway/go.mod ||
	grep -qE "$ROOT_MODULE[^ ]* v0\.0\.0" gateway/go.mod; then
	echo "error: gateway/go.mod still has a replace or a v0.0.0 require:" >&2
	grep -nE "^\s*replace\s|$ROOT_MODULE[^ ]* v0\.0\.0" gateway/go.mod >&2
	echo "hint: a repository module was added without updating this script" >&2
	exit 1
fi
( cd gateway && go mod tidy && go build ./... && go test ./... )
git --no-pager diff --stat gateway/
echo
echo "    git commit -am 'chore: release gateway $VERSION'"
echo "    git tag gateway/$VERSION && git push origin gateway/$VERSION"

step "done"
cat <<EOF
Confirm the release is real by installing it from outside this repository:

    cd \$(mktemp -d) && go mod init check
    go get $ROOT_MODULE/provider/anthropic@$VERSION
    go get $ROOT_MODULE/otel@$VERSION
    go get $ROOT_MODULE/gateway@$VERSION

Both must resolve without a replace directive. If either fails, the tag is
already immutable on the proxy — fix forward with a new patch version.
EOF
