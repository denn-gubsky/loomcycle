#!/usr/bin/env bash
# Token-safe stdio wrapper loomcycle spawns as the `gitea` MCP server. Maps the
# inherited LOOMCYCLE_GITEA_* env into the GITEA_* names gitea-mcp expects; the
# secret flows env→env→env and is never read, printed, or placed in any argv.
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"     # .../work/bin
EX="$(cd "$HERE/../.." && pwd)"                          # example root
if [[ -z "${LOOMCYCLE_GITEA_TOKEN:-}" && -f "$EX/.env.local" ]]; then set -a; source "$EX/.env.local"; set +a; fi
exec env \
  GITEA_ACCESS_TOKEN="${LOOMCYCLE_GITEA_TOKEN:-}" \
  GITEA_HOST="${LOOMCYCLE_GITEA_HOST:-}" \
  GITEA_INSECURE="${LOOMCYCLE_GITEA_INSECURE:-false}" \
  "$HERE/gitea-mcp" -t stdio
