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
# Both agents run Python in a workspace on the local worker (workspace.mode:
# local in loomcycle.yaml): python3 must be on PATH.
mkdir -p "$HERE/data" "$HERE/work"
LOOMCYCLE_BIN="${LOOMCYCLE_BIN:-loomcycle}"
command -v "$LOOMCYCLE_BIN" >/dev/null 2>&1 || \
  { echo "run.sh: '$LOOMCYCLE_BIN' not found. Build from source:" >&2
    echo "  cd /Users/denn/work/loomcycle && go build -o /tmp/loomcycle-dev ./cmd/loomcycle/" >&2
    echo "  LOOMCYCLE_BIN=/tmp/loomcycle-dev ./run.sh serve" >&2
    exit 127; }
# cd into the volume root so loomcycle.yaml `default: {path: .}` resolves to ./work.
# primes.py is in ./work, the default volume; the coder copies it into a workspace.
cd "$HERE/work"
exec "$LOOMCYCLE_BIN" "$@" --config "$HERE/loomcycle.yaml"
