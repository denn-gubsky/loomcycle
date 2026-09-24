---
name: Document/verification_stats
description: "Document op=verification_stats — counts of how many facts in a scope carry a source quote, have been judged, are verified, or are withheld."
---
`verification_stats` reports, for one scope's whole fact store, how much of it
is evidenced: how many facts carry a source quote, how many a judge has looked
at, how many were affirmed, and how many are withheld. It counts claims only,
never subject name nodes, and includes retired facts. It takes no arguments
besides `scope`.

## Arguments

- `scope` — `user` (default), `agent` or `tenant`.

## Returns

`{facts, with_span, judged, supported, withheld, unverifiable_no_span,
awaiting_judge, verified_share}`:

- `facts` — all claims in the scope.
- `with_span` — claims that carry a `source_quote`.
- `judged` — claims that have a verdict.
- `supported` — claims judged `supported`.
- `withheld` — claims below the confidence floor (judged `unsupported`, or
  written with a very low confidence).
- `unverifiable_no_span` — claims with no quote: nobody can ever check them.
- `awaiting_judge` — claims with a quote and no verdict yet: `judge_fact` these.
- `verified_share` — `supported / facts`; omitted when there are no facts.

## Errors

- `verification_stats: ...` wraps a storage failure; retrying is reasonable.
- `{facts: 0}` is an answer: this scope has no facts. Check the scope.

## Examples

Coverage of the user's fact store:

```json
{"op": "verification_stats"}
```

```json result
{"facts": 120, "with_span": 103, "judged": 80, "supported": 71, "withheld": 4, "unverifiable_no_span": 17, "awaiting_judge": 23, "verified_share": 0.5916666666666667}
```

Coverage of the tenant's shared facts:

```json
{"op": "verification_stats", "scope": "tenant"}
```
