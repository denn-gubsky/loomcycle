#!/usr/bin/env bash
# Launch exp1 (tools usage) fully self-contained from this folder.
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# Bootstrap .env.local from the committed template on first run.
[ -f "$HERE/.env.local" ] || { cp "$HERE/.env.local.example" "$HERE/.env.local"; \
  echo "run.sh: created .env.local from template — fill in secrets, or rely on the deepseek fallback."; }
set -a; source "$HERE/.env.local"; set +a

export LOOMCYCLE_DATA_DIR="${LOOMCYCLE_DATA_DIR:-$HERE/data}"
export LOOMCYCLE_LISTEN_ADDR="${LOOMCYCLE_LISTEN_ADDR:-127.0.0.1:8787}"
# Anthropic-OAuth primary (research/dev). If you have NOT run `loomcycle anthropic
# login`, leave this unset (or the provider is excluded) and runs use deepseek-v4-pro.
export LOOMCYCLE_ANTHROPIC_OAUTH_DEV_ENABLED="${LOOMCYCLE_ANTHROPIC_OAUTH_DEV_ENABLED:-1}"
# exp1 tool sandbox: Read/Write/Edit/Bash scoped to ./work via the `default`
# volume in loomcycle.yaml (run.sh cd's to ./work, so `path: .` == ./work).
export LOOMCYCLE_BASH_ENABLED="${LOOMCYCLE_BASH_ENABLED:-1}"

mkdir -p "$HERE/data" "$HERE/work"
LOOMCYCLE_BIN="${LOOMCYCLE_BIN:-loomcycle}"
command -v "$LOOMCYCLE_BIN" >/dev/null 2>&1 || { echo "run.sh: '$LOOMCYCLE_BIN' not on PATH — install loomcycle (brew install denn-gubsky/loomcycle/loomcycle) or set LOOMCYCLE_BIN." >&2; exit 127; }
cd "$HERE/work"
exec "$LOOMCYCLE_BIN" "$@" --config "$HERE/loomcycle.yaml"
