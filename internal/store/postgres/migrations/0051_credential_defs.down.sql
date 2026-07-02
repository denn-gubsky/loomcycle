-- 0051_credential_defs.down.sql — drop the RFC AR CredentialDef table.
-- Dropping the rows unmaps every tenant/user credential; nothing external is
-- touched (external-backend pointers just become dangling; inline ciphertext is
-- unrecoverable without the KEK anyway).

DROP TABLE IF EXISTS credential_defs;
