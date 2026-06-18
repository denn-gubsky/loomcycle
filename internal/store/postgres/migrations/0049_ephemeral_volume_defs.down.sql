-- 0049_ephemeral_volume_defs.down.sql — drop the RFC AH Phase 2b ephemeral
-- volume table. Removing the rows only "unmaps" the volumes; the on-disk
-- directories under <dynamic_root>/_ephemeral/ are unaffected (the row delete
-- never touches files — the fenced purge does).

DROP TABLE IF EXISTS ephemeral_volume_defs;
