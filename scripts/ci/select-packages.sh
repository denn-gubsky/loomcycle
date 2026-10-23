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
# Both cases cover every in-tree Go module (scripts/ci/modules.sh): the
# patterns are matched as go list matches them, but over the packages of all
# of them, so "./..." also names a nested module's packages, which go list
# from the root does not. Run the result with scripts/ci/in-modules.sh, which
# tests each package from its own module.
#
# Empty output means the step has nothing to run.
set -euo pipefail

within="${1:?usage: select-packages.sh WITHIN [EXCLUDE]}"
exclude="${2:-}"

# Built every time rather than reused if present: the build cache makes it a
# link, and a binary left from an older checkout would select by old rules.
bin="${RUNNER_TEMP:-${TMPDIR:-/tmp}}/loomcycle-affected"
go build -o "$bin" ./scripts/ci/affected

if [ "${GITHUB_EVENT_NAME:-}" != "pull_request" ]; then
  "$bin" -all -within "$within" -exclude "$exclude" | tr '\n' ' '
  exit 0
fi

# -explain goes to stderr, so the job log says why each package runs.
"$bin" -base HEAD^1 -head HEAD -within "$within" -exclude "$exclude" -explain | tr '\n' ' '
