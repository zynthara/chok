#!/usr/bin/env bash
# apidiff gate (SPEC §7.4): incompatible public-API changes against
# the release baseline must ship with a CHANGELOG.md entry.
#
# Baseline policy: .apidiff-baseline names the anchor tag. Until that
# tag exists (pre-v2.0.0-beta.1), the gate is armed but skips — there
# is no published v2 API to diff against, and diffing v0.1.4 would be
# meaningless across module paths. The moment the tag is pushed, the
# same file makes every subsequent run diff for real.
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

BASELINE_FILE=".apidiff-baseline"
MOD="github.com/zynthara/chok/v2"
# Pinned so CI stays reproducible; bump deliberately.
APIDIFF="golang.org/x/exp/cmd/apidiff@v0.0.0-20260611194520-c48552f49976"

BASE_TAG=$(tr -d '[:space:]' < "$BASELINE_FILE" 2>/dev/null || true)
if [ -z "$BASE_TAG" ]; then
  echo "apidiff: no baseline configured in $BASELINE_FILE — gate skipped"
  exit 0
fi

# Shallow CI checkouts don't carry tags.
git fetch --tags --quiet 2>/dev/null || true
if ! git rev-parse -q --verify "refs/tags/$BASE_TAG" >/dev/null; then
  echo "apidiff: baseline tag $BASE_TAG not published yet — gate armed, skipping"
  exit 0
fi

TMP=$(mktemp -d)
cleanup() {
  git worktree remove --force "$TMP/base" 2>/dev/null || true
  rm -rf "$TMP"
}
trap cleanup EXIT

echo "apidiff: baseline $BASE_TAG"
git worktree add -q "$TMP/base" "$BASE_TAG"
(cd "$TMP/base" && go run "$APIDIFF" -m -w "$TMP/old.export" "$MOD")
go run "$APIDIFF" -m -w "$TMP/new.export" "$MOD"

REPORT=$(go run "$APIDIFF" -m -incompatible "$TMP/old.export" "$TMP/new.export")
if [ -z "$REPORT" ]; then
  echo "apidiff: no incompatible API changes vs $BASE_TAG"
  exit 0
fi

echo "apidiff: incompatible changes vs $BASE_TAG:"
echo "$REPORT"
if git diff --name-only "$BASE_TAG"..HEAD -- CHANGELOG.md | grep -q CHANGELOG.md; then
  echo "apidiff: CHANGELOG.md has an entry since $BASE_TAG — OK"
  exit 0
fi
echo "apidiff: public API changed but CHANGELOG.md is untouched since $BASE_TAG."
echo "         Document the change in CHANGELOG.md (the release discipline is manual now)."
exit 1
