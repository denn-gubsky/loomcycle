---
name: Path/mv
description: "Path op=mv — rename or move a path within one scope; a directory moves with everything beneath it, and the resources themselves are untouched."
---
`mv` renames a path, or moves it elsewhere in the same tree. Only the NAME
changes: the document, memory entry or volume behind it keeps its id and
content. Moving a directory moves every entry under it in one step.

## Arguments

- `path` (required) — the current path.
- `to` (required) — the new path. It must not exist yet.
- `scope` — `agent` (default), `user` or `tenant`. `tenant` needs `tenant` in both the agent's `memory_scopes` and `sql_scopes`. Both paths are in this one
  scope: `mv` cannot carry a name from one scope to another.

## Returns

`{ok: true, from, to}`. When `to` lands under a name that is a document
(e.g. `/a/b/c` where `/a/b` is a document), the move still succeeds and the
result adds `warnings: ["parent_is_document: /a/b names a document; this name is listed under it"]`:
`/a/b` is now a document with names beneath it (`ls` marks it
`has_children: true`). Move the name elsewhere if you meant a plain folder.

## Errors

- `destination already exists: ...` — `mv` never overwrites. `rm` the
  destination first if replacing it is really what you want.
- `no such path: ...` — the source is not named in this scope.
- `cannot move a path into itself or its own subtree` — e.g. `/docs` to
  `/docs/old`.
- `source and destination are the same path`.
- `... is a reserved name in the tenant scope ...` — `/memory/ontology`,
  `/memory/entities` and `/memory/tenant_root` in the tenant tree are the
  operator's: an agent cannot remove, rename or replace them, nor move a directory above them such as `/memory`.

## Examples

Rename a document:

```json
{"op": "mv", "path": "/docs/draft", "to": "/docs/launch-plan", "scope": "user"}
```

```json result
{"ok": true, "from": "/docs/draft", "to": "/docs/launch-plan"}
```

Move a whole directory, and everything in it, under an archive folder:

```json
{"op": "mv", "path": "/docs/2025", "to": "/archive/2025", "scope": "tenant"}
```
