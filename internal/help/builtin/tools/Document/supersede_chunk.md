---
name: Document/supersede_chunk
description: "Document op=supersede_chunk — retire an old fact in favour of a new one without deleting it, so questions about the past still have an answer."
---
`supersede_chunk` records a correction: the chunk `supersedes_id` is retired and
the chunk `id` replaces it. The old chunk is **not deleted** — its "stopped
being true" and "stopped being believed" times are both set to now, and a
`supersedes` edge links new to old — so default reads return only the new fact
while an `as_of` read about before the correction still finds the old one.
**`id` is the NEW fact and `supersedes_id` is the OLD one; swapping them
retires the correction.** Write the replacement first (`upsert_chunk` with a
new `natural_key`), then call this.

## Arguments

- `id` (required) — the replacement chunk. It must already exist.
- `supersedes_id` (required) — the chunk being retired.
- `scope` — `user` (default), `agent` or `tenant`. Both chunks must be in it.

A chunk can be retired only once. Corrections form a chain: to correct a
correction, supersede the newer fact, not the original.

## Returns

`{id, supersedes, retired_at}` (unix nanos). Repeating the same call returns
`{id, supersedes, already: true}` and changes nothing.

## Errors

- `supersede_chunk: missing required field: id (the REPLACEMENT chunk)`.
- `supersede_chunk: missing required field: supersedes_id (the chunk being retired)`.
- `supersede_chunk: a chunk cannot supersede itself`.
- `supersede_chunk: chunk "<id>" not found in this scope` — create the
  replacement first, or pass the scope both chunks are in.
- `supersede_chunk: chunk "A" is already superseded by "B". To correct that newer fact, supersede "B" instead ...`
  — retry with `supersedes_id` set to the id it names.
- `an agent may not modify the ontology document ...` — operator-only.

## Examples

Replace an outdated fact with its correction:

```json
{"op": "supersede_chunk", "id": "0f1e2d3c4b5a69788796a5b4c3d2e1f0", "supersedes_id": "5d8e2f1a0b9c4d3e7f6a5b4c3d2e1f00"}
```

```json result
{"id": "0f1e2d3c4b5a69788796a5b4c3d2e1f0", "supersedes": "5d8e2f1a0b9c4d3e7f6a5b4c3d2e1f00", "retired_at": 1772409600000000000}
```

The same correction in the tenant store:

```json
{"op": "supersede_chunk", "id": "0f1e2d3c4b5a69788796a5b4c3d2e1f0", "supersedes_id": "5d8e2f1a0b9c4d3e7f6a5b4c3d2e1f00", "scope": "tenant"}
```
