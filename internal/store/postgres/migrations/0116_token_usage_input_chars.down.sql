-- 0116_token_usage_input_chars.down.sql — drop input_chars.
--
-- The rows stay (their purpose and seconds are 0113's); only the character
-- counts are lost, which no binary before this migration reads.
ALTER TABLE usage_archive DROP COLUMN IF EXISTS input_chars;
ALTER TABLE token_usage   DROP COLUMN IF EXISTS input_chars;
