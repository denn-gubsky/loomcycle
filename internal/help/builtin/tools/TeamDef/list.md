---
name: TeamDef/list
description: "TeamDef op=list — every stored version of one team in your tenant, retired ones included."
---
`list` returns every version of a team, so you can pick one to
`get`, `promote`, `retire` or fork from.

## Arguments

- `name` (required) — the team.

## Returns

`{"name": ..., "versions": [{def_id, version, parent_def_id, description,
created_at, retired, content_sha256, definition, ...}]}` — empty when your
tenant has no team of that name.

## Errors

- `list: missing required field: name`.

## Examples

```json
{"op": "list", "name": "pr-review"}
```
