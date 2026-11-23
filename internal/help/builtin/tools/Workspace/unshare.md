---
name: Workspace/unshare
description: "Workspace op=unshare — end the share of one of your workspaces with another run."
---
`unshare` ends what `share` gave: the other run can no longer reach the
workspace. A share also ends by itself when the workspace is released or
either run ends.

## Arguments

- `name` (required) — your workspace.
- `with` (required) — the agent id you shared it with.

## Returns

`{name, with, unshared}`: `unshared` is false when there was no share with
that agent id, which is not an error.

## Errors

- `not_found` — this run has no workspace of that name.

## Examples

```json
{"op": "unshare", "name": "build", "with": "a_7f3c2e"}
```

```json result
{"name": "build", "unshared": true, "with": "a_7f3c2e"}
```
