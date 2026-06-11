#!/usr/bin/env bash
# Token-safe Gitea API curl. Token via STDIN config (never argv). -k tolerates a
# self-signed/expired cert when LOOMCYCLE_GITEA_INSECURE=true.
#   ./giteapi.sh https://gitea.example.com/api/v1/user
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
set -a; [ -f "$HERE/.env.local" ] && source "$HERE/.env.local"; set +a
[ -n "${LOOMCYCLE_GITEA_TOKEN:-}" ] || { echo "giteapi: LOOMCYCLE_GITEA_TOKEN not set (.env.local)" >&2; exit 2; }
K=""; [ "${LOOMCYCLE_GITEA_INSECURE:-false}" = "true" ] && K="-k"
printf 'header = "Authorization: token %s"\n' "$LOOMCYCLE_GITEA_TOKEN" | curl -sS $K -K - "$@"
