-- 0115_auth_external_gates.down.sql — drop the RFC FA P4 tables.
DROP INDEX IF EXISTS auth_removals_by_erase_due;
DROP INDEX IF EXISTS auth_removals_by_unbind_due;
DROP TABLE IF EXISTS auth_removals;
DROP INDEX IF EXISTS auth_assertion_jtis_by_expiry;
DROP TABLE IF EXISTS auth_assertion_jtis;
