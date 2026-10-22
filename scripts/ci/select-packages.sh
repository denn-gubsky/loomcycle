#!/usr/bin/env bash
# Prints, space-separated, the packages a CI test step runs for PATTERN.
#
# On a pull request: only the packages whose tests can see the change
# (scripts/ci/affected), measured from the merge commit's first parent, which
# is the base branch as GitHub merged the pull request onto it. That needs the
# checkout to fetch two commits (fetch-depth: 2). Anywhere else (a push to
# main): every package PATTERN names, so main always runs the full suite and a
# test the selection cannot see still fails there.
#
# Empty output means the step has nothing to run.
set -euo pipefail

pattern="${1:?usage: select-packages.sh PATTERN}"

if [ "${GITHUB_EVENT_NAME:-}" != "pull_request" ]; then
  go list "$pattern" | tr '\n' ' '
  exit 0
fi

bin="${RUNNER_TEMP:-${TMPDIR:-/tmp}}/loomcycle-affected"
[ -x "$bin" ] || go build -o "$bin" ./scripts/ci/affected
# -explain goes to stderr, so the job log says why each package runs.
"$bin" -base HEAD^1 -head HEAD -within "$pattern" -explain | tr '\n' ' '
