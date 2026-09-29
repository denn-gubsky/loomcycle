-- 0087_teamdef_active_promoter.up.sql — the promoter's confinement on a team's
-- active pointer.
--
-- An armed team subscription starts the team's walks itself, with no caller on
-- ctx, so the walk runs under the confinement of whoever PROMOTED the team: the
-- operator-key restriction and the isolation bit, captured at promote time.
-- They live on the pointer, not on teamdefs, because promoting is the act that
-- arms and it stays out of the def's content hash.
--
-- NULLABLE with no DEFAULT on purpose: a pointer promoted before this migration
-- reads as "not captured", which the sweep treats as fully confined until the
-- team is promoted again. A FALSE default would read every existing pointer as
-- unrestricted — the defect this closes.
ALTER TABLE teamdef_active ADD COLUMN IF NOT EXISTS promoter_operator_key_restricted BOOLEAN;
ALTER TABLE teamdef_active ADD COLUMN IF NOT EXISTS promoter_isolated BOOLEAN;
