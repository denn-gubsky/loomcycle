-- Runs ONCE, on first postgres init, as the bootstrap superuser `loomcycle`
-- (POSTGRES_USER). Being a superuser, `loomcycle` already has CREATEROLE, which
-- the SQL-Memory tier needs at runtime to provision one login role per scope.
--
-- The postgres image creates the MAIN database (loomcycle) from POSTGRES_DB.
-- Here we add the SQL-Memory aux database + its pgvector schema. The MAIN db's
-- `vector` extension is created later by `loomcycle migrate up` (migration 0017),
-- so we do NOT create it here.

CREATE DATABASE loomcycle_sqlmem OWNER loomcycle;

\connect loomcycle_sqlmem

-- SQL-Memory expects pgvector installed into a dedicated schema it bakes onto
-- each per-scope role's search_path. Only needed for vector search INSIDE SQL
-- Memory; the pgvector/pgvector:pg18 image ships the extension binaries.
CREATE SCHEMA IF NOT EXISTS sqlmem_ext;
CREATE EXTENSION IF NOT EXISTS vector SCHEMA sqlmem_ext;
