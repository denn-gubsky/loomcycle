---
name: Document/reject_statement
description: "Document op=reject_statement — an operator rejects a proposed relation, with a reason."
---
`reject_statement` marks a proposed relation rejected. It is kept, not deleted,
as a record that it was considered and refused: it is withheld from every
reader, and the same relation is refused if anyone proposes or links it again,
at any `valid_at`. Like `accept_statement`, it is the operator's ruling and is
refused inside an agent run.

It rules on proposals only. To withdraw a relation that is already asserted,
judge it unsupported with `judge_fact` (kept, withheld) or remove it with
`unlink_chunks`. A relation made by a `[[name]]` link in a chunk body is
removed by editing the link out of the body.

## Arguments

- `id` (required) — the statement.
- `reason` (required) — why it is wrong; shown to whoever proposes it again.
- `scope` — `user` (default), `agent` or `tenant`.

## Returns

`{id, status: "rejected"}`.

## Errors

- Refused with a permission error inside a run.
- `reason is required`.
- `not_a_proposal` — the relation is asserted or already rejected.
- `statement_changed` — someone ruled on it while you did; reload it.

## Examples

```json
{"op": "reject_statement", "id": "7c2e9a1b3d4f4e6a8b0c2d4e6f8a1b3c", "reason": "The quoted sentence cites the 2015 act, not this one."}
```
