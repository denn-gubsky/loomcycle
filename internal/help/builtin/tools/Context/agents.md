---
name: Context/agents
description: "Context op=agents — the agents declared in the operator's config, with tier, model, tool count and the id of each one's active runtime definition."
---
`agents` lists the agents the operator declared in configuration, so you can
find a name to spawn with `Agent` or a `def_id` to inspect with `lineage` and
`evaluations`. The one thing to know: **it lists configured agents only**. An
agent created at runtime under a new name does not appear here; use the
`AgentDef` tool to list those.

## Arguments

- `prefix` — only names starting with this, e.g. `research/`.

## Returns

`{agents: [{name, tier?, model?, provider?, tools_count, active_def_id?,
error?}], count}`, sorted by name. `active_def_id` is present when a
runtime definition is active for that name; pass it as `def_id` to
`lineage` or `evaluations`. `error` reports a lookup failure for one name
while the rest of the list is still returned.

Being listed does not mean you may spawn it: the `Agent` tool must be in your
tools, and the name must resolve for your tenant.

## Errors

- `agents: not configured (no Cfg)` — this runtime has no config to list;
  retrying is pointless.

## Examples

List every configured agent:

```json
{"op": "agents"}
```

```json result
{"count": 2, "agents": [
  {"name": "cv-adapter", "tier": "middle", "tools_count": 4, "active_def_id": "def_b4407b522dc11495"},
  {"name": "researcher", "tier": "top", "tools_count": 6}]}
```

Only the agents in one group:

```json
{"op": "agents", "prefix": "research/"}
```
