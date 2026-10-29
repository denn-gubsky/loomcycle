---
name: Document/reject_statement
description: "Document op=reject_statement — an operator rejects a proposed (or asserted) relation, with a reason."
---
`reject_statement` marks a relation rejected. It is kept, not deleted, as a
record that it was considered and refused: it is withheld from every reader,
and the same relation is refused if anyone proposes or links it again. Like
`accept_statement`, it is the operator's ruling and is refused inside an agent
run. To remove a link outright, use `unlink_chunks`.

## Arguments

- `id` (required) — the statement.
- `reason` (required) — why it is wrong; shown to whoever proposes it again.
- `scope` — `user` (default), `agent` or `tenant`.

## Returns

`{id, status: "rejected"}`.

## Errors

- Refused with a permission error inside a run.
- `reason is required`.

## Examples

```json
{"op": "reject_statement", "id": "7c2e9a1b3d4f4e6a8b0c2d4e6f8a1b3c", "reason": "The quoted sentence cites the 2015 act, not this one."}
```
