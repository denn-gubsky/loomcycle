---
name: Git/remote_remove
description: "Git op=remote_remove — remove a remote, its remote-tracking branches and the upstream settings that pointed at it."
---
`remote_remove` removes a remote as `git remote remove` does: its section of
the configuration, its remote-tracking branches (`<remote>/...`), and the
upstream setting of every local branch that tracked it. Local branches and
commits are kept. Needs a read-write volume.

## Arguments

- `volume`, `repo` — which repository (defaults: your default volume, its root).
- `remote` (required) — the remote's name.

## Returns

`{op: "remote_remove", ok: true, head, files}` — `head` and `files` as
`status` reports them.

## Errors

- `invalid_argument` — no such remote, or an invalid remote name.
- `volume_refused` — the volume is read-only.

## Examples

```json
{"op": "remote_remove", "repo": "service", "remote": "upstream"}
```
