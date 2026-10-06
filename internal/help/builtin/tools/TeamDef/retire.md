---
name: TeamDef/retire
description: "TeamDef op=retire — retire one version of a team, or put a retired one back; the team and its other versions stay."
---
`retire` marks one version retired (`retired: true`) or brings it back
(`retired: false`). It does not delete anything; to remove a whole team use
`delete`.

When the version subscribes to channels, the answer names them: a retired
team stops reading them, so check that nothing is still piling up there.

## Arguments

- `def_id` (required) — the version.
- `retired` (required) — `true` to retire it, `false` to un-retire it.

## Returns

`{def_id, retired}`, plus `sources_released` (retiring) or `sources_resumed`
(un-retiring) — the channels the version reads — when it has any.

## Errors

- `retire: missing required field: retired (true|false)`.
- `retire: def_id "..." not found` — no such version in your tenant.

## Examples

```json
{"op": "retire", "def_id": "tdf_9a1c3e5b7d2f4086", "retired": true}
```
