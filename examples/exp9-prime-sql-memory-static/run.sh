#!/usr/bin/env bash
# Launch exp9-static (prime pipeline via code-js static agents — no LLM calls).
# Requires loomcycle >= v1.1.2.  Build from source if needed:
#   cd /Users/denn/work/loomcycle && go build -o /tmp/loomcycle-dev ./cmd/loomcycle/
#   LOOMCYCLE_BIN=/tmp/loomcycle-dev ./run.sh
#
# After the server starts, trigger from a second terminal:
#   python3 work/exp9_static_run.py
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

export LOOMCYCLE_DATA_DIR="${LOOMCYCLE_DATA_DIR:-$HERE/data}"
export LOOMCYCLE_LISTEN_ADDR="${LOOMCYCLE_LISTEN_ADDR:-127.0.0.1:8787}"
# The coder runs primes.py in a workspace on the local worker (workspace.mode:
# local in loomcycle.yaml): python3 must be on PATH.

mkdir -p "$HERE/data" "$HERE/work"
LOOMCYCLE_BIN="${LOOMCYCLE_BIN:-loomcycle}"
command -v "$LOOMCYCLE_BIN" >/dev/null 2>&1 || {
  echo "run.sh: '$LOOMCYCLE_BIN' not found. Build from source:" >&2
  echo "  cd /Users/denn/work/loomcycle && go build -o /tmp/loomcycle-dev ./cmd/loomcycle/" >&2
  echo "  LOOMCYCLE_BIN=/tmp/loomcycle-dev ./run.sh" >&2
  exit 127
}
# cd into the volume root so the default volume path=. resolves to ./work.
# primes.py is in ./work, the default volume; the coder copies it into a workspace.
cd "$HERE/work"
exec "$LOOMCYCLE_BIN" --config "$HERE/loomcycle.yaml"
