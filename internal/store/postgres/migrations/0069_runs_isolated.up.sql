-- RFC BX P2b: persist whether a run belongs to an ISOLATED MEMBER (a
-- substrate:user principal). The confinement is computed at run-start from the
-- principal's scopes (auth.IsIsolated) — never a wire/body/model field — and
-- rides the run row so a resumed / snapshot-restored / crash-recovered run
-- reconstructs its data-scope confinement (the tenant-shared + cross-tenant
-- global keyspaces are refused for an isolated run) without the original
-- principal on ctx (the same reason `operator_key_restricted`, `interactive`,
-- and `tenant_id` are denormalised here). Additive default-false column; legacy
-- rows + every pre-P2b deployment read false (fail-open). CreateRun stamps it.
ALTER TABLE runs ADD COLUMN IF NOT EXISTS isolated BOOLEAN NOT NULL DEFAULT FALSE;
