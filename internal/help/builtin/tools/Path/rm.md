---
name: Path/rm
description: "Path op=rm — remove a path entry, and with recursive:true everything beneath it; the resources behind the names are NOT deleted."
---
`rm` removes a **name** from the tree. The resource it pointed at is not
deleted: a document unlinked from the tree still exists and is still
reachable by its id. To delete the resource itself, use its own tool (for
example `Document op=delete_document`).

## Arguments

- `path` (required).
- `scope` — `agent` (default), `user` or `tenant`.
- `recursive` — required to remove a path that has entries beneath it.
- `resource_too` — not supported; passing `true` is refused.

## Returns

`{ok: true, removed, n_removed}` — `n_removed` counts the path and every
descendant removed with it.

## Errors

- `path "..." has N descendant(s); pass recursive:true to remove them` — a
  guard, not a failure: nothing was removed.
- `no such path: ...` — not named in this scope.
- `cannot remove the root path`.

## Examples

Unlink one entry:

```json
{"op": "rm", "path": "/docs/old-notes", "scope": "user"}
```

```json result
{"ok": true, "removed": "/docs/old-notes", "n_removed": 1}
```

Remove a directory and every name beneath it (the documents survive, reachable
by id):

```json
{"op": "rm", "path": "/scratch", "recursive": true}
```
