#!/usr/bin/env bash
# Runs a command in each in-tree Go module's directory (scripts/ci/modules.sh).
#
#   scripts/ci/in-modules.sh CMD [ARGS...]            # in every module
#   scripts/ci/in-modules.sh CMD [ARGS...] -- PKG...  # with each module's own packages
#
# The first form is for whole-module commands: `go vet ./...`,
# `go mod tidy`. The second takes import paths (what select-packages.sh
# prints) and, in each module that owns any of them, runs CMD ARGS followed by
# that module's paths: a nested module's packages build against its own go.mod
# and go.sum only from its own directory. An empty PKG list runs nothing.
#
# Every module runs even when one fails; the exit status is non-zero if any
# did. With a single module (the root), the second form is exactly
# `CMD ARGS PKG...` run from the root.
set -euo pipefail

cmd=()
while [ $# -gt 0 ] && [ "$1" != "--" ]; do
  cmd+=("$1")
  shift
done
if [ ${#cmd[@]} -eq 0 ]; then
  echo "usage: in-modules.sh CMD [ARGS...] [-- PKG...]" >&2
  exit 2
fi

here="$(cd "$(dirname "$0")" && pwd)"
root="$(cd "$here/../.." && pwd)"

if [ $# -gt 0 ]; then
  shift # the --
  [ $# -gt 0 ] || exit 0
  groups=$("$here/modules.sh" -group "$@")
else
  groups=$("$here/modules.sh")
fi

status=0
while read -r dir pkgs; do
  [ -n "$dir" ] || continue
  echo "==> $dir: ${cmd[*]}" >&2
  read -ra args <<<"$pkgs"
  (cd "$root/$dir" && "${cmd[@]}" ${args[@]+"${args[@]}"} </dev/null) || status=1
done <<<"$groups"
exit "$status"
