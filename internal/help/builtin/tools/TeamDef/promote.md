---
name: TeamDef/promote
description: "TeamDef op=promote — make one version of a team the active one, the version run uses by name."
---
`promote` points a team's name at one of its versions. From then on `run` by
name walks that version. Promoting an older version is how you roll back.

## Arguments

- `def_id` (required) — the version to make active.

## Returns

`{def_id, name, promoted: true}`.

## Errors

- `promote: def_id "..." not found` — no such version in your tenant.
- `promote: ...` — a name one of the team's own agents would run under is
  taken.

## Examples

```json
{"op": "promote", "def_id": "tdf_9a1c3e5b7d2f4086"}
```
