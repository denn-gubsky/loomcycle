#!/usr/bin/env bash
# Launch exp11 (Skybits documents via remote MCP + cross-run Memory).
# Requires a model provider (Anthropic OAuth login, or DEEPSEEK_API_KEY) and a
# Skybits connector API key (LOOMCYCLE_SKYBITS_TOKEN) — see README.md.
#
# After the server starts, drive from a second terminal:
#   ./loomcurl.sh -X POST http://127.0.0.1:8787/v1/runs -H 'Content-Type: application/json' -d @body.json
# (exact run-1 / run-2 bodies in README.md)
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
[ -f "$HERE/.env.local" ] || { cp "$HERE/.env.local.example" "$HERE/.env.local"; \
  echo "run.sh: created .env.local — add a provider (loomcycle anthropic login, or DEEPSEEK_API_KEY) + LOOMCYCLE_SKYBITS_TOKEN (see README)."; }
set -a; source "$HERE/.env.local"; set +a

export LOOMCYCLE_DATA_DIR="${LOOMCYCLE_DATA_DIR:-$HERE/data}"
export LOOMCYCLE_LISTEN_ADDR="${LOOMCYCLE_LISTEN_ADDR:-127.0.0.1:8787}"
export LOOMCYCLE_ANTHROPIC_OAUTH_DEV_ENABLED="${LOOMCYCLE_ANTHROPIC_OAUTH_DEV_ENABLED:-1}"

mkdir -p "$HERE/data"
LOOMCYCLE_BIN="${LOOMCYCLE_BIN:-loomcycle}"
command -v "$LOOMCYCLE_BIN" >/dev/null 2>&1 || \
  { echo "run.sh: '$LOOMCYCLE_BIN' not found. Build from source:" >&2
    echo "  go build -o /tmp/loomcycle-dev ./cmd/loomcycle/" >&2
    echo "  LOOMCYCLE_BIN=/tmp/loomcycle-dev ./run.sh serve" >&2
    exit 127; }
# No Bash/fs tools in this example — no tool-sandbox volume, so no ./work cd.
exec "$LOOMCYCLE_BIN" "$@" --config "$HERE/loomcycle.yaml"
