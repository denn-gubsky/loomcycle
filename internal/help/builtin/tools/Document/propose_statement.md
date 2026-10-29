---
name: Document/propose_statement
description: "Document op=propose_statement — suggest a relation between two chunks for review; nothing live changes until an operator accepts it."
---
`propose_statement` files a relation as **proposed**: it is recorded with its
evidence and shows in `get_edges` and the review queue, but it is not followed
by `graph_recall` or listed by `backlinks` until an operator accepts it. Use it
when you found a relation in text and want it checked rather than asserted.
It cannot change or remove anything that exists.

Proposing a relation that already exists adds your evidence to it. Passing
`id` proposes a correction of an existing relation instead: it is filed beside
the original (its `attributes.revision_of` names it), and the original stays as
it is.

## Arguments

- `from_id`, `to_id`, `kind` — the relation, as for `link_chunks`. With `id`
  they come from that statement; `kind` may change it.
- `id` — the existing statement this proposal revises.
- `evidence` — the supporting text, as for `link_chunks`. Strongly
  recommended: a reviewer reads it to decide, and a quote that is not in its
  source is refused.
- `confidence`, `valid_at`, `invalid_at`, `attributes` — as for `link_chunks`.
- `scope` — `user` (default), `agent` or `tenant`.

## Returns

`{id, status, created, evidence_written, ontology_valid, warnings?}`.
`status` is `proposed` unless the server asserted it outright, which happens
only for relation kinds the operator declared safe to assert automatically.
`ontology_valid: false` with `warnings` means the relation does not fit the
tenant's declared vocabulary; it is still filed, marked, for the reviewer.

## Errors

- `kind_reserved` — the kind is one the runtime writes.
- `evidence_quote_not_found` — copy the quote exactly from its source.
- `statement_rejected` — an operator already rejected this relation; do not
  propose it again.

## Examples

```json
{"op": "propose_statement", "from_id": "e1f3a5b7c9d14e2f8a4b6c8d0e2f4a6b", "to_id": "5f1c9e2a7b3d4e6f8a9b0c1d2e3f4a5b", "kind": "implements",
 "evidence": [{"chunk_id": "e1f3a5b7c9d14e2f8a4b6c8d0e2f4a6b", "quote": "This order implements Directive 2019/1937"}], "confidence": 0.8}
```

```json result
{"id": "7c2e9a1b3d4f4e6a8b0c2d4e6f8a1b3c", "status": "proposed", "created": true, "evidence_written": 1, "ontology_valid": true}
```
