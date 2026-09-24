---
name: Memory/get
description: "Memory op=get — read one key/value entry by key, or by the Path-tree path it was named at; a missing key returns value null, not an error."
---
`get` reads one entry you (or another run) stored with `set`. Address it by
`key`, or by the `path` it was named at when it was set. **A missing entry is
not an error**: you get `"value": null`, so check the value before using it.

## Arguments

- `scope` (required) — `agent`, `user` or `tenant`: the keyspace the entry
  was written in. The same key in another scope is a different entry.
- `key` — the entry's key. Required unless you pass `path`.
- `path` — an absolute Path-tree path such as `/prefs/voice`, looked up in the
  same scope. Used only when `key` is absent; if you pass both, `key` wins.
- `include_provenance` — `true` also returns where the entry came from.
  Default `false`.

## Returns

`{value, expires_at}`. `value` is the stored JSON, or `null` when there is no
such entry (or it expired). `expires_at` is an RFC3339 time, or `null` for no
expiry.

With `include_provenance: true` the result also has `provenance`: `null` when
nothing was recorded, else any of `origin`, `class`, `source_session_id`,
`source_run_id`, and `origin_available`. A false `origin_available` means the
chat the fact came from has since been deleted — the fact is still valid, you
just cannot re-read its source.

## Errors

- `get: missing required field: key (or path)` — pass one of them.
- `get: no such path: /prefs/voice` — nothing is named at that path in THIS
  scope. Check the scope, or list with `Path op=ls`. Retrying is pointless.
- `get: path /docs/launch is a document, not a memory entry` — that path names
  something else; read it with its own tool.
- `Memory tool: scope "tenant" not in this agent's memory_scopes [user]` —
  this agent is not granted that scope. Use a granted one; retrying the same
  call is pointless.
- `Memory tool: scope=user requires a user_id on the run` — this run has no
  end-user, so `user` scope cannot resolve.

## Examples

Read the user's preferred voice:

```json
{"op": "get", "scope": "user", "key": "prefs/voice"}
```

```json result
{"value": {"tone": "concise", "language": "en"}, "expires_at": null}
```

Read the same entry through the path it was named at:

```json
{"op": "get", "scope": "user", "path": "/prefs/voice"}
```

Read a fact together with where it came from:

```json
{"op": "get", "scope": "user", "key": "memory/preference/coffee", "include_provenance": true}
```

```json result
{"value": "Prefers oat-milk flat whites", "expires_at": null,
 "provenance": {"origin": "consolidator", "class": "preference",
  "source_session_id": "b4407b522dc11495d3de371311db17f0", "origin_available": true}}
```
