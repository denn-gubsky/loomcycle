---
name: Document/set_path
description: "Document op=set_path — give an existing document a Path-tree name (an extra name; existing names are kept)."
---
`set_path` names an existing document at `path` in the Path tree, in the
document's own scope. It **adds** a name: any name the document already has
stays, so it is not a move. To rename, `set_path` the new name and then
`Path op=rm` the old one, or use `Path op=mv`. Calling it again with the same
path is harmless.

## Arguments

- `id` (required) — the **document** id. `document_id` is accepted in its
  place.
- `path` (required) — the new name, absolute, e.g. `/docs/launch-plan`. Not
  `/`. If the path already names something else, that resource loses the name
  to this document.
- `scope` — `user` (default), `agent` or `tenant`: the scope the document is in.

## Returns

`{document_id, path}` with the path in canonical form.

## Errors

- `set_path: missing required field: id (the document_id)`.
- `set_path: missing required field: path`.
- `set_path: no such document: <id>` — the id is wrong or belongs to another
  scope; pass the scope the document was created in.
- `set_path: invalid path segment ...` / `path may not be the root` — segments
  are letters, digits, `.`, `_`, `-`.
- `an agent may not modify the ontology document ...` — refused for the tenant
  ontology.

## Examples

Name a document that was created without a path:

```json
{"op": "set_path", "id": "b4407b522dc11495d3de371311db17f0", "path": "/docs/launch-plan"}
```

```json result
{"document_id": "b4407b522dc11495d3de371311db17f0", "path": "/docs/launch-plan"}
```

Add a second name for a tenant document:

```json
{"op": "set_path", "id": "b4407b522dc11495d3de371311db17f0", "path": "/runbooks/current", "scope": "tenant"}
```
