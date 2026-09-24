---
name: Path/mkdir
description: "Path op=mkdir — create an empty directory so it persists and lists; idempotent, and never overwrites something that is not a directory."
---
`mkdir` creates an **empty directory**. You rarely need it: directories are
implicit, so writing a document to `/a/b/c` already makes `/a` and `/a/b`
exist. Use `mkdir` only for a folder that should exist while it is empty.

## Arguments

- `path` (required) — the directory to create. Not `/`.
- `scope` — `agent` (default), `user` or `tenant`.

## Returns

`{ok: true, path, created}`. `created: false` with a `note` means the
directory already existed, explicitly or because something lives beneath it.
Calling `mkdir` twice is safe.

## Errors

- `path exists and is not a directory: ... (kind=document)` — the name is
  taken by a resource. `mkdir` never replaces it; pick another name or `mv` the
  resource first.
- `cannot mkdir the root path` — `/` always exists.

## Examples

An empty inbox folder in the tenant tree, for other agents to drop documents
into:

```json
{"op": "mkdir", "path": "/inbox", "scope": "tenant"}
```

```json result
{"ok": true, "path": "/inbox", "created": true}
```
