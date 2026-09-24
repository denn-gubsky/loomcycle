---
name: Document/define_type
description: "Document op=define_type — record a named chunk type and a description of its fields, for one document or for the whole scope."
---
`define_type` writes down what a chunk type means — its name and the fields a
chunk of that type is expected to carry — so that you, other agents and people
reading the store use the type the same way. It is a shared record, **not a
validator**: a chunk's `type` does not have to be defined first, and a chunk's
`fields` are never checked against the definition.

Defining a name that already exists in the same place replaces it.

## Arguments

- `name` (required) — the type name, e.g. `task`. Use the exact string you
  put in chunks' `type`.
- `fields` — an object describing the fields, in whatever shape is useful to
  readers, e.g. `{"owner": "person id", "due": "date"}`. Stored as given.
  Default: empty.
- `document_id` — attach the type to one document. Omit it for a scope-wide
  type. `list_types` lists these two groups separately.
- `scope` — `user` (default), `agent` or `tenant`. See
  `{"op":"help","topic":"scopes"}`.

## Returns

`{ok: true, name, document_id}` — `document_id` is `""` for a scope-wide type.

## Errors

- `define_type: missing required field: name`.

## Examples

A scope-wide `task` type:

```json
{"op": "define_type", "name": "task", "fields": {"owner": "who does it", "due": "date, YYYY-MM-DD", "estimate_hours": "number"}}
```

```json result
{"ok": true, "name": "task", "document_id": ""}
```

A type that belongs to one document only:

```json
{"op": "define_type", "name": "decision", "document_id": "b4407b522dc11495d3de371311db17f0", "fields": {"decided_by": "person", "date": "date"}}
```
