#!/usr/bin/env bash
# Launch exp8 (ephemeral volume fan-out) self-contained from this folder. Needs loomcycle ≥ v1.0.0.
# Drive it from a second terminal after the server starts:
#   POST /v1/runs agent=exp8-dispatcher  (see README for a Python SSE client example)
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
[ -f "$HERE/.env.local" ] || { cp "$HERE/.env.local.example" "$HERE/.env.local"; \
  echo "run.sh: created .env.local — add a provider (loomcycle anthropic login, or DEEPSEEK_API_KEY)."; }
set -a; source "$HERE/.env.local"; set +a

export LOOMCYCLE_DATA_DIR="${LOOMCYCLE_DATA_DIR:-$HERE/data}"
export LOOMCYCLE_LISTEN_ADDR="${LOOMCYCLE_LISTEN_ADDR:-127.0.0.1:8787}"
export LOOMCYCLE_ANTHROPIC_OAUTH_DEV_ENABLED="${LOOMCYCLE_ANTHROPIC_OAUTH_DEV_ENABLED:-1}"
# Bash is required by the dispatcher (git clone + ls verify). Reviewers get Read/Grep/Glob only.
export LOOMCYCLE_BASH_ENABLED="${LOOMCYCLE_BASH_ENABLED:-1}"

# dynamic-root backing dir must exist before the server loads the volume config.
mkdir -p "$HERE/data" "$HERE/work" "$HERE/work/dynamic"
LOOMCYCLE_BIN="${LOOMCYCLE_BIN:-loomcycle}"
command -v "$LOOMCYCLE_BIN" >/dev/null 2>&1 || \
  { echo "run.sh: '$LOOMCYCLE_BIN' not on PATH — install loomcycle (≥ v1.0.0) or set LOOMCYCLE_BIN." >&2; exit 127; }
# cd into the volume root so loomcycle.yaml `default: {path: .}` resolves to ./work.
cd "$HERE/work"
exec "$LOOMCYCLE_BIN" "$@" --config "$HERE/loomcycle.yaml"
