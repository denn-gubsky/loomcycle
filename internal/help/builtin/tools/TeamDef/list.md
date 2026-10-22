---
name: TeamDef/list
description: "TeamDef op=list — every stored version of one team in your tenant and the shared tier, retired ones included."
---
`list` returns every version of a team, so you can pick one to
`get`, `promote`, `retire` or fork from.

## Arguments

- `name` (required) — the team.

## Returns

`{"name": ..., "versions": [{def_id, version, parent_def_id, description,
created_at, retired, content_sha256, definition, ...}]}` — empty when neither
your tenant nor the shared tier has a team of that name. Shared versions carry
`shared: true`: you may read, run and fork them, but not retire or promote them.

## Errors

- `list: missing required field: name`.

## Examples

```json
{"op": "list", "name": "pr-review"}
```
