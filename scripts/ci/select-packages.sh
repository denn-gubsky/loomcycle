#!/usr/bin/env bash
# Prints, space-separated, the packages a CI test step runs: those the
# space-separated go list patterns WITHIN name, less those EXCLUDE names.
#
# On a pull request: only the packages whose tests can see the change
# (scripts/ci/affected), measured from the merge commit's first parent, which
# is the base branch as GitHub merged the pull request onto it. That needs the
# checkout to fetch two commits (fetch-depth: 2). Anywhere else (a push to
# main): every package the patterns name, so main always runs the full suite
# and a test the selection cannot see still fails there.
#
# Empty output means the step has nothing to run.
set -euo pipefail

within="${1:?usage: select-packages.sh WITHIN [EXCLUDE]}"
exclude="${2:-}"

if [ "${GITHUB_EVENT_NAME:-}" != "pull_request" ]; then
  # shellcheck disable=SC2086 # patterns are space-separated on purpose
  if [ -n "$exclude" ]; then
    comm -23 <(go list $within | sort) <(go list $exclude | sort) | tr '\n' ' '
  else
    go list $within | tr '\n' ' '
  fi
  exit 0
fi

bin="${RUNNER_TEMP:-${TMPDIR:-/tmp}}/loomcycle-affected"
[ -x "$bin" ] || go build -o "$bin" ./scripts/ci/affected
# -explain goes to stderr, so the job log says why each package runs.
"$bin" -base HEAD^1 -head HEAD -within "$within" -exclude "$exclude" -explain | tr '\n' ' '
