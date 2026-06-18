-- 0048_volume_defs.down.sql — drop the RFC AH Phase 2a VolumeDef table.
-- Removing the rows only "unmaps" the dynamic volumes; the on-disk
-- directories under the operator's dynamic_root are unaffected (the
-- substrate's `delete` op has the same non-destructive semantics).

DROP TABLE IF EXISTS volume_defs;
