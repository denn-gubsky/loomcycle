#!/usr/bin/env bash
# Launch exp5 (scheduler fan-out/fan-in news-digest → Telegram) self-contained from this folder.
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
[ -f "$HERE/.env.local" ] || { cp "$HERE/.env.local.example" "$HERE/.env.local"; \
  echo "run.sh: created .env.local — fill in the Telegram bot token + chat id (see README)."; }
set -a; source "$HERE/.env.local"; set +a

export LOOMCYCLE_DATA_DIR="${LOOMCYCLE_DATA_DIR:-$HERE/data}"
export LOOMCYCLE_LISTEN_ADDR="${LOOMCYCLE_LISTEN_ADDR:-127.0.0.1:8787}"
export LOOMCYCLE_ANTHROPIC_OAUTH_DEV_ENABLED="${LOOMCYCLE_ANTHROPIC_OAUTH_DEV_ENABLED:-1}"

# The scheduler must be ON — this experiment is entirely scheduler-driven.
export LOOMCYCLE_SCHEDULER_ENABLED="${LOOMCYCLE_SCHEDULER_ENABLED:-1}"

# HTTP host allowlist: scheduler-fired runs get NO per-run allowlist, so this env var is
# the SOLE HTTP egress policy (empty = refuse all). It MUST list every feed host the
# collectors GET, or every fetch is refused. (Both bare + www forms, to be safe.)
export LOOMCYCLE_HTTP_HOST_ALLOWLIST="${LOOMCYCLE_HTTP_HOST_ALLOWLIST:-news.ycombinator.com,www.wired.com,wired.com,www.engadget.com,engadget.com,feeds.arstechnica.com,arstechnica.com,techcrunch.com,www.techcrunch.com}"

# The consolidator's Channel.await uses wait_ms up to 120000; the long-poll cap must be >= that.
export LOOMCYCLE_CHANNELS_LONGPOLL_CAP_MS="${LOOMCYCLE_CHANNELS_LONGPOLL_CAP_MS:-180000}"

mkdir -p "$HERE/data" "$HERE/work/bin"
LOOMCYCLE_BIN="${LOOMCYCLE_BIN:-loomcycle}"
command -v "$LOOMCYCLE_BIN" >/dev/null 2>&1 || { echo "run.sh: '$LOOMCYCLE_BIN' not on PATH — install loomcycle or set LOOMCYCLE_BIN." >&2; exit 127; }
command -v python3 >/dev/null 2>&1 || echo "run.sh: NOTE python3 not found — the telegram MCP server won't start (the rest still runs)." >&2
cd "$HERE/work"
exec "$LOOMCYCLE_BIN" "$@" --config "$HERE/loomcycle.yaml"
