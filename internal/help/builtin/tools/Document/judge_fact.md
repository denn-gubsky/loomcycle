---
name: Document/judge_fact
description: "Document op=judge_fact — record whether a fact's stored source quote supports it; an unsupported fact is withheld from fact reads, never deleted."
---
`judge_fact` records a verdict on one fact: does the `source_quote` stored on
it actually support the claim? The verdict sets the fact's confidence (the
server owns the numbers) and records when, why and by whom. It changes nothing
else — not the text, not the timestamps — so a wrong verdict is fixed by judging
again. **Read the fact first (`get_chunk`) and judge its stored
`entity.source_quote`, not your own memory of the source.**

It judges a **relation** the same way: pass a statement id (from `get_edges`,
`backlinks` or `list_statements`) as `id`, and the verdict is checked against
the relation's evidence. An `unsupported` relation is withheld from
`get_edges`, `backlinks`, `graph_recall` and `search` `expand` (shown again
with `include_refuted`), never deleted. This is how an agent withdraws a
relation it can show is wrong; an operator's `reject_statement` is a ruling,
a verdict is a finding.

## Arguments

- `id` — the fact's chunk id, or a statement id. Or:
- `natural_key` — the fact's natural key, used when `id` is empty.
- `verdict` (required) — one of:
  - `supported` — the quote carries the claim (confidence 0.9).
  - `unclear` — it partly does (0.5).
  - `mistyped` — the quote supports it, but it is filed under the wrong type
    (0.4; stays visible, the fix is to retype it).
  - `unsupported` — the quote does not carry it (0.0; withheld from
    `list_facts` and `graph_recall`).
- `reason` (required) — one sentence on why; shown to the operator.
- `scope` — `user` (default), `agent` or `tenant`.

## Returns

`{chunk_id, verdict, confidence, judged_at, withheld}`. `withheld: true` means
the default fact reads now leave it out; `include_refuted: true` shows it again.
For a statement: `{edge_id, verdict, confidence, judged_at, withheld}`, and the
statement's `verified_at` is stamped too.

## Errors

- `judge_fact: the fact's quote no longer occurs in the chunk it was taken from ...`
  — a fact taken from a document passage is checked against the passage it
  was last taken from, as it is now; when the passage changed, judge it `unclear` or `unsupported`, or
  re-derive the fact.
- `judge_fact: verdict must be "supported", "unclear", "mistyped" or "unsupported" ...`
  — pass the word, never a number.
- `judge_fact: reason is required ...`.
- `judge_fact: name the fact by id or natural_key` — also what you get when the
  `natural_key` matches nothing in this scope.
- `judge_fact: that chunk is not a fact ...` — a plain document chunk has
  nothing to judge.
- `judge_fact: that fact records no source span ...` — there is no quote to
  check against. Only `unclear` is accepted from an agent; leave it unjudged.
  Retrying with another verdict is pointless.
- `judge_fact: that statement records no evidence verified in this scope ...`
  — the relation carries no quote from a chunk or chat turn of this scope, so
  only `unclear` is accepted from a run.

## Examples

Mark a fact as supported by its quote:

```json
{"op": "judge_fact", "id": "5d8e2f1a0b9c4d3e7f6a5b4c3d2e1f00", "verdict": "supported", "reason": "The quote states the user moved to Cluj-Napoca in May."}
```

```json result
{"chunk_id": "5d8e2f1a0b9c4d3e7f6a5b4c3d2e1f00", "verdict": "supported", "confidence": 0.9, "judged_at": 1772409600000000000, "withheld": false}
```

Refute a fact found by its natural key:

```json
{"op": "judge_fact", "natural_key": "person:ada|works_at|acme", "verdict": "unsupported", "reason": "The quote says Ada interviewed at Acme, not that she works there."}
```

Find a relation unsupported by its own evidence:

```json
{"op": "judge_fact", "id": "4b1d9c2e7a3f4e5d8c6b0a1f2e3d4c5b", "scope": "tenant", "verdict": "unsupported", "reason": "The quoted sentence cites the 2015 act, not this one."}
```

```json result
{"edge_id": "4b1d9c2e7a3f4e5d8c6b0a1f2e3d4c5b", "verdict": "unsupported", "confidence": 0, "judged_at": 1772409600000000000, "withheld": true}
```
