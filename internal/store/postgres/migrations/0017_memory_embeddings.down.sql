-- Drop the table; we don't DROP EXTENSION vector because other
-- applications sharing this Postgres instance may use it. The
-- extension is harmless when no consumer is wired to it.
DROP TABLE IF EXISTS memory_embeddings;
