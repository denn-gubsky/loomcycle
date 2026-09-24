---
name: Context/doc
description: "Context op=doc — one tool's full description and input schema, plus its help article and the list of its operation articles."
---
`doc` returns everything about one tool you hold: its description, its full
input schema, and — when the tool has one — its help article and the names of
its operation articles. Use it when a call keeps failing validation and you
need to see every argument the tool accepts.

## Arguments

- `name` (required) — the tool's exact name, e.g. `Memory`, `Channel`,
  `mcp__jobs__getAgentContext`. Case matters.

## Returns

`{name, description, input_schema, side_effect_class}`. When the tool has a
help article, also `help` (the article text), and when it has operation
articles, `operation_topics` (e.g. `["Channel/ack", "Channel/await", ...]`)
and a `hint`. Read an operation with `{"op":"help","topic":"<one of them>"}`.

## Errors

- `doc: missing required field: name`.
- `doc: tool "X" not found (use op=tools to list available)` — check the
  spelling against `tools` or `guide`.
- `doc: tool "X" is not in this agent's tools` — the tool exists but you do
  not hold it. You cannot call it, so its details are withheld. Do not retry.

## Examples

See the Channel tool's full schema and article:

```json
{"op": "doc", "name": "Channel"}
```

```json result
{"name": "Channel", "description": "Persistent inter-agent message bus. ...", "input_schema": {"type": "object", "properties": {"op": {"type": "string", "enum": ["publish", "..."]}}},
 "side_effect_class": "state", "help": "The `Channel` tool is a persistent message bus. ...",
 "operation_topics": ["Channel/ack", "Channel/await", "Channel/broadcast", "Channel/list_channels", "Channel/peek", "Channel/publish", "Channel/release", "Channel/subscribe"],
 "hint": "Read one operation, with examples, via op=help topic=<name>, e.g. topic=Channel/ack."}
```
