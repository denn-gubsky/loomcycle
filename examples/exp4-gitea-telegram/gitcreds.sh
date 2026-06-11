#!/usr/bin/env bash
# Configure a clone with:
#   git config credential.helper "$PWD/gitcreds.sh"; git config credential.useHttpPath false
# git calls `gitcreds.sh get`; we reply username/password from env (or .env.local).
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
if [[ -z "${LOOMCYCLE_GITEA_TOKEN:-}" && -f "$HERE/.env.local" ]]; then set -a; source "$HERE/.env.local"; set +a; fi
if [[ "${1:-}" == "get" ]]; then
  printf 'username=%s\n' "${LOOMCYCLE_GITEA_USER:-oauth2}"
  printf 'password=%s\n' "${LOOMCYCLE_GITEA_TOKEN:-}"
fi
exit 0
