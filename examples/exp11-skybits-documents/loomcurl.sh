#!/usr/bin/env bash
# Token-safe curl to the local loomcycle REST API. Sources .env.local and passes
# the bearer via a STDIN config (-K -) so LOOMCYCLE_AUTH_TOKEN never appears in argv.
# If LOOMCYCLE_AUTH_TOKEN is empty (dev open mode), the header is simply omitted.
#   ./loomcurl.sh -X POST http://127.0.0.1:8787/v1/runs -H 'Content-Type: application/json' -d @body.json
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
set -a; [ -f "$HERE/.env.local" ] && source "$HERE/.env.local"; set +a
if [ -n "${LOOMCYCLE_AUTH_TOKEN:-}" ]; then
  printf 'header = "Authorization: Bearer %s"\n' "$LOOMCYCLE_AUTH_TOKEN" | curl -sS -K - "$@"
else
  curl -sS "$@"
fi
