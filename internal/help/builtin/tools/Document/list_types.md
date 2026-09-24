---
name: Document/list_types
description: "Document op=list_types — the chunk types defined with define_type, either scope-wide or for one document."
---
`list_types` returns the type definitions recorded with `define_type`.
**Without `document_id` you get only the scope-wide types; with it you get only
that document's types** — the two are never merged. To see everything a
document can use, call it twice.

It lists definitions only. To find which types chunks actually carry, use
`query_chunks` (for example with `sql`).

## Arguments

- `document_id` — list the types defined for this document. Omit it for the
  scope-wide types.
- `scope` — `user` (default), `agent` or `tenant`. See
  `{"op":"help","topic":"scopes"}`.

## Returns

`{types: [{name, document_id, fields}]}`, sorted by name. `document_id` is
`""` for scope-wide types.

## Errors

- An empty list is not an error: nothing was defined in that group. A type
  can be in use on chunks without ever having been defined.

## Examples

The scope-wide types:

```json
{"op": "list_types"}
```

```json result
{"types": [{"name": "task", "document_id": "", "fields": {"owner": "who does it", "due": "date, YYYY-MM-DD"}}]}
```

The types defined for one document:

```json
{"op": "list_types", "document_id": "b4407b522dc11495d3de371311db17f0"}
```
