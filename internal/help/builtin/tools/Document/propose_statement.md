---
name: Document/propose_statement
description: "Document op=propose_statement — suggest a relation between two chunks for review; nothing live changes until an operator accepts it."
---
`propose_statement` files a relation as **proposed**: it is recorded with its
evidence and shows in `get_edges` and the review queue, but it is not followed
by `graph_recall` or listed by `backlinks` until an operator accepts it. Use it
when you found a relation in text and want it checked rather than asserted.
It cannot change or remove anything that exists.

Which to use: `link_chunks` asserts a relation you are the authority on;
`propose_statement` files one for a person to rule on, and is what a pass that
extracts relations from text should send. Only an operator accepts or rejects
a proposal (`accept_statement`, `reject_statement`); an agent never rules on
its own.

Proposing a relation that is already proposed adds your evidence to it. A
relation that is already asserted is not changed by a proposal: pass its `id`
to propose a correction instead, which is filed beside the original (its
`attributes.revision_of` names it) while the original stays as it is.

## Arguments

- `from_id`, `to_id`, `kind` — the relation, as for `link_chunks`. With `id`
  they come from that statement and `from_id` / `to_id` are not read; `kind`
  may change it. To point a relation at a different chunk, propose a new
  statement without `id`.
- `id` — the existing statement this proposal revises.
- `evidence` — the supporting text, as for `link_chunks`: a list of
  `{chunk_id, quote, char_start?, char_end?}`, `{session_id, event_seq, quote}`
  or `{scope, natural_key, quote}`. Strongly recommended: a reviewer reads it
  to decide, and a quote that is not in its source is refused.
- `confidence`, `valid_at`, `invalid_at`, `attributes` — as for `link_chunks`.
- `method` — how the relation was found: `rule:<name>` (code matched it) or
  `extractor:<name>` (a model the code drives found it); `<name>` is 1–64 of
  `a-z 0-9 _ . / -`. Only an agent whose definition grants that kind of method
  may send it (a code agent's `statement_methods`). Omitted, the statement is
  `manual`, as a person's.
- `scope` — `user` (default), `agent` or `tenant`.

## Returns

`{id, status, created, evidence_written, ontology_valid, warnings?, ruled_by?,
ruling_id?, review_route?}`.
`status` is `proposed` — except for a `rule:` statement of a kind the tenant's
vocabulary marks auto-asserting, with every quote in a chunk: that is
`asserted`, and over a relation that already holds it adds its evidence. A
correction proposed with `id` always stays `proposed`.
When the tenant ontology has a review policy (its Review section), the policy
decides instead: `asserted` or `rejected` with `ruled_by: policy:<rule>@<version>`
and the `ruling_id` it is recorded under, or `proposed`, with `review_route:
judge` when it is left for the judge. A policy's rejection is a verdict on
this proposal only: proposing the relation again files it again.
`ontology_valid: false` with `warnings` means the relation does not fit the
tenant's declared vocabulary (each warning starts with its code:
`kind_not_declared`, `domain_range_violation`, `characteristic_violation`,
`qualifier_violation`); it is still filed for the reviewer, marked
`attributes.ontology_valid: false`, with its confidence lowered to at most 0.3
so it sorts after clean proposals. `evidence_capped: true` means the relation
already holds as much evidence as it keeps.

## Errors

- `kind_reserved` — the kind is one the runtime writes.
- `method_not_granted` — `method` is malformed, or your agent may not stamp that
  kind of method; omit it.
- `evidence_quote_not_found` — copy the quote exactly from its source.
- `statement_rejected` — an operator already rejected this relation (at any
  `valid_at`), or the statement passed as `id` is a rejected one; do not propose
  it again.
- `attribute_reserved` — `attributes` names `judge`, `review_route` or
  `classified_by`, which only the runtime writes; leave them out.
- `statement_asserted` — the relation already holds; propose a correction with
  `id`, or leave it.
- `confidence must be between 0 and 1`. `invalid_at must be after valid_at`.
  `attributes must be a JSON object of at most 4096 bytes`.
- `no such statement` — the `id` you passed is not a statement of this scope.
- `from_id: no such chunk` / `to_id: no such chunk` — take chunk ids from
  `search`, `query_chunks` or `get_document`.

## Examples

```json
{"op": "propose_statement", "from_id": "e1f3a5b7c9d14e2f8a4b6c8d0e2f4a6b", "to_id": "5f1c9e2a7b3d4e6f8a9b0c1d2e3f4a5b", "kind": "implements",
 "evidence": [{"chunk_id": "e1f3a5b7c9d14e2f8a4b6c8d0e2f4a6b", "quote": "This order implements Directive 2019/1937"}], "confidence": 0.8}
```

```json result
{"id": "7c2e9a1b3d4f4e6a8b0c2d4e6f8a1b3c", "status": "proposed", "created": true, "evidence_written": 1, "ontology_valid": true}
```

Propose a correction to a relation that is already asserted: the clause does
not merely cite the article, it implements it. The correction keeps the
original's two ends (with `id`, `from_id` and `to_id` are not read; only `kind`
can change), and the original stays live until an operator rules:

```json
{"op": "propose_statement", "id": "4b1d9c2e7a3f4e5d8c6b0a1f2e3d4c5b", "kind": "implements",
 "evidence": [{"chunk_id": "e1f3a5b7c9d14e2f8a4b6c8d0e2f4a6b", "quote": "This order implements Article 12 of the Act"}]}
```

A code agent granted `rule` methods filing what its citation rule found, for a
kind the tenant marks auto-asserting (then it is written `asserted`):

```json
{"op": "propose_statement", "scope": "tenant", "from_id": "e1f3a5b7c9d14e2f8a4b6c8d0e2f4a6b", "to_id": "5f1c9e2a7b3d4e6f8a9b0c1d2e3f4a5b", "kind": "cites", "method": "rule:citations",
 "evidence": [{"chunk_id": "e1f3a5b7c9d14e2f8a4b6c8d0e2f4a6b", "quote": "Article 12 of the Act", "char_start": 120, "char_end": 141}]}
```
