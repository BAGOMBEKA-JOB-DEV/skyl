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
step "waiting for the proxy to serve $ROOT_MODULE@$VERSION"
for attempt in $(seq 1 30); do
	if go list -m "$ROOT_MODULE@$VERSION" >/dev/null 2>&1; then
		echo "resolved."
		break
	fi
	if [[ $attempt -eq 30 ]]; then
		echo "error: $ROOT_MODULE@$VERSION is still not resolvable" >&2
		echo "hint: GOPROXY may be caching; try GOPRIVATE or wait and re-run" >&2
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

# gateway goes last: it is the only module that depends on another submodule.
step "4/4  gateway"
retarget gateway "$ROOT_MODULE"
retarget gateway "$ROOT_MODULE/provider/anthropic"
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
