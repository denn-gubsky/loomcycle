---
name: Memory/placement
description: "Memory op=placement — ask, for a batch of facts you are about to store, which scope each belongs in (your own or the tenant's); writes nothing."
---
`placement` answers **where** facts should be stored before you store them.
Pass the `{type, subject}` of each fact and the scope you were going to use;
each answer tells you the scope to write, whether it differs from yours, and
why. It writes nothing — you then write to the scope it names, with your
ordinary grants. **Ask once per batch, before writing**, so every half of a
fact lands in the same scope.

The answer comes from the operator's declarations on the tenant ontology, not
from you. Anything the declarations do not settle — an undeclared or unknown
type, a subject that is the run's own user, a subject recorded under
conflicting types, a subject the tenant does not know yet, or a scope you are
not granted — answers with the scope you passed in. A `tenant` answer needs the `tenant` grant on BOTH `memory_scopes`
and `sql_scopes`.

## Arguments

- `scope` (required) — the scope you intended to write, usually `user`.
- `items` (required) — 1 to 200 `{type, subject}` pairs, e.g.
  `{"type": "service", "subject": "checkout-api"}`.

## Returns

`{placements, moved, caller_scope, granted_scopes, granted_sql_scopes}`.
Each placement is `{type, subject, scope, moved, reason}` and sometimes an
`advisory`. `scope` is where to write; `moved: true` means it differs from
yours; `reason` says why. `moved` at the top counts the moved items.

## Errors

- `placement: items is required ...` — pass at least one pair.
- `placement: 250 items, over the 200 limit — split the batch`.
- `Memory tool: scope "tenant" not in this agent's memory_scopes [...]` — the
  scope you pass must itself be granted.

A `reason` such as "this tenant has no ontology document" or "SQL Memory is
not enabled" is not an error: every item stays in your scope.

## Examples

Before writing three facts from a consolidation pass:

```json
{"op": "placement", "scope": "user", "items": [{"type": "service", "subject": "checkout-api"}, {"type": "preference", "subject": "coffee"}, {"type": "person", "subject": "Dana Whitfield"}]}
```

```json result
{"placements": [
  {"type": "service", "subject": "checkout-api", "scope": "tenant", "moved": true, "reason": "service declares tenant scope"},
  {"type": "preference", "subject": "coffee", "scope": "user", "moved": false, "reason": "preference declares no memory scope"},
  {"type": "person", "subject": "Dana Whitfield", "scope": "user", "moved": false, "reason": "person declares user scope, which is where this was already going"}],
 "moved": 1, "caller_scope": "user", "granted_scopes": ["user", "tenant"], "granted_sql_scopes": ["user", "tenant"]}
```

A single fact:

```json
{"op": "placement", "scope": "user", "items": [{"type": "team", "subject": "payments"}]}
```
