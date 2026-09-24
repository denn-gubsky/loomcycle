---
name: Path/resolve
description: "Path op=resolve — what a path names: its kind and a reference to the resource behind it."
---
`resolve` answers **"what is at this path?"** It returns the entry's `kind` and
a `resource_ref` you hand to the resource's own tool. It never returns the
resource's content.

## Arguments

- `path` (required) — absolute, e.g. `/docs/launch`.
- `scope` — `agent` (default), `user` or `tenant`. Only that tree is searched.

## Returns

`{name, kind, full_path, resource_ref}`. `kind` is `document`, `volume_mount`,
`memory_entry` or `directory`. For a document, `resource_ref` carries the
`document_id` to pass to the Document tool. `/` always resolves, as a directory.

## Errors

- `no such path: /docs/launch` — nothing is named that in THIS scope. The same
  path may exist in another scope; resolve again with that `scope`.
- A path with `..`, spaces or a trailing segment outside `[a-zA-Z0-9._-]` is
  refused before any lookup.

## Examples

Find the document named `/docs/launch` in the end-user's tree:

```json
{"op": "resolve", "path": "/docs/launch", "scope": "user"}
```

```json result
{"name": "launch", "kind": "document", "full_path": "/docs/launch",
 "resource_ref": {"document_id": "b4407b522dc11495d3de371311db17f0"}}
```

Then read it with the Document tool, not Path:
`{"op": "get_document", "id": "b4407b522dc11495d3de371311db17f0", "scope": "user"}`.
