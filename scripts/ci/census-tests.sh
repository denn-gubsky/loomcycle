#!/usr/bin/env bash
# Runs every census and drift test, whatever a pull request changed.
#
# A pull request's Go jobs run only the packages its change can reach
# (scripts/ci/affected), and the TypeScript and Python jobs only when their
# own tree changed. A census or drift test reads files no import names — Go
# source walked at run time, the proto, the other adapters — so a change
# elsewhere could break one and the pull request would stay green; main then
# went red after the merge (a TS drift test six times in one day, a census
# in api/http, another in tools/builtin). This runs them all, without -race:
# they check what the tree says, not timing, and the race shards still run
# them under -race wherever the selection reaches.
#
# Which tests: a Go test whose name has one of the words in GO_PATTERN (the
# naming convention for "every X has a Y" tests), and a TS or Python test file
# that opens a file outside its own adapter. A new census test is picked up by
# its name; one named otherwise runs only where the selection reaches it.
#
#   scripts/ci/census-tests.sh go|ts|python
set -euo pipefail

GO_PATTERN='Census|Drift|Twin|Parity|Every|Complete|Listed'

root="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$root"

case "${1:-}" in
go)
  dirs=$(git grep -lE "^func Test[A-Za-z0-9_]*(${GO_PATTERN})" -- '*_test.go' ':!**/testdata/**' |
    xargs -n1 dirname | sort -u | sed 's|^|./|' | tr '\n' ' ')
  bin="${RUNNER_TEMP:-${TMPDIR:-/tmp}}/loomcycle-affected"
  go build -o "$bin" ./scripts/ci/affected
  pkgs=$("$bin" -all -within "$dirs" | tr '\n' ' ')
  # shellcheck disable=SC2086 # pkgs is a space-separated list
  scripts/ci/in-modules.sh go test -count=1 -timeout 1200s \
    -run "^Test[A-Za-z0-9_]*(${GO_PATTERN})" -- $pkgs
  ;;
ts)
  # A test that reads the Go tree, the proto or the docs (../../ from tests/).
  files=$(cd adapters/ts && grep -lE "\.\./\.\./|\"\.\.\", \"\.\.\"" tests/*.test.ts || true)
  [ -n "$files" ] || { echo "no cross-tree TS tests" >&2; exit 1; }
  # shellcheck disable=SC2086 # files is a space-separated list
  (cd adapters/ts && npx vitest run $files)
  ;;
python)
  # A test that reads outside the adapter (pathlib parents[N] up to the repo).
  files=$(cd adapters/python && grep -lE "parents\[[2-9]\]" tests/*.py || true)
  [ -n "$files" ] || { echo "no cross-tree Python tests" >&2; exit 1; }
  # shellcheck disable=SC2086 # files is a space-separated list
  (cd adapters/python && python -m pytest -q $files)
  ;;
*)
  echo "usage: census-tests.sh go|ts|python" >&2
  exit 2
  ;;
esac
