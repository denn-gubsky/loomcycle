-- 0111_password_gate.down.sql — drop the RFC FA P2 password-gate tables.
-- Nothing references them by FK; users (0068) is left as it was.
DROP TABLE IF EXISTS auth_throttle;
DROP INDEX IF EXISTS auth_resets_by_subject;
DROP TABLE IF EXISTS auth_resets;
DROP INDEX IF EXISTS auth_invites_by_gate_tenant;
DROP TABLE IF EXISTS auth_invites;
DROP TABLE IF EXISTS user_recovery_codes;
DROP TABLE IF EXISTS user_totp;
DROP TABLE IF EXISTS user_credentials;
DROP TABLE IF EXISTS user_identities;
