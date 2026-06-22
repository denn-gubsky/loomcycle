#!/usr/bin/env bash
# Launch exp10-gbash-bench (RFC AJ GBash comparative benchmark).
# Requires loomcycle >= v1.1.2 with RFC AJ (Bashbox) compiled in.
# Build:  cd /Users/denn/work/loomcycle && go build -o /tmp/loomcycle-dev ./cmd/loomcycle/
# Start:  LOOMCYCLE_BIN=/tmp/loomcycle-dev ./run.sh
# Bench:  python3 work/exp10_run.py  (in a second terminal)
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

export LOOMCYCLE_DATA_DIR="${LOOMCYCLE_DATA_DIR:-$HERE/data}"
export LOOMCYCLE_LISTEN_ADDR="${LOOMCYCLE_LISTEN_ADDR:-127.0.0.1:8787}"
# System Bash tool — needed by exp10-bash-agent
export LOOMCYCLE_BASH_ENABLED="${LOOMCYCLE_BASH_ENABLED:-1}"
# Bashbox (RFC AJ) — needed by exp10-bashbox-agent
export LOOMCYCLE_BASHBOX_ENABLED="${LOOMCYCLE_BASHBOX_ENABLED:-1}"
# Allow git to escape the Bashbox sandbox (RFC AJ §13 fallback)
# git — for cloning the repo in the Bashbox agent
# python3 — for ms-level wall-clock timing (Date.now() measures JS overhead only)
export LOOMCYCLE_BASHBOX_FALLBACK_COMMANDS="${LOOMCYCLE_BASHBOX_FALLBACK_COMMANDS:-git,python3}"
# code-js provider — required for provider: code-js agents
export LOOMCYCLE_CODE_AGENTS_ENABLED="${LOOMCYCLE_CODE_AGENTS_ENABLED:-1}"

mkdir -p "$HERE/data" "$HERE/dynamic" "$HERE/work" "$HERE/work/reports"
LOOMCYCLE_BIN="${LOOMCYCLE_BIN:-loomcycle}"
command -v "$LOOMCYCLE_BIN" >/dev/null 2>&1 || {
  echo "run.sh: '$LOOMCYCLE_BIN' not found. Build with:" >&2
  echo "  cd /Users/denn/work/loomcycle && go build -o /tmp/loomcycle-dev ./cmd/loomcycle/" >&2
  echo "  LOOMCYCLE_BIN=/tmp/loomcycle-dev ./run.sh" >&2
  exit 127
}
# cd into work/ — the default volume path=. resolves here.
# dynamic-root path=../dynamic resolves to exp10-gbash-bench/dynamic/.
cd "$HERE/work"
exec "$LOOMCYCLE_BIN" --config "$HERE/loomcycle.yaml"
