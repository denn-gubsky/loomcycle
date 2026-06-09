-- 0042_schedule_defs_tenant_id.up.sql — RFC N (completion): tenant-scope
-- the Schedule definition plane.
--
-- Third of the RFC-N-completion migrations (0040–0044). A ScheduleDef's
-- active pointer was keyed on name alone, so two tenants' schedules of the
-- same name collided (last-writer-wins) and the def-authoring plane was not
-- tenant-isolated. This adds the OWNING-tenant axis (distinct from the
-- run-execution tenant carried inside the schedule's definition JSON, which
-- already drives RunInput.TenantID when the sweeper fires).
--
-- The sweeper's ScheduleRunStateListDue JOINs schedule_def_active on the
-- globally-unique def_id, so firing stays correct with no JOIN change —
-- each def fires under its own (tenant, name). Mirror of 0037, minus the
-- dynamic_* tier (Schedule has none).
--
-- '' = the shared/operator/legacy tenant; existing rows backfill via the
-- column DEFAULT, so single-tenant deployments are unchanged.

ALTER TABLE schedule_defs ADD COLUMN tenant_id TEXT NOT NULL DEFAULT '';
ALTER TABLE schedule_defs DROP CONSTRAINT schedule_defs_name_version_key;
ALTER TABLE schedule_defs ADD CONSTRAINT schedule_defs_tenant_name_version_key UNIQUE (tenant_id, name, version);

ALTER TABLE schedule_def_active ADD COLUMN tenant_id TEXT NOT NULL DEFAULT '';
ALTER TABLE schedule_def_active DROP CONSTRAINT schedule_def_active_pkey;
ALTER TABLE schedule_def_active ADD PRIMARY KEY (tenant_id, name);
