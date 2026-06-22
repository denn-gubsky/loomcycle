#!/usr/bin/env bash
# Launch exp9 (prime numbers via stdio → SQL memory → channel → validation).
# Requires loomcycle ≥ v1.1.2 (RFC AA SQL memory). Build from source if needed:
#   cd /Users/denn/work/loomcycle && go build -o /tmp/loomcycle-dev ./cmd/loomcycle/
#   LOOMCYCLE_BIN=/tmp/loomcycle-dev ./run.sh serve
#
# After the server starts, drive from a second terminal (start validator FIRST):
#   python3 work/exp9_run.py
# Or manually:
#   POST /v1/runs  {"agent":"exp9-validator","user_id":"exp9","segments":[...]}  # start first
#   POST /v1/runs  {"agent":"exp9-coder","user_id":"exp9","segments":[...]}      # then coder
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
[ -f "$HERE/.env.local" ] || { cp "$HERE/.env.local.example" "$HERE/.env.local"; \
  echo "run.sh: created .env.local — add a provider (loomcycle anthropic login, or DEEPSEEK_API_KEY)."; }
set -a; source "$HERE/.env.local"; set +a

export LOOMCYCLE_DATA_DIR="${LOOMCYCLE_DATA_DIR:-$HERE/data}"
export LOOMCYCLE_LISTEN_ADDR="${LOOMCYCLE_LISTEN_ADDR:-127.0.0.1:8787}"
export LOOMCYCLE_ANTHROPIC_OAUTH_DEV_ENABLED="${LOOMCYCLE_ANTHROPIC_OAUTH_DEV_ENABLED:-1}"
# Bash is required by both agents (coder: python3 stdio; validator: inline trial-division)
export LOOMCYCLE_BASH_ENABLED="${LOOMCYCLE_BASH_ENABLED:-1}"
# RFC AA — SQL memory subsystem. The YAML also sets storage.sqlmem_enabled: true;
# this env var provides a belt-and-suspenders enable for CI / one-liner runs.
export LOOMCYCLE_SQLMEM_ENABLED="${LOOMCYCLE_SQLMEM_ENABLED:-1}"

mkdir -p "$HERE/data" "$HERE/work"
LOOMCYCLE_BIN="${LOOMCYCLE_BIN:-loomcycle}"
command -v "$LOOMCYCLE_BIN" >/dev/null 2>&1 || \
  { echo "run.sh: '$LOOMCYCLE_BIN' not found. Build from source:" >&2
    echo "  cd /Users/denn/work/loomcycle && go build -o /tmp/loomcycle-dev ./cmd/loomcycle/" >&2
    echo "  LOOMCYCLE_BIN=/tmp/loomcycle-dev ./run.sh serve" >&2
    exit 127; }
# cd into the volume root so loomcycle.yaml `default: {path: .}` resolves to ./work.
# primes.py is at ../primes.py from the coder's working directory (../primes.py = $HERE/primes.py).
cd "$HERE/work"
exec "$LOOMCYCLE_BIN" "$@" --config "$HERE/loomcycle.yaml"
