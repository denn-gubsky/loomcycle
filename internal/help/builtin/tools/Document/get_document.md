---
name: Document/get_document
description: "Document op=get_document — a document's metadata (title, root_chunk_id, type, status, tags) by id or Path-tree path; not its chunks."
---
`get_document` returns a document's **metadata**, found by `id` or by `path`.
It does not return the chunks or their text: for the content, call `export_md`
or `query_chunks` with the `document_id`, or `get_chunk` on the root. Use it to
turn a path into a `document_id` and a `root_chunk_id`.

## Arguments

- `id` (or `document_id`) — the document id. Or:
- `path` — the document's Path-tree name, e.g. `/docs/launch-plan`. Used only
  when `id` is empty.
- `scope` — `user` (default), `agent` or `tenant`. The path and the id are
  looked up in this scope only.
- `across_scopes` — `true` also looks for the same subject in your other
  readable scopes (only meaningful when the root is a fact subject). Costs up
  to three extra queries; never reaches another user's store.

## Returns

`{document_id, title, root_chunk_id, color_enabled}`, plus when present:
`type`, `status`, `tags`, `color_scheme`.

When the root is a fact subject, `references` lists facts in OTHER documents
that point at it (`[{id, title, type, document_id, kind}]`, at most 100; then
`references_truncated` and a `references_note` telling you to use `list_facts`
with `about`). With `across_scopes`, `across_scopes` lists each other scope that
knows the subject: `[{scope, document_id, entity_chunk_id, facts}]`.

## Errors

- `get_document: missing required field: document_id (or id, or path)`.
- `get_document: no such path: /...` — nothing is named there in THIS scope.
  Check `scope`, or list with `Path op=ls`.
- `get_document: path /... is a directory, not a document` — the path names
  something else; `ls` it.
- `get_document: no such document: <id>` — wrong id, or the id belongs to
  another scope.

## Examples

Open a document by its path:

```json
{"op": "get_document", "path": "/docs/launch-plan"}
```

```json result
{"document_id": "b4407b522dc11495d3de371311db17f0", "title": "Launch plan", "root_chunk_id": "9e1c3a7d2b5f4e6081a2c3d4e5f60718", "type": "plan", "status": "draft", "color_enabled": false}
```

A subject document in the tenant store, with what your other scopes know about
the same subject:

```json
{"op": "get_document", "path": "/facts/ada-lovelace", "scope": "tenant", "across_scopes": true}
```
