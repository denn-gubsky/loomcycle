#!/usr/bin/env bash
# Use it for one command, without saving it in the repository (README "Setup"):
#   git -c credential.helper="$PWD/gitcreds.sh" -c credential.useHttpPath=false clone ...
# git calls `gitcreds.sh get`; we reply username/password from env (or .env.local).
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
if [[ -z "${LOOMCYCLE_GITEA_TOKEN:-}" && -f "$HERE/.env.local" ]]; then set -a; source "$HERE/.env.local"; set +a; fi
if [[ "${1:-}" == "get" ]]; then
  printf 'username=%s\n' "${LOOMCYCLE_GITEA_USER:-oauth2}"
  printf 'password=%s\n' "${LOOMCYCLE_GITEA_TOKEN:-}"
fi
exit 0
