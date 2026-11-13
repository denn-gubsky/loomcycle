-- Runs once, when the bundled Postgres initialises an empty data directory, as
-- the superuser `loomcycle` (POSTGRES_USER). A superuser may create roles,
-- which SQL Memory needs: it makes one login role per scope.
--
-- The image creates the main database from POSTGRES_DB; `loomcycle migrate up`
-- gives it its schema. This adds the second database, for SQL Memory and
-- Documents, with pgvector in the schema SQL Memory looks for it in.

CREATE DATABASE loomcycle_sqlmem OWNER loomcycle;

\connect loomcycle_sqlmem

CREATE SCHEMA IF NOT EXISTS sqlmem_ext;
CREATE EXTENSION IF NOT EXISTS vector SCHEMA sqlmem_ext;
