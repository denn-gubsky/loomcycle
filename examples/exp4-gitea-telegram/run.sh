#!/usr/bin/env bash
# Launch exp4 (Gitea webhook → reviewer-merge → Telegram) self-contained from this folder.
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
[ -f "$HERE/.env.local" ] || { cp "$HERE/.env.local.example" "$HERE/.env.local"; \
  echo "run.sh: created .env.local — fill in Gitea/Telegram secrets + hosts (see README)."; }
set -a; source "$HERE/.env.local"; set +a

export LOOMCYCLE_DATA_DIR="${LOOMCYCLE_DATA_DIR:-$HERE/data}"
# For the webhook leg, Gitea must REACH this server. 127.0.0.1 only works if Gitea
# runs on this host; otherwise bind a reachable IP, e.g. LOOMCYCLE_LISTEN_ADDR=0.0.0.0:8788
# (or a tailnet/LAN IP) and point the Gitea webhook URL at it. See README.
export LOOMCYCLE_LISTEN_ADDR="${LOOMCYCLE_LISTEN_ADDR:-127.0.0.1:8787}"
export LOOMCYCLE_ANTHROPIC_OAUTH_DEV_ENABLED="${LOOMCYCLE_ANTHROPIC_OAUTH_DEV_ENABLED:-1}"
# tool sandbox: the coder/reviewer use Bash+git under ./work via the `default`
# volume in loomcycle.yaml (run.sh cd's to ./work, so `path: .` == ./work).
export LOOMCYCLE_BASH_ENABLED="${LOOMCYCLE_BASH_ENABLED:-1}"
export LOOMCYCLE_CHANNELS_LONGPOLL_CAP_MS="${LOOMCYCLE_CHANNELS_LONGPOLL_CAP_MS:-180000}"
# inbound webhooks + the HMAC secret allowlist (the receiver resolves these env NAMES)
export LOOMCYCLE_WEBHOOKS_ENABLED="${LOOMCYCLE_WEBHOOKS_ENABLED:-1}"
export LOOMCYCLE_WEBHOOKS_ENV_ALLOWLIST="${LOOMCYCLE_WEBHOOKS_ENV_ALLOWLIST:-LOOMCYCLE_GITEA_WEBHOOK_SECRET}"
export LOOMCYCLE_SCHEDULER_ENV_ALLOWLIST="${LOOMCYCLE_SCHEDULER_ENV_ALLOWLIST:-LOOMCYCLE_GITEA_WEBHOOK_SECRET}"

mkdir -p "$HERE/data" "$HERE/work/bin"
LOOMCYCLE_BIN="${LOOMCYCLE_BIN:-loomcycle}"
command -v "$LOOMCYCLE_BIN" >/dev/null 2>&1 || { echo "run.sh: '$LOOMCYCLE_BIN' not on PATH — install loomcycle or set LOOMCYCLE_BIN." >&2; exit 127; }
[ -x "$HERE/work/bin/gitea-mcp" ] || echo "run.sh: NOTE work/bin/gitea-mcp is missing — download it (see README) or the gitea MCP server won't start." >&2
cd "$HERE/work"
exec "$LOOMCYCLE_BIN" "$@" --config "$HERE/loomcycle.yaml"
