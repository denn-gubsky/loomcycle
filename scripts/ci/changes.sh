#!/usr/bin/env bash
# Prints, as key=true|false lines for $GITHUB_OUTPUT, which parts of the tree
# a pull request changes, so a CI job whose part is untouched is skipped.
#
# Outside a pull request every key is true: main always runs every job. A
# change to the workflow or to these scripts also sets every key, so a change
# to how CI decides is proven on everything.
#
# go is decided by scripts/ci/affected, not by file extension: Go tests read
# the TS adapter, the proto, web/ and the bundles, so a change there concerns
# Go too. The census tests that walk the whole repository are left out of
# that decision (-no-walkers); when go is true they run with everything else.
#
# CI_DIFF_BASE and CI_DIFF_HEAD replay another change locally, e.g.
#   GITHUB_EVENT_NAME=pull_request CI_DIFF_BASE=abc^ CI_DIFF_HEAD=abc scripts/ci/changes.sh
set -euo pipefail

base="${CI_DIFF_BASE:-HEAD^1}"
head="${CI_DIFF_HEAD:-HEAD}"

keys=(go runtime web ts python bridges proto)

all() {
  for k in "${keys[@]}"; do echo "$k=true"; done
}

if [ "${GITHUB_EVENT_NAME:-}" != "pull_request" ]; then
  all
  exit 0
fi

changed=$(git diff --name-only --no-renames "$base" "$head")
echo "changed files:" >&2
sed 's/^/  /' <<<"$changed" >&2

if grep -qE '^(\.github/workflows/ci\.yml|scripts/ci/)' <<<"$changed"; then
  all
  exit 0
fi

has() { grep -qE "$1" <<<"$changed" && echo true || echo false; }

go=false
if grep -qE '^(go\.mod|go\.sum|Makefile)$' <<<"$changed"; then
  go=true
else
  bin="${RUNNER_TEMP:-${TMPDIR:-/tmp}}/loomcycle-affected"
  [ -x "$bin" ] || go build -o "$bin" ./scripts/ci/affected
  if [ -n "$("$bin" -base "$base" -head "$head" -no-walkers -explain)" ]; then
    go=true
  fi
fi

runtime=$go
if [ "$(has '^test/runtime/')" = true ]; then runtime=true; fi

echo "go=$go"
echo "runtime=$runtime"
echo "web=$(has '^(web|packages/(library|explorer|memory-view|def-fields))/')"
echo "ts=$(has '^adapters/ts/')"
echo "python=$(has '^adapters/python/')"
echo "bridges=$(has '^(packages/bridge-[^/]+|deploy/bridges)/')"
echo "proto=$(has '^(proto|internal/api/grpc/loomcyclepb|adapters/python/loomcycle/_generated)/|^(go\.mod|Makefile)$')"
