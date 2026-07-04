-- RFC AX: persist whether a run is RESTRICTED from the operator's host provider
-- API key. The permission is a NEGATIVE bit (false = allowed) computed at
-- run-start from the principal + the LOOMCYCLE_OPERATOR_KEY_RESTRICTION gate; it
-- rides the run row so a resumed / snapshot-restored / crash-recovered run
-- reconstructs its restriction without the original principal on ctx (the same
-- reason `interactive` and `tenant_id` are denormalised here). Additive
-- default-false column; legacy rows + every gate-off deployment read false
-- (fail-open — the deliberate backward-safety trade). CreateRun stamps it.
ALTER TABLE runs ADD COLUMN IF NOT EXISTS operator_key_restricted BOOLEAN NOT NULL DEFAULT FALSE;
