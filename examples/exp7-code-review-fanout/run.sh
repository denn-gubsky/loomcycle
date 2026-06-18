#!/usr/bin/env bash
# Launch exp7 (code-review fan-out) self-contained from this folder. Needs loomcycle ≥ v0.32.0
# (for the `spawn_runs` MCP fan-out) + a provider. Drive it from a second terminal:
#   ./work/exp7_run.sh delegate     (clone the review target first — see README step 1)
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
[ -f "$HERE/.env.local" ] || { cp "$HERE/.env.local.example" "$HERE/.env.local"; \
  echo "run.sh: created .env.local — add a provider (loomcycle anthropic login, or DEEPSEEK_API_KEY)."; }
set -a; source "$HERE/.env.local"; set +a

export LOOMCYCLE_DATA_DIR="${LOOMCYCLE_DATA_DIR:-$HERE/data}"
export LOOMCYCLE_LISTEN_ADDR="${LOOMCYCLE_LISTEN_ADDR:-127.0.0.1:8787}"
export LOOMCYCLE_ANTHROPIC_OAUTH_DEV_ENABLED="${LOOMCYCLE_ANTHROPIC_OAUTH_DEV_ENABLED:-1}"

# The read-only review jail + the imported skill. The reviewers get Read/Grep/Glob sandboxed to
# the read-only `default` volume in loomcycle.yaml (run.sh cd's to ./work below, so `path: .` ==
# ./work); the repo under review is cloned to ./work/loomcycle-src and addressed by reviewers
# RELATIVE to that volume root (loomcycle-src/<path>). No Bash/Write/egress.
export LOOMCYCLE_SKILLS_ROOT="${LOOMCYCLE_SKILLS_ROOT:-$HERE/skills}"   # bundle ./skills/code-review/SKILL.md
# Bearer the MCP thin client (work/exp7_mcp.py → `loomcycle mcp --upstream`) uses to reach this runtime.
export LOOMCYCLE_MCP_UPSTREAM_TOKEN="${LOOMCYCLE_MCP_UPSTREAM_TOKEN:-${LOOMCYCLE_AUTH_TOKEN:-}}"

mkdir -p "$HERE/data" "$HERE/work"
LOOMCYCLE_BIN="${LOOMCYCLE_BIN:-loomcycle}"
command -v "$LOOMCYCLE_BIN" >/dev/null 2>&1 || { echo "run.sh: '$LOOMCYCLE_BIN' not on PATH — install loomcycle (≥ v0.32.0) or set LOOMCYCLE_BIN." >&2; exit 127; }
# cd into the read-only volume root so loomcycle.yaml's `default: {path: .}` resolves to ./work.
cd "$HERE/work"
exec "$LOOMCYCLE_BIN" "$@" --config "$HERE/loomcycle.yaml"
