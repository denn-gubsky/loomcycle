-- 0117_token_authority.down.sql — undo RFC EY §8's token authority column.
ALTER TABLE operator_token_defs DROP COLUMN IF EXISTS authority;
