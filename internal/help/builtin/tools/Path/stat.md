---
name: Path/stat
description: "Path op=stat — one path entry's full record, including the scope it lives in and when it was created and last changed."
---
`stat` returns everything the tree records about one entry. Use it when you
need the timestamps or want to confirm which scope a name lives in. `resolve`
is enough for finding the resource.

## Arguments

- `path` (required).
- `scope` — `agent` (default), `user` or `tenant`. `tenant` needs `tenant` in both the agent's `memory_scopes` and `sql_scopes`.

## Returns

`{full_path, name, kind, scope, resource_ref, created_at, updated_at, has_children?}`.
`has_children: true` means an entry that is not a directory also has names
beneath it that you can list.
`/` always exists and returns `{kind: "directory", full_path: "/", scope}`.

## Errors

- `no such path: ...` — not named in this scope. A directory that exists only
  because something lives beneath it has no record of its own, so `stat`
  reports it missing even though `ls` shows it. `mkdir` gives it one.

## Examples

```json
{"op": "stat", "path": "/prefs/voice", "scope": "user"}
```

```json result
{"full_path": "/prefs/voice", "name": "voice", "kind": "memory_entry", "scope": "user",
 "resource_ref": {"scope": "user", "scope_id": "u-123", "key": "voice", "facet": "kv"}, "created_at": "2026-09-01T10:00:00Z", "updated_at": "2026-09-20T08:12:00Z"}
```
