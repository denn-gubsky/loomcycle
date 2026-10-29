---
name: Document/propose_entity
description: "Document op=propose_entity — suggest a new entity type, or a relation kind, for the tenant ontology; it stays inert until an operator accepts it."
---
`propose_entity` files a suggestion that the tenant ontology gain a new entity
type (or a subtype of an existing one) — or, with `section: relations`, a new
relation kind: a name for how two things are related. The proposal is **inert**: no run sees
it and nothing changes until an operator accepts it in the console. It needs no
tenant grant and always goes to your own tenant's ontology, whatever `scope`
you pass — but **`scope` is still checked first**, so it must be a scope your
run can open (the default `user` needs a user id; otherwise pass `agent`).

## Arguments

- `name` (required) — the proposed type's name, e.g. `vehicle`. `title` is
  accepted in its place.
- `parent` — an entity type that is already IN FORCE, by name, that this one is
  a kind of. Omit for a new top-level type. With `section: relations`, a
  relation kind the tenant declares, that this one is a narrower kind of.
- `section` — `classes` (the default) for an entity type, `relations` for a
  relation kind. A relation kind can only be proposed where the tenant's
  ontology declares relation kinds at all.
- `body` — your evidence: counts and a few example facts, at most 4000 bytes.
- `scope` — see above.

## Returns

`{proposed, chunk_id, parent_id, note}` — `note` says it is filed and not in
force.

## Errors

- `propose_entity: name is required ...`.
- `propose_entity: body is N bytes, over the 4000-byte limit ...` — shorten
  the evidence.
- `propose_entity: this tenant has no ontology document yet ...` — an operator
  must open the ontology once. Retrying is pointless.
- `propose_entity: <name> is already in force ...` — propose a subtype of it
  instead, or use it as it is.
- `propose_entity: <name> is already proposed` / `... rejected` — an operator
  has seen it. Do not file it again.
- `propose_entity: no entity named <parent> is in force ... (have: ...)` — pick
  a parent from the list it gives.
- `propose_entity: this tenant's ontology declares no Relations section ...` —
  relation kinds are not used here; describe the relation in prose.
- `propose_entity: <name> is a standard relation kind and already valid` — use
  it as it is.

## Examples

Propose a subtype of an existing type, with evidence:

```json
{"op": "propose_entity", "name": "vehicle", "parent": "object", "body": "14 facts mention cars, vans or bikes the user owns, e.g. 'Ada drives a 2019 Škoda Octavia'."}
```

```json result
{"proposed": "vehicle", "chunk_id": "7a6b5c4d3e2f10a9b8c7d6e5f4a3b2c1", "parent_id": "2c3d4e5f6a7b8c9d0e1f2a3b4c5d6e7f", "note": "Filed as a suggestion, not in force. ..."}
```

A relation kind, narrower than one the tenant already declares:

```json
{"op": "propose_entity", "section": "relations", "name": "supervises", "parent": "manages", "body": "6 facts say one person supervises another's thesis."}
```

A top-level type from an agent with no user on its run:

```json
{"op": "propose_entity", "name": "medication", "scope": "agent", "body": "9 facts name prescriptions and dosages."}
```
