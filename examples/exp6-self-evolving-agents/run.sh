#!/usr/bin/env bash
# Launch exp6 (self-evolving agents) self-contained from this folder. Needs only loomcycle
# + a provider — no external services. Drive the evolution from a second terminal:
#   ./work/exp6_run.sh evolve
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
[ -f "$HERE/.env.local" ] || { cp "$HERE/.env.local.example" "$HERE/.env.local"; \
  echo "run.sh: created .env.local — add a provider (loomcycle anthropic login, or DEEPSEEK_API_KEY)."; }
set -a; source "$HERE/.env.local"; set +a

export LOOMCYCLE_DATA_DIR="${LOOMCYCLE_DATA_DIR:-$HERE/data}"
export LOOMCYCLE_LISTEN_ADDR="${LOOMCYCLE_LISTEN_ADDR:-127.0.0.1:8787}"
export LOOMCYCLE_ANTHROPIC_OAUTH_DEV_ENABLED="${LOOMCYCLE_ANTHROPIC_OAUTH_DEV_ENABLED:-1}"

# The breeder/solvers/advisor only use built-in tools (AgentDef, Agent, Evaluation, Memory,
# Context) — no Bash, no HTTP egress, no MCP, no scheduler, no webhooks. Nothing else to enable.

mkdir -p "$HERE/data"
LOOMCYCLE_BIN="${LOOMCYCLE_BIN:-loomcycle}"
command -v "$LOOMCYCLE_BIN" >/dev/null 2>&1 || { echo "run.sh: '$LOOMCYCLE_BIN' not on PATH — install loomcycle or set LOOMCYCLE_BIN." >&2; exit 127; }
exec "$LOOMCYCLE_BIN" "$@" --config "$HERE/loomcycle.yaml"
