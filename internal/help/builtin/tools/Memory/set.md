---
name: Memory/set
description: "Memory op=set — write or overwrite one key/value entry, with optional TTL, embedding for semantic search, a Path-tree name, and dates for when it was said or true."
---
`set` writes one JSON value under a key, replacing whatever was there. It is
synchronous: a `get` right after sees the new value. Use it for anything you
must read back reliably. **`value` must be valid JSON** — to store text, pass
a JSON string (`"value": "Prefers email"`), not bare words.

## Arguments

- `scope` (required) — `agent`, `user` or `tenant`.
- `key` (required) — the entry's key. Slash-separated keys such as
  `prefs/voice` group well under `list`'s `prefix`. Keys starting with
  `trace.turn:` are reserved and refused.
- `value` (required) — any JSON value.
- `ttl` — seconds until the entry expires. Omit for no expiry.
- `path` — also name the entry in the Path tree at this absolute path (e.g.
  `/prefs/voice`), in the same scope, so `get` and `Path op=ls` can find it.
- `embed` — `true` also stores an embedding so `search` and `recall` can find
  the row by meaning. Needs the operator's embedder and vector store.
- `embed_text` — the text to embed with `embed: true`. Defaults to the JSON
  text of `value`; pass a clean sentence for better matches.
- `observed_at` — when the thing was SAID or happened, if not now. Omit when
  you do not know; a guessed date hides the row from searches by time.
- `valid_at` / `invalid_at` — when it became true, and when it stopped being
  true. `invalid_at` must be after `valid_at`.
- `provenance` — `{class, source_session_id, source_run_id}`: a short label
  for the kind of fact and the chat/run it came from. Descriptive only.
- `from_pending` — a pending-item id from `pending_drain`; the server fills in
  where the fact came from. Used by consolidation agents.

## Returns

`{ok: true}`, plus:

- `embedded: true` when `embed` succeeded; `embedded: false` and an
  `embed_warning` when the value was stored but embedding failed.
- `path` when the Path name was registered, or `path_warning` when the value
  was stored but the name was not (retry with the same call).

## Errors

- `set: value is not valid JSON` — quote strings: `"value": "text"`.
- `set: missing required field: key` / `value`.
- `set: value (N bytes) exceeds max M bytes`, or `Memory.set: scope "user"
  quota ... would be exceeded` — store less, or `delete` old entries first.
  Retrying the same call is pointless.
- `set: observed_at "last week" is not an RFC3339 timestamp` — resolve the
  date yourself and pass e.g. `2026-03-14T00:00:00Z`.
- `path must be absolute` / `invalid path segment` — paths are `/`-rooted,
  segments use letters, digits, `.`, `_`, `-`. Nothing was written.
- `memory: no embedder configured` / `memory: vector index not configured` —
  `embed: true` is not available on this server. Nothing was written; retry
  without `embed`.
- `Memory.set: core block "persona" ... is read_only` — the operator owns
  that key; you may not change it.

## Examples

Remember the user's preferred tone:

```json
{"op": "set", "scope": "user", "key": "prefs/voice", "value": {"tone": "concise", "language": "en"}}
```

```json result
{"ok": true}
```

Store the same entry and give it a Path name you can browse:

```json
{"op": "set", "scope": "user", "key": "prefs/voice", "value": {"tone": "concise", "language": "en"}, "path": "/prefs/voice"}
```

```json result
{"ok": true, "path": "/prefs/voice"}
```

Store a dated remark so it can be found by meaning and by time:

```json
{"op": "set", "scope": "user", "key": "notes/boston-trip", "value": "Met the design team in Boston", "embed": true, "embed_text": "Met the design team in Boston", "observed_at": "2026-03-04T10:00:00Z", "valid_at": "2026-03-03T00:00:00Z"}
```
