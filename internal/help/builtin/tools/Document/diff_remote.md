---
name: Document/diff_remote
description: "Document op=diff_remote — dry run: what a sync between a bound document and its peer copy would change, without writing anything."
---
`diff_remote` compares a local document with the peer document it was bound to
by `set_remote` and reports the differences, changing neither side. Use it
before `sync` to decide on `pull` or `push`. Like `sync`, it needs a document
source configured by the operator, and it compares only keyed chunks (chunks
with a `natural_key`), matched by that key.

## Arguments

- `document_id` (or `id`), or `path` — one is required: the LOCAL document.
- `scope` — `user` (default), `agent` or `tenant`. See
  `{"op":"help","topic":"scopes"}`.

## Returns

`{source, remote_ref, remote_document_id, local_document_id, only_local,
only_remote, diverged, retagged, reparented, same, edges_only_local,
edges_only_remote, excluded_unkeyed_local, excluded_unkeyed_remote}`.

- `only_local` — keyed chunks only you have; a `push` would create them on the
  peer.
- `only_remote` — keyed chunks only the peer has; a `pull` would create them
  here.
- `diverged` — on both sides with a different title, type, status or body.
- `retagged` — on both sides with different tags.
- `reparented` — a sync in either direction would move them in the tree.
- Each of these is a list of `{natural_key, title}`. `same` counts chunks
  whose content matches; `edges_only_*` count links present on one side;
  `excluded_unkeyed_*` count chunks with no key, which neither op can carry.

## Errors

- `diff_remote: this document is not bound to a remote (call set_remote first)`.
- `diff_remote: unknown document source ...` — ask the operator.
- `diff_remote: fetch remote document: ...` / `remote document not found at ...`
  — the peer is unreachable or has no document at `remote_ref`.

## Examples

Preview a sync of the local runbook:

```json
{"op": "diff_remote", "path": "/docs/runbook"}
```

```json result
{"source": "hq-docs", "remote_ref": "/docs/runbook",
 "remote_document_id": "0d9c8b7a6f5e4d3c2b1a09f8e7d6c5b4", "local_document_id": "b4407b522dc11495d3de371311db17f0",
 "only_local": [{"natural_key": "runbook:paging", "title": "Paging policy"}],
 "only_remote": [], "diverged": [{"natural_key": "runbook:rollback", "title": "Rollback plan"}],
 "retagged": [], "reparented": [], "same": 14, "edges_only_local": 0, "edges_only_remote": 1,
 "excluded_unkeyed_local": 3, "excluded_unkeyed_remote": 0}
```
