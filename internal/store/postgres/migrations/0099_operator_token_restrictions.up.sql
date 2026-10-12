-- 0099_operator_token_restrictions.up.sql — a token can be narrowed below its scopes.
--
-- A scope says which kind of thing a token may do; a restriction says which
-- ones (this agent only, …). A token handed to a client machine is minted for
-- one purpose, and the record states that purpose on the credential itself,
-- so it holds on every transport and not only behind one proxy route.
--
-- JSON, so a later restriction is a new key and not a new column. NULL =
-- unrestricted, which is every row that existed before this column.
ALTER TABLE operator_token_defs ADD COLUMN IF NOT EXISTS restrictions JSONB;
