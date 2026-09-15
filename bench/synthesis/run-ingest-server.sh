#!/bin/sh
# RFC DB-2 ingest rig: Postgres + pgvector + the memory bundle's extractor.
# Run from the repo root. .env.local carries the provider keys and the bearer.
set -e
here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../.." && pwd)
cd "$root"
set -a; [ -f .env.local ] && . ./.env.local; set +a
export LOOMCYCLE_PRESETS=base,memory
export LOOMCYCLE_STORAGE_BACKEND=postgres
export LOOMCYCLE_PG_AUTOMIGRATE=1
export LOOMCYCLE_PGVECTOR_ENABLED=1
# The consolidator writes the chunk/graph tier through SQL Memory. Without this
# every sql_exec refuses and the run produces flat facts with no relations --
# which is exactly the structure the traversal arm is supposed to walk.
# NOTE: the Postgres tier creates a per-scope LOGIN role, so the DSN role needs
# CREATEROLE or Documents fail with "permission denied to create role".
export LOOMCYCLE_SQLMEM_ENABLED=1
# SQL Memory on a Postgres main store demands a SEPARATE database, not a schema,
# and the aux DSN must carry a PASSWORD -- the admin password keys the per-scope
# role credentials. So the DSN is a secret and lives outside the repo.
[ -f "$HOME/.config/loomcycle-bench/db2.env" ] && . "$HOME/.config/loomcycle-bench/db2.env"
export LOOMCYCLE_PG_DSN="${LOOMCYCLE_PG_DSN:-postgres://localhost:5432/loomcycle_db2?sslmode=disable}"
# The extractor and the embedder must reach the SAME host; ingest.yaml pins the
# embedder, this pins the provider.
export OLLAMA_BASE_URL="${OLLAMA_BASE_URL:-http://100.112.7.68:11434}"
export LOOMCYCLE_OLLAMA_LOCAL_NUM_CTX="${LOOMCYCLE_OLLAMA_LOCAL_NUM_CTX:-32768}"
export LOOMCYCLE_LISTEN_ADDR="${LOOMCYCLE_LISTEN_ADDR:-127.0.0.1:8874}"
export LOOMCYCLE_DATA_DIR="${LOOMCYCLE_DATA_DIR:-$here/data}"
exec ./bin/loomcycle --config "$here/ingest.yaml"
