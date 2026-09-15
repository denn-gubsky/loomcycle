#!/bin/sh
# Start the DB-1 rig on 127.0.0.1:8873. Run from the repo root after `make build`.
#
# .env.local carries DEEPSEEK_API_KEY and LOOMCYCLE_AUTH_TOKEN; it is sourced into
# THIS shell so neither value has to appear on a command line or in a log.
set -e
here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../.." && pwd)
cd "$root"
set -a; [ -f .env.local ] && . ./.env.local; set +a
export LOOMCYCLE_LISTEN_ADDR="${LOOMCYCLE_LISTEN_ADDR:-127.0.0.1:8873}"
export LOOMCYCLE_DATA_DIR="${LOOMCYCLE_DATA_DIR:-$here/data}"
exec ./bin/loomcycle --config "$here/db1.yaml"
