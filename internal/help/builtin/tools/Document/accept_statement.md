---
name: Document/accept_statement
description: "Document op=accept_statement — an operator accepts a proposed relation, making it live."
---
`accept_statement` turns a proposed relation into an asserted one. It is the
operator's ruling and is refused inside an agent run: a model cannot accept
its own proposals. Operators reach it from the review queue in the console,
or from an operator session over the API.

## Arguments

- `id` (required) — the statement, from `list_statements`.
- `reason` — optional note recorded with the ruling.
- `scope` — `user` (default), `agent` or `tenant`.

## Returns

`{id, status: "asserted"}`.

## Errors

- Refused with a permission error inside a run.
- `no such statement` — check the id and the scope.

## Examples

```json
{"op": "accept_statement", "id": "7c2e9a1b3d4f4e6a8b0c2d4e6f8a1b3c"}
```
