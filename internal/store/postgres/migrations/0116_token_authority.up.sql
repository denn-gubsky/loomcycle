-- 0116_token_authority.up.sql — RFC EY §8: a token's authority mode.
--
-- '' (the default, and every existing row) is a frozen token: its
-- allowed_scopes are its ceiling, as before. 'inherit' is a per-user token
-- whose ceiling the server re-derives from its user's CURRENT access_mode on
-- every request, so a promotion or a demotion reaches the token without a
-- re-mint. Not secret.
ALTER TABLE operator_token_defs ADD COLUMN IF NOT EXISTS authority TEXT NOT NULL DEFAULT '';
