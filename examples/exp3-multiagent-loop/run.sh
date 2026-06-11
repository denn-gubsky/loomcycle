#!/usr/bin/env bash
# Launch exp3 (multi-agent refine/evaluate loop) self-contained from this folder.
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
[ -f "$HERE/.env.local" ] || { cp "$HERE/.env.local.example" "$HERE/.env.local"; \
  echo "run.sh: created .env.local from template — fill in secrets, or rely on the deepseek fallback."; }
set -a; source "$HERE/.env.local"; set +a

export LOOMCYCLE_DATA_DIR="${LOOMCYCLE_DATA_DIR:-$HERE/data}"
export LOOMCYCLE_LISTEN_ADDR="${LOOMCYCLE_LISTEN_ADDR:-127.0.0.1:8787}"
export LOOMCYCLE_ANTHROPIC_OAUTH_DEV_ENABLED="${LOOMCYCLE_ANTHROPIC_OAUTH_DEV_ENABLED:-1}"
# exp3: the agents subscribe with wait_ms up to 600s; raise the long-poll cap above
# the default 30s so a parked subscriber doesn't re-subscribe every 30s (burning
# max_iterations) before a message arrives.
export LOOMCYCLE_CHANNELS_LONGPOLL_CAP_MS="${LOOMCYCLE_CHANNELS_LONGPOLL_CAP_MS:-180000}"
# (no file/shell tools — agents use Context/Memory/Channel/Evaluation only.)

mkdir -p "$HERE/data" "$HERE/work"
LOOMCYCLE_BIN="${LOOMCYCLE_BIN:-loomcycle}"
command -v "$LOOMCYCLE_BIN" >/dev/null 2>&1 || { echo "run.sh: '$LOOMCYCLE_BIN' not on PATH — install loomcycle or set LOOMCYCLE_BIN." >&2; exit 127; }
cd "$HERE/work"
exec "$LOOMCYCLE_BIN" "$@" --config "$HERE/loomcycle.yaml"
