---
name: Document/set_remote
description: "Document op=set_remote — bind a local document to a document on a peer loomcycle server that the operator configured as a document source."
---
`set_remote` connects one of your documents to a document on another loomcycle
server, so that `diff_remote` and `sync` can compare and copy between them.
**It only works with a document source the operator has configured** — you
name that source; you cannot point at an arbitrary URL. If no source is
configured, federation is not available on this server.

Binding is only a record: it contacts nobody and changes no content. The peer
is first reached by `diff_remote` or `sync`. Binding again replaces the
previous binding.

## Arguments

- `source` (required) — the configured document source's name, e.g.
  `hq-docs`.
- `remote_ref` (required) — the document's Path-tree path ON THE PEER, e.g.
  `/docs/runbook`. It must be a path starting with `/`; a document id is not
  resolved on the peer.
- `id` or `path` (one required) — the LOCAL document: its document id, or its
  path. This op does not read `document_id`.
- `scope` — `user` (default), `agent` or `tenant`. See
  `{"op":"help","topic":"scopes"}`. `sync` and `diff_remote` read the peer's
  document in this same scope.

## Returns

`{document_id, source, remote_ref, bound: true}`. The binding is
stored in the document root chunk's fields as `_remote`, so `get_chunk` on the
root shows it.

## Errors

- `set_remote: unknown document source "..."` — no source by that name is
  configured. Ask the operator; retrying with the same name is pointless.
- `set_remote: missing required field: remote_ref ...`.
- `set_remote: missing required field: id (or path)` — name the local document
  with `id` or `path`, not `document_id`.
- `set_remote: no such path: ...` — the local path is not in this scope.

## Examples

Bind a local runbook to the same runbook on a peer:

```json
{"op": "set_remote", "path": "/docs/runbook", "source": "hq-docs", "remote_ref": "/docs/runbook"}
```

```json result
{"document_id": "b4407b522dc11495d3de371311db17f0", "source": "hq-docs", "remote_ref": "/docs/runbook", "bound": true}
```

The same, naming the local document by id:

```json
{"op": "set_remote", "id": "b4407b522dc11495d3de371311db17f0", "source": "hq-docs", "remote_ref": "/teams/platform/runbook"}
```
