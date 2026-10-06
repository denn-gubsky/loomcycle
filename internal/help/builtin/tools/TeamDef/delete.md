---
name: TeamDef/delete
description: "TeamDef op=delete — remove a whole team from your tenant: every version and its active pointer. Cannot be undone."
---
`delete` removes a team by name: every version, and the active pointer. It
cannot be undone. To take one version out of use and keep the rest, use
`retire`.

## Arguments

- `name` (required) — the team to remove.

## Returns

`{name, deleted: true}`.

## Errors

- `delete: team "..." not found` — your tenant has no team of that name.

## Examples

```json
{"op": "delete", "name": "pr-review-scratch"}
```
