#!/usr/bin/env bash
# Launch exp6.8 (self-evolving agents on LOCAL models) self-contained from this folder. The ONLY
# local model is gemma4:max (the evolving solver population); the meta-agents (breeder + advisor) run
# on cloud sonnet. Needs: loomcycle >= v0.37.0, an ollama host with your solver model, and a sonnet
# provider (OAuth or an Anthropic API key — see README). Drive from a second terminal:
#   ./work/exp6_run.sh evolve
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
[ -f "$HERE/.env.local" ] || { cp "$HERE/.env.local.example" "$HERE/.env.local"; \
  echo "run.sh: created .env.local — set OLLAMA_BASE_URL + a sonnet provider, then re-run."; }
set -a; source "$HERE/.env.local"; set +a

export LOOMCYCLE_DATA_DIR="${LOOMCYCLE_DATA_DIR:-$HERE/data}"
export LOOMCYCLE_LISTEN_ADDR="${LOOMCYCLE_LISTEN_ADDR:-127.0.0.1:8787}"
export LOOMCYCLE_ANTHROPIC_OAUTH_DEV_ENABLED="${LOOMCYCLE_ANTHROPIC_OAUTH_DEV_ENABLED:-1}"

# ── Local-model knobs (the gemma4:max solver population dials this ollama host) ──
export OLLAMA_BASE_URL="${OLLAMA_BASE_URL:-http://localhost:11434}"
# Global context window for ollama-local (no per-model override exists). 16K is ample for the
# short (<=120-word) solver answers and keeps prefill cheap on a slow box.
export LOOMCYCLE_OLLAMA_LOCAL_NUM_CTX="${LOOMCYCLE_OLLAMA_LOCAL_NUM_CTX:-16384}"
# Generous local time-to-first-byte / inter-token timeouts (cold model load + slow generation).
export LOOMCYCLE_OLLAMA_LOCAL_HEADER_TIMEOUT_MS="${LOOMCYCLE_OLLAMA_LOCAL_HEADER_TIMEOUT_MS:-300000}"
export LOOMCYCLE_OLLAMA_LOCAL_IDLE_TIMEOUT_MS="${LOOMCYCLE_OLLAMA_LOCAL_IDLE_TIMEOUT_MS:-300000}"

mkdir -p "$HERE/data"
LOOMCYCLE_BIN="${LOOMCYCLE_BIN:-loomcycle}"
command -v "$LOOMCYCLE_BIN" >/dev/null 2>&1 || { echo "run.sh: '$LOOMCYCLE_BIN' not on PATH — install loomcycle >= v0.37.0 or set LOOMCYCLE_BIN." >&2; exit 127; }
exec "$LOOMCYCLE_BIN" "$@" --config "$HERE/loomcycle.yaml"
