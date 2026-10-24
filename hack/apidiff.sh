#!/usr/bin/env bash
# apidiff.sh — incompatible changes to chok v3's public API since the
# baseline release; `make apidiff`. CI runs it in its own job, advisory
# (continue-on-error) while v3 is in alpha; it becomes blocking before
# v3.0.0.
#
# The baseline is v3.0.0-alpha.1, or the git ref APIDIFF_BASE names; move
# it to each new release. Until the baseline exists the script prints
# "no baseline tag yet" and exits 0. An incompatible change fails the
# script unless CHANGELOG.md has changed since the baseline.
# Portable to the bash 3.2 that macOS ships.
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

BASE="${APIDIFF_BASE:-v3.0.0-alpha.1}"
MOD="github.com/zynthara/chok/v3"
# Pinned so runs are reproducible; bump deliberately.
APIDIFF="golang.org/x/exp/cmd/apidiff@v0.0.0-20260611194520-c48552f49976"

# CI's shallow checkout carries no tags.
git fetch --tags --quiet 2>/dev/null || true
if ! git rev-parse -q --verify "$BASE^{commit}" >/dev/null; then
	echo "apidiff: no baseline tag yet ($BASE does not exist) — nothing to compare"
	exit 0
fi

tmp=$(mktemp -d)
cleanup() {
	git worktree remove --force "$tmp/base" 2>/dev/null || true
	rm -rf "$tmp"
}
trap cleanup EXIT

echo "apidiff: $MOD at HEAD against $BASE"
git worktree add -q --detach "$tmp/base" "$BASE"
(cd "$tmp/base" && go run "$APIDIFF" -m -w "$tmp/old.export" "$MOD")
go run "$APIDIFF" -m -w "$tmp/new.export" "$MOD"

report=$(go run "$APIDIFF" -m -incompatible "$tmp/old.export" "$tmp/new.export")
if [ -z "$report" ]; then
	echo "apidiff: no incompatible changes since $BASE"
	exit 0
fi
echo "apidiff: incompatible changes since $BASE:"
echo "$report"
if git diff --name-only "$BASE" HEAD -- CHANGELOG.md | grep -q CHANGELOG.md; then
	echo "apidiff: CHANGELOG.md has changed since $BASE — OK while the changes are listed there"
	exit 0
fi
echo "apidiff: the public API changed incompatibly and CHANGELOG.md has not changed since $BASE."
echo "         Describe the change in CHANGELOG.md."
exit 1
