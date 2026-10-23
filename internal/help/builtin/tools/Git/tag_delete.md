---
name: Git/tag_delete
description: "Git op=tag_delete — delete a local tag; the commit it named stays."
---
`tag_delete` deletes a tag from this repository. The commit it named is not
touched. A tag already pushed stays on the remote. Needs a read-write volume.

## Arguments

- `volume`, `repo` — which repository (defaults: your default volume, its root).
- `tag` (required) — the tag to delete.

## Returns

`{op: "tag_delete", ok: true, head, files}` — `head` and `files` as `status`
reports them.

## Errors

- `invalid_argument` — no such tag, or an invalid tag name.
- `volume_refused` — the volume is read-only.

## Examples

```json
{"op": "tag_delete", "repo": "service", "tag": "build-142"}
```
