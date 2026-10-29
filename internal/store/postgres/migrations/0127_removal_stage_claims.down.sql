-- 0127_removal_stage_claims.down.sql — drop the stage claim, the prior
-- status and the re-enable stamp.
ALTER TABLE scim_users DROP COLUMN IF EXISTS reactivated_at;
ALTER TABLE auth_removals DROP COLUMN IF EXISTS prior_status;
ALTER TABLE auth_removals DROP COLUMN IF EXISTS stage;
