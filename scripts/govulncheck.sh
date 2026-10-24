#!/usr/bin/env bash
# Run govulncheck with picolet's build tags and fail on any called
# vulnerability that is not listed in .github/govulncheck-exclusions.txt.
#
# Usage: BUILD_TAGS=<tags> scripts/govulncheck.sh
# Requires: go, jq.
set -euo pipefail

GOVULNCHECK_VERSION="v1.9.0"
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
exclusions="$root/.github/govulncheck-exclusions.txt"
: "${BUILD_TAGS:?BUILD_TAGS must be set to the Taskfile build tags}"

report="$(mktemp)"
trap 'rm -f "$report"' EXIT

cd "$root"
# JSON mode exits 0 when it finds vulnerabilities; a non-zero exit is a
# scanner error and fails the script through set -e.
go run "golang.org/x/vuln/cmd/govulncheck@$GOVULNCHECK_VERSION" \
  -format json -tags "$BUILD_TAGS" ./... > "$report"

go_version="$(jq -r 'select(.config) | .config.go_version' "$report")"
echo "govulncheck $GOVULNCHECK_VERSION, $go_version"

# Called findings only (a trace that ends in a function), as in the text
# report; imported-but-not-called findings are informational.
found="$(jq -r 'select(.finding) | .finding | select(.trace[0].function != null) | .osv' "$report" | sort -u)"
excluded="$(sed -E 's/#.*//; s/[[:space:]]+//g' "$exclusions" | grep -v '^$' | sort -u)"

new="$(comm -23 <(printf '%s\n' "$found" | grep -v '^$' || true) <(printf '%s\n' "$excluded"))"
stale="$(comm -13 <(printf '%s\n' "$found" | grep -v '^$' || true) <(printf '%s\n' "$excluded"))"

if [ -n "$stale" ]; then
  while read -r id; do
    echo "::warning::$id is excluded in .github/govulncheck-exclusions.txt but no longer found; remove it"
  done <<< "$stale"
fi

if [ -n "$excluded" ]; then
  echo "Excluded (pre-existing, see .github/govulncheck-exclusions.txt): $(printf '%s' "$excluded" | tr '\n' ' ')"
fi

if [ -z "$new" ]; then
  echo "No new vulnerabilities."
  exit 0
fi

while read -r id; do
  summary="$(jq -r --arg id "$id" 'select(.osv) | .osv | select(.id == $id) | .summary' "$report" | head -n1)"
  module="$(jq -r --arg id "$id" 'select(.finding) | .finding | select(.osv == $id and .trace[0].function != null) | "\(.trace[0].module)@\(.trace[0].version)"' "$report" | sort -u | paste -sd ' ' -)"
  fixed="$(jq -r --arg id "$id" 'select(.finding) | .finding | select(.osv == $id) | .fixed_version // empty' "$report" | sort -u | paste -sd ' ' -)"
  echo "::error::$id ($module): $summary${fixed:+ (fixed in $fixed)}"
done <<< "$new"
echo "Run govulncheck -tags \"\$BUILD_TAGS\" ./... for call traces." >&2
exit 1
