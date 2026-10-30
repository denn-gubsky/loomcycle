---
name: Document/resolve_anchor
description: "Document op=resolve_anchor — find a work by its key (e.g. law:ua/2297-vi) and a part of it by anchor, as of a date; optionally file a placeholder for a work not held yet."
---
`resolve_anchor` turns a citation into a chunk: the work by its key, the part by
its anchor — the part's heading path such as `article-12/paragraph-3`, or the
anchor it had when first imported (a renumbered part is still found by it). It
answers with the version of the part in force at `as_of`, so a citation to a
past edition resolves to the text that was in force then.

When the scope does not hold the work — or holds it in a document you may not
read — `found` is false. With
`create_placeholder: true` an empty document is filed for it instead: link
citations to its root, and its backlinks are the list of works still to import.
Importing the work (`import_md` with `reimport: true` and the same `work_key`)
fills that placeholder in.

## Arguments

- `work_key` (required) — the work's key.
- `anchor` — the part to find.
- `as_of` — the moment to resolve at (unix nanoseconds; default now).
- `create_placeholder` — file a placeholder when the work is not held.
- `scope` — `user` (default), `agent` or `tenant`.

A heading path can name two parts after a renumbering: the part now under that
heading, and an older part whose FIRST anchor it was (Article 2 renumbered to
Article 3 still carries `article-2` as its first anchor). The part now under the
heading wins; pass `as_of` before the renumbering to reach the other.

## Returns

`{work_key, found, document_id?, root_chunk_id?, placeholder?, chunk_id?}`.
`chunk_id` is absent when the part is not found at that moment.

## Errors

- `resolve_anchor: missing required field: work_key`.
- With `create_placeholder`: a permission error when the work is held in a
  document the caller may not read (the key is taken, so no placeholder can be
  filed), or when the caller may not write the whole scope.

## Examples

```json
{"op": "resolve_anchor", "work_key": "law:ua/2297-vi", "anchor": "article-12", "as_of": 1735689600000000000}
```

```json result
{"work_key": "law:ua/2297-vi", "found": true, "document_id": "b4407b522dc11495d3de371311db17f0", "root_chunk_id": "c7d2e4f6a8b04c1d9e3f5a7b9c1d3e5f", "placeholder": false, "chunk_id": "9a3e7c1f2b4d4a6e8c0f1a2b3c4d5e6f"}
```
