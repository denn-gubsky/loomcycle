---
name: Context/self
description: "Context op=self — your identity (agent, run, user, tenant), model, the scope values each of your tools grants, volumes, network allowlist and context-window usage."
---
`self` describes you and your run in one call. Call it before choosing a
`scope` for Memory, Path, Document, History or any other scoped tool: the
`scopes` block says which values this run may use and, for the rest, the exact
refusal you would get. Guessing a scope costs a failed call; this costs none.

## Arguments

None besides `op`.

## Returns

Always present:

- `agent_name`, `agent_id`, `run_id`, `user_id`, `tenant_id`, `user_tier`,
  `agent_def_id` — who you are. `agent_name`, `user_id` and `tenant_id` are
  what the `agent`, `user` and `tenant` scopes resolve to. Pass `run_id` to
  another agent (for example in a Channel message) when it needs to refer to
  this run.
- `provider`, `model` — the model you are running on right now.
- `operator_authored` — `true` when a person wrote your definition, `false`
  when an agent wrote it at runtime.
- `volumes` (with `bindings: [{name, path, mode, default}]` and a
  `path_convention`) — or `filesystem: "none ..."` when no volume is bound, in
  which case Read/Write/Edit/Glob/Grep/Bash will refuse.

Present when they apply:

- `scopes` — per scoped tool, a list of fields, each
  `{applies?, granted: [scopes], refused: {scope: reason}}`. Memory has two
  entries: the first covers its ordinary operations, the second (with
  `applies: "sql_* ops"`) its SQL operations, which have a separate grant.
- `network` — `{allowed_hosts, source}`: the hosts HTTP, WebFetch and
  WebSearch may reach. An empty list means no web access.
- `sampling`, `max_context_tokens`, `compaction`, `context_policy` — the
  generation and context settings in effect.
- `context` — `{used_tokens, max_tokens?, used_pct?}` as of your last
  completed turn. Absent before your first turn completes.
- `context_distill_declined` — `{mode, reason, message?}` when an attempt to
  compact did nothing. **If it is present, calling `compact` again will not
  help**; report the reason instead.
- `self_names` — the names the end-user declared for themselves.
- `principal` — the credential this run acts under: `tenant_id`, `subject`,
  `scopes`, `is_admin`, `legacy`, `token_def_id`, `token_suffix`. Never the
  token itself.
- `loomcycle` (`version`, `commit`, `build_time`) and `server`
  (`listen_addr`, `url`).

## Errors

None in practice: this op reads only your own run.

## Examples

Check your identity and your scope grants:

```json
{"op": "self"}
```

```json result
{"agent_name": "researcher", "agent_id": "a_9f2c41d07be35a18", "run_id": "r_4e8b1c9a2f7d6035",
 "user_id": "u_1024", "tenant_id": "acme", "user_tier": "pro", "agent_def_id": "",
 "provider": "anthropic", "model": "claude-sonnet-4-5", "operator_authored": true,
 "scopes": {"Memory": [
   {"granted": ["agent", "user"], "refused": {"tenant": "Memory tool: scope \"tenant\" not in this agent's memory_scopes [agent user]"}},
   {"applies": "sql_* ops", "granted": [], "refused": {"agent": "...", "user": "...", "tenant": "..."}}]},
 "filesystem": "none — no volume bound; ...",
 "context": {"used_tokens": 18400, "max_tokens": 200000, "used_pct": 9}}
```
