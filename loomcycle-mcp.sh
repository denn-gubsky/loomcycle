#!/usr/bin/env bash
# loomcycle-mcp.sh — launch loomcycle in stdio MCP mode with the env files sourced.
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
# This wrapper sources .env.insecure + .env.local first so every spawn gets
# the full env (config NAMES and secret VALUES alike).
#
# Compare with loomcycle.sh, which does the same env-sourcing but runs the
# HTTP+SSE server and rebuilds. MCP mode is per-invocation stdio — no
# rebuild and no port-stop logic; let signals (EOF on stdin) end the run.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR"

CONFIG="${LOOMCYCLE_CONFIG:-$HOME/.config/loomcycle/loomcycle.yaml}"
# Two-file convention (docs/CONFIGURATION.md §9c): config first, secrets
# last. LOOMCYCLE_ENV_FILE collapses the pair to one explicit file.
if [[ -n "${LOOMCYCLE_ENV_FILE:-}" ]]; then
  ENV_FILES=("$LOOMCYCLE_ENV_FILE")
else
  ENV_FILES=(.env.insecure .env.local)
fi
BIN="${LOOMCYCLE_BIN:-bin/loomcycle}"

if [[ ! -x "$BIN" ]]; then
  echo "loomcycle-mcp.sh: binary not found or not executable: $BIN" >&2
  echo "loomcycle-mcp.sh: run ./loomcycle.sh --no-build=0 first, or set LOOMCYCLE_BIN" >&2
  exit 127
fi

_mcp_sourced_any=0
for _ef in "${ENV_FILES[@]}"; do
  if [[ -f "$_ef" ]]; then
    set -a
    # shellcheck disable=SC1090
    source "$_ef"
    set +a
    _mcp_sourced_any=1
  fi
done
if [[ $_mcp_sourced_any -eq 0 ]]; then
  echo "loomcycle-mcp.sh: no env file found (${ENV_FILES[*]}) — upstream MCP handshakes will likely fail" >&2
fi
unset _ef _mcp_sourced_any

exec "$BIN" mcp --config "$CONFIG"
