---
name: Document/sync
description: "Document op=sync — reconcile a bound document with its peer copy, pulling the peer's keyed chunks in (default) or pushing yours up."
---
`sync` copies changes between a local document and the peer document it was
bound to with `set_remote`. It only works when the operator has configured
that peer as a document source. Run `diff_remote` first to see what it will
change.

**Only keyed chunks travel** — chunks that carry a `natural_key` (written with
`upsert_chunk`). Chunks are matched across the two servers by that key, never
by id. Chunks made with `create_chunk` or `import_md` have no key and are
skipped (counted in `excluded_unkeyed`).

For each keyed chunk, the side being written to receives the other side's
title, type, status, body and tags, its place in the tree (under the chunk
with the same parent key, at the same position; with no keyed parent, under
the root), and the `link_chunks` links between keyed chunks. Nothing is
deleted: a chunk or link that exists only on the receiving side stays. An
overwritten body is kept in that chunk's `history`. Facts a judge marked
unsupported are not copied.

## Arguments

- `id` or `path` (one required) — the LOCAL document, as bound with
  `set_remote`. This op does not read `document_id`.
- `direction` — `pull` (default): the peer's chunks are written into your
  document. `push`: your chunks are written to the peer.
- `scope` — `user` (default), `agent` or `tenant`: the scope of the local
  document, and the scope read on the peer. See
  `{"op":"help","topic":"scopes"}`.

## Returns

`{direction, source, remote_ref, remote_document_id, local_document_id,
created, updated, unchanged, reparented, edges_added, excluded_unkeyed,
excluded_withheld}` — counts of keyed chunks. `excluded_unkeyed` counts the
sending side's chunks that could not be carried.

## Errors

- `sync: this document is not bound to a remote (call set_remote first)`.
- `sync: unknown document source ...` — the source was removed from the
  configuration. Ask the operator.
- `sync: fetch remote document: ...` / `sync: remote document not found at ...`
  — the peer is unreachable, refused the call, or has no document at that
  path. Check `remote_ref`; a retry helps only for a network failure.
- `sync: direction must be "pull" (default) or "push" ...` — `up` and `down`
  belong to `reorder_chunk`.
- The sync is not all-or-nothing: if it fails part-way, what was already
  written stays. Running it again continues from where it stands.

## Examples

Pull the peer's latest keyed chunks into the local runbook:

```json
{"op": "sync", "path": "/docs/runbook"}
```

```json result
{"direction": "pull", "source": "hq-docs", "remote_ref": "/docs/runbook",
 "remote_document_id": "0d9c8b7a6f5e4d3c2b1a09f8e7d6c5b4", "local_document_id": "b4407b522dc11495d3de371311db17f0",
 "created": 2, "updated": 1, "unchanged": 14, "reparented": 2, "edges_added": 1, "excluded_unkeyed": 3, "excluded_withheld": 0}
```

Push local edits up to the peer:

```json
{"op": "sync", "id": "b4407b522dc11495d3de371311db17f0", "direction": "push"}
```
