-- 0112_oidc_gate.down.sql — undo RFC FA P1's schema.
ALTER TABLE operator_token_defs DROP COLUMN IF EXISTS issued_by_gate;
ALTER TABLE ui_sessions DROP COLUMN IF EXISTS auth_time;
DROP TABLE IF EXISTS auth_login_states;
