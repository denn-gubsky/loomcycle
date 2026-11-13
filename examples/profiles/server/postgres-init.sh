#!/bin/sh
# Runs once, when the bundled Postgres initialises an empty data directory, as
# its superuser (POSTGRES_USER). After this the superuser is not used again:
# the runtime and the migration connect as `loomcycle`.
#
# `loomcycle` is not a superuser. It owns the two databases, and it may create
# roles, which SQL Memory needs: it makes one login role per scope, each
# confined to its own schema. It has no server-file, program or replication
# powers.
#
# The image runs this file when it is executable and sources it when it is
# not, so it sets nothing in the shell beyond `-e`, which the image's own
# entrypoint already has.
set -e
: "${LOOMCYCLE_DB_PASSWORD:?set LOOMCYCLE_DB_PASSWORD in env.local}"

psql -v ON_ERROR_STOP=1 -v pw="$LOOMCYCLE_DB_PASSWORD" --username "$POSTGRES_USER" --dbname postgres <<'SQL'
CREATE ROLE loomcycle LOGIN PASSWORD :'pw' CREATEROLE;

-- The store, and the second database for SQL Memory and Documents.
CREATE DATABASE loomcycle OWNER loomcycle;
CREATE DATABASE loomcycle_sqlmem OWNER loomcycle;

-- Lets loomcycle cap each scope role's on-disk sort spill.
GRANT SET ON PARAMETER temp_file_limit TO loomcycle;

-- pgvector for the store. An extension is installed by the superuser; the
-- schema migration then finds it there.
\connect loomcycle
CREATE EXTENSION IF NOT EXISTS vector;

-- SQL Memory: scopes never use `public`, and find pgvector's types in a
-- schema of its own that every scope role may use (types and operators only).
\connect loomcycle_sqlmem
REVOKE ALL ON SCHEMA public FROM PUBLIC;
CREATE SCHEMA sqlmem_ext;
CREATE EXTENSION vector SCHEMA sqlmem_ext;
GRANT USAGE ON SCHEMA sqlmem_ext TO PUBLIC;
SQL
