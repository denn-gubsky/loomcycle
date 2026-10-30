---
name: Document/accept_statement
description: "Document op=accept_statement — an operator accepts a proposed relation, making it live."
---
`accept_statement` turns a proposed relation into an asserted one, or reverses
a rejection made in error. It is the operator's ruling and is refused inside an
agent run: a model cannot accept its own proposals. Operators reach it from the
review queue in the console, or from an operator session over the API.

An accepted relation is shown to every reader. If a judge had found it
unsupported, accepting overrules that verdict and records the operator as the
one who judged it; a supporting verdict is kept as it was.

## Arguments

- `id` (required) — the statement, from `list_statements`.
- `reason` — optional note recorded with the ruling.
- `scope` — `user` (default), `agent` or `tenant`.

## Returns

`{id, status: "asserted"}`.

## Errors

- Refused with a permission error inside a run.
- `no such statement` — check the id and the scope.
- `not_a_proposal` — the relation is already asserted.
- `statement_changed` — someone ruled on it while you did; reload it and rule
  again if it still needs it.

## Examples

```json
{"op": "accept_statement", "id": "7c2e9a1b3d4f4e6a8b0c2d4e6f8a1b3c"}
```
