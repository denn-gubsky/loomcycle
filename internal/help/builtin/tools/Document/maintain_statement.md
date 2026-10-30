---
name: Document/maintain_statement
description: "Document op=maintain_statement — the upkeep of a relation by a code agent granted statement methods, or an operator: verify, expire, demote, merge, mark or re-point evidence, flag."
---
`maintain_statement` keeps an existing relation true to its sources: it stamps
that it was re-checked, retires it, sends it back to review, folds a duplicate
into its twin, or keeps an evidence row pointing at text that still says it.
It never asserts a relation (acceptance is an operator's ruling) and never
deletes one or an evidence row: what it retires stays readable with
`include_retired`.

Only a code agent whose definition grants statement methods
(`statement_methods`) may call it, and an operator session. Inside a run it
also needs the `propose` permission on the documents the relation joins.

A relation a person wrote (its `method` is `manual`) takes only `verify` and
`flag`, from anyone: flag it with your reason and let a person decide.

## Arguments

- `id` (required) — the statement, from `list_statements` or `get_edges`.
- `action` (required) — one of:
  - `verify` — stamp `verified_at`; nothing else changes.
  - `expire` — retire it. `reason` is required; it and your `method` are
    recorded as why and by what it was retired.
  - `demote` — an asserted relation goes back to `proposed` for review.
    `reason` is required.
  - `merge` — move its evidence onto `survivor_id`, a live statement of the
    same `from_id`, `to_id` and `kind`, then retire it with the reason
    `merged`. A row the survivor already holds (same source, quote and span)
    stays where it was.
  - `stale_evidence` — mark the evidence row `evidence_id` stale: its quote is
    no longer where it points.
  - `relocate_evidence` — re-point the evidence row `evidence_id` to
    `chunk_id`, where its quote must occur; the span is recomputed and the
    stale mark cleared.
  - `flag` — set `attributes.needs_review` (and `needs_review_reason`) for a
    person to look at.
- `reason` — why. Required for `expire` and `demote`; recorded by `flag`.
- `survivor_id` — `merge`: the statement that keeps the evidence.
- `evidence_id` — `stale_evidence`, `relocate_evidence`: the row, by its id
  from `get_edge`.
- `chunk_id` — `relocate_evidence`: the chunk the quote is in now.
- `method` — `rule:<name>` or `extractor:<name>`, as for `propose_statement`:
  what did this. Only an agent granted that kind of method may send it.
- `scope` — `user` (default), `agent` or `tenant`.

## Returns

`{id, action, ok: true}`. `merge` adds `survivor_id`, `evidence_moved`,
`evidence_duplicate` and, when the survivor's evidence was full,
`evidence_left`.

## Errors

- `permission` — the caller holds no statement methods and is not an operator.
- `method_not_granted` — `method` is malformed or outside your grant.
- `statement_manual` — a person wrote the relation; only `verify` and `flag`
  apply.
- `statement_not_asserted` — `demote` of a relation that is not asserted.
- `statement_expired` / `statement_rejected` — the relation is already retired,
  or an operator rejected it.
- `survivor_mismatch` — the survivor is not another live statement of the same
  relation.
- `evidence_quote_not_found` — the quote does not occur in `chunk_id`.
- `no such evidence row on that statement` — `evidence_id` belongs to another
  relation.
- `statement_changed` — the relation changed or was retired while you acted;
  reload it (`get_edge`) and decide again.
- `reason is required` — for `expire` and `demote`.

## Examples

```json
{"op": "maintain_statement", "id": "7c2e9a1b3d4f4e6a8b0c2d4e6f8a1b3c", "action": "expire",
 "reason": "the cited article was repealed", "method": "rule:maintain"}
```

```json result
{"id": "7c2e9a1b3d4f4e6a8b0c2d4e6f8a1b3c", "action": "expire", "ok": true}
```

```json
{"op": "maintain_statement", "id": "7c2e9a1b3d4f4e6a8b0c2d4e6f8a1b3c", "action": "merge",
 "survivor_id": "5f1c9e2a7b3d4e6f8a9b0c1d2e3f4a5b", "method": "rule:maintain"}
```

```json result
{"id": "7c2e9a1b3d4f4e6a8b0c2d4e6f8a1b3c", "action": "merge", "ok": true, "survivor_id": "5f1c9e2a7b3d4e6f8a9b0c1d2e3f4a5b", "evidence_moved": 2, "evidence_duplicate": 1}
```
