---
name: Context/tools
description: "Context op=tools — the tools you hold, each with its full description and a side-effect class."
---
`tools` lists every tool this run may call, with each tool's full
description. It is longer than `guide`; use `guide` when you only need the
operations and required arguments, and `doc` when you need one tool's full
input schema.

## Arguments

None besides `op`.

## Returns

`{tools: [{name, description, side_effect_class}], count}`, sorted by name.
`side_effect_class` is a coarse hint: `pure` (only reads), `state` (may
change stored state), `network`, `filesystem`, `privileged` (runs code or
other agents), or `unknown` (an MCP tool). A tool with some reading and some
writing operations is classed by what it may do at worst.

## Errors

None. An empty list means the runtime attached no tool list to this run.

## Examples

List your tools:

```json
{"op": "tools"}
```

```json result
{"count": 2, "tools": [
  {"name": "Agent", "description": "Spawn or drive named sub-agents, ...", "side_effect_class": "privileged"},
  {"name": "Context", "description": "Read-only runtime introspection. ...", "side_effect_class": "pure"}]}
```
