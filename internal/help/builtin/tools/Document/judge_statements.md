---
name: Document/judge_statements
description: "Document op=judge_statements — a judge agent records its verdicts on proposed relations, and rules on those the review policy routes to it, at or above the policy's floor."
---
`judge_statements` records a judge's verdicts on proposed relations. Only a run
of a code agent whose definition lists `judge` in `statement_methods` may call
it; a model never calls it itself. The runtime, not the caller, decides what each
verdict may do:

- Every verdict is recorded on its statement as `attributes.judge`
  (`verdict`, `confidence`, `rationale`, `outcome`, the judging `run_id`, the
  rule's `floor`, and the `model` and `verdict_run_id` when the verdict came
  from a run this run started).
- A verdict **rules** only on a proposal the tenant's review policy routes to
  the judge now (a `judge` rule decides it, and the policy marked it
  `review_route: judge`), in `rule` mode, at or above that rule's floor (its
  `floor:`, else the section's `judge_floor`, default 0.85), and where the run
  may rule as an operator would (write on the from-document, link on both
  ends). `accept` asserts it and `reject` rejects it, recorded as
  `ruled_by: judge:<run id>`, `judged_by: judge`, with no operator mark: a
  judge's rejection is a verdict on that proposal, not a tombstone on the
  relation.
- Anything else is only recorded, and the proposal waits for a person:
  `below_floor`, `unsure`, `not_permitted` (escalated), `recommend` (the call's
  `write`, or the rule's `mode:recommend`) and `not_routed`.
- Refused, with nothing recorded: a statement an operator ruled on
  (`operator_ruled`), one that is not a proposal (`not_a_proposal`), one that
  already carries a verdict (`already_judged` — a statement is judged once), and
  one that does not exist or that the run may not read (`not_found`).
- Every ruling of one run is one ruling (`ruling_id`), which `revert_ruling`
  undoes together. Its verdicts stay on the statements, so they are not judged
  again.
- A random share of each run's acceptances (the policy's `judge_audit`, default
  0.1, and at least one) is marked `attributes.judge.audited: pending` for a
  person to confirm or overturn.

## Arguments

- `judgements` (required) — 1 to 100 verdicts, each `{id, verdict, confidence,
  rationale, verdict_run_id?}`. `verdict` is `accept`, `reject` or `unsure`;
  `confidence` is 0..1; `rationale` is one sentence (at most 500 characters);
  `verdict_run_id` is the run that judged it, from the sub-agent result header.
- `write` — `rule` (default) or `recommend` (record only).
- `scope` — `user` (default), `agent` or `tenant`.

## Returns

`{ruling_id?, judged, ruled, accepted, rejected, escalated, recommended,
refused, audit_marked, items: [{id, outcome, status?, error?, message?,
audit?}]}`. `outcome` is `ruled`, `below_floor`, `unsure`, `not_permitted`,
`recommend`, `not_routed`, `refused` or `ruling_failed`.

## Examples

```json
{"op": "judge_statements", "scope": "tenant", "judgements": [
  {"id": "7c2e9a1b3d4f4e6a8b0c2d4e6f8a1b3c", "verdict": "accept", "confidence": 0.93,
   "rationale": "The passage restates the cited article's reporting duty in more detail."}]}
```
