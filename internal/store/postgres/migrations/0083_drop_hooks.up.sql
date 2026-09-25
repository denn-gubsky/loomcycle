-- 0083_drop_hooks.up.sql — the hook registration table is gone.
--
-- Hooks were registered at runtime into a process-wide registry, which in
-- cluster mode persisted here. A run now carries its hooks: its AgentDef names
-- them (inline webhooks or HookDefs, in hook_defs), so nothing registers into a
-- shared table any more. A deployment upgrading with rows here moves each
-- registered webhook onto the tools of the AgentDefs it gated.
DROP TABLE IF EXISTS hooks;
