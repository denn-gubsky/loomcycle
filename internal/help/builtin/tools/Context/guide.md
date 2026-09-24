---
name: Context/guide
description: "Context op=guide — a compact digest of every tool you hold: its operations, its required arguments and a one-line usage hint."
---
`guide` is the short version of your tool list: for each tool, which `op`
values it accepts, which arguments are always required, and one line on the
thing most often got wrong. Call it when you are unsure which operation to use
or what a call needs. For the full rules and examples of one operation, follow
up with `{"op":"help","topic":"<Tool>/<op>"}`.

## Arguments

None besides `op`.

## Returns

`{tools: [{name, side_effect_class, ops?, required?, hint?}], count}`, sorted
by name, listing only the tools you hold.

- `ops` — the tool's `op` values. Absent for a tool that takes no `op`.
- `required` — arguments the tool always requires. An operation may need more
  than this; its help article says which.
- `side_effect_class` — `pure`, `state`, `network`, `filesystem`,
  `privileged` or `unknown` (an MCP tool).

## Errors

None. An empty list means the runtime attached no tool list to this run.

## Examples

Get the digest:

```json
{"op": "guide"}
```

```json result
{"count": 2, "tools": [
  {"name": "Context", "side_effect_class": "pure", "ops": ["self", "tools", "guide", "doc", "permissions", "agents", "lineage", "evaluations", "channels", "help", "time", "compact", "state", "capabilities"], "required": ["op"]},
  {"name": "Skill", "side_effect_class": "pure", "ops": ["invoke", "list"],
   "hint": "Load a skill by name to pull its guidance into context; you can only load skills your agent's skills allowlist permits."}]}
```
