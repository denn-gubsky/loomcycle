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
# SQL memory subsystem (RFC AA)
export LOOMCYCLE_SQLMEM_ENABLED="${LOOMCYCLE_SQLMEM_ENABLED:-1}"
# Bash tool — needed by coder to run primes.py
export LOOMCYCLE_BASH_ENABLED="${LOOMCYCLE_BASH_ENABLED:-1}"
# code-js provider — required for static agents; inline `code:` in YAML also uses this path
# Note: code-js agents in YAML config work without this flag; it only gates the
# AgentDef tool's ability to register NEW dynamic code agents at runtime.
export LOOMCYCLE_CODE_AGENTS_ENABLED="${LOOMCYCLE_CODE_AGENTS_ENABLED:-1}"

mkdir -p "$HERE/data" "$HERE/work"
LOOMCYCLE_BIN="${LOOMCYCLE_BIN:-loomcycle}"
command -v "$LOOMCYCLE_BIN" >/dev/null 2>&1 || {
  echo "run.sh: '$LOOMCYCLE_BIN' not found. Build from source:" >&2
  echo "  cd /Users/denn/work/loomcycle && go build -o /tmp/loomcycle-dev ./cmd/loomcycle/" >&2
  echo "  LOOMCYCLE_BIN=/tmp/loomcycle-dev ./run.sh" >&2
  exit 127
}
# cd into the volume root so the default volume path=. resolves to ./work.
# primes.py is one level up (../primes.py from the coder's cwd).
cd "$HERE/work"
exec "$LOOMCYCLE_BIN" --config "$HERE/loomcycle.yaml"
