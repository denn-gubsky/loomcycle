#!/usr/bin/env bash
# loomcycle-mcp.sh — launch loomcycle in stdio MCP mode with .env.local sourced.
#
# Usage (typically invoked by an MCP client, not by hand):
#   ./loomcycle-mcp.sh                 # uses default config
#   LOOMCYCLE_CONFIG=/path/to.yaml ./loomcycle-mcp.sh
#
# Why this exists: the bare `bin/loomcycle mcp ...` inherits whatever env
# the MCP client (e.g. Claude Code) hands it — typically empty of the
# LOOMCYCLE_* / BRAVE_API_KEY / LOOMCYCLE_JOBS_SEARCH_API_TOKEN vars that
# loomcycle.yaml's `${...}` placeholders expect. With those unset, the
# aggregator's upstream pool handshakes fail (brave-search exits, jobs
# redirects to /login) and the stdio server never converges to ready.
# This wrapper sources .env.local first so every spawn gets the full env.
#
# Compare with loomcycle.sh, which does the same env-sourcing but runs the
# HTTP+SSE server and rebuilds. MCP mode is per-invocation stdio — no
# rebuild and no port-stop logic; let signals (EOF on stdin) end the run.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR"

CONFIG="${LOOMCYCLE_CONFIG:-$HOME/.config/loomcycle/loomcycle.yaml}"
ENV_FILE="${LOOMCYCLE_ENV_FILE:-.env.local}"
BIN="${LOOMCYCLE_BIN:-bin/loomcycle}"

if [[ ! -x "$BIN" ]]; then
  echo "loomcycle-mcp.sh: binary not found or not executable: $BIN" >&2
  echo "loomcycle-mcp.sh: run ./loomcycle.sh --no-build=0 first, or set LOOMCYCLE_BIN" >&2
  exit 127
fi

if [[ -f "$ENV_FILE" ]]; then
  set -a
  # shellcheck disable=SC1090
  source "$ENV_FILE"
  set +a
else
  echo "loomcycle-mcp.sh: $ENV_FILE missing — upstream MCP handshakes will likely fail" >&2
fi

exec "$BIN" mcp --config "$CONFIG"
