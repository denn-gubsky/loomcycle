---
name: TeamDef/get
description: "TeamDef op=get — read one version of a team by def_id, with its full definition."
---
`get` returns one stored version of a team, by the `def_id` that `create`,
`fork` or `list` gave you.

## Arguments

- `def_id` (required) — the version to read.

## Returns

`{def_id, name, version, parent_def_id, description, created_at,
created_by_agent_id, retired, content_sha256, definition, promoted: false}` —
`definition` is the whole graph.

## Errors

- `get: def_id "..." not found` — no such version in your tenant.

## Examples

```json
{"op": "get", "def_id": "tdf_9a1c3e5b7d2f4086"}
```
