---
name: Memory/list
description: "Memory op=list — list key/value entries in one scope, optionally only keys starting with a prefix, with their values."
---
`list` returns the entries in one scope, keys and values together. Narrow it
with `prefix` — that is also how you page through a large scope, because
there is no cursor. An empty list is not an error: it means nothing matches
in THIS scope.

## Arguments

- `scope` (required) — `agent`, `user` or `tenant`.
- `prefix` — only keys that start with this, e.g. `prefs/`.
- `limit` — maximum entries (default 100, at most 1000).

## Returns

`{entries: [{key, value, expires_at}], truncated}`. `truncated: true` means
more entries matched than `limit`; narrow the `prefix` to see the rest.
Expired entries are never listed.

## Errors

- `Memory tool: scope "tenant" not in this agent's memory_scopes [...]` — not
  granted; retrying is pointless.
- `list: ...` wraps a storage failure; retrying is reasonable.

## Examples

Everything this user has stored under `prefs/`:

```json
{"op": "list", "scope": "user", "prefix": "prefs/"}
```

```json result
{"entries": [
  {"key": "prefs/voice", "value": {"tone": "concise"}, "expires_at": null},
  {"key": "prefs/timezone", "value": "Europe/Kyiv", "expires_at": null}],
 "truncated": false}
```

A short look at this agent's own keyspace:

```json
{"op": "list", "scope": "agent", "limit": 20}
```
