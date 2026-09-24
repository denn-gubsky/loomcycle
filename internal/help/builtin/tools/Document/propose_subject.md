---
name: Document/propose_subject
description: "Document op=propose_subject — suggest that a subject (a person, place, thing) become a shared tenant subject; inert until an operator adopts it."
---
`propose_subject` files a suggestion that a subject you learned about in one
scope become a shared subject of the whole tenant. Like `propose_entity`, the
proposal is **inert** until an operator adopts it; facts about the subject stay
where they are. It needs no tenant grant, but `scope` is still checked first, so
pass one your run can open (default `user`, or `agent`). **The `natural_key` you
pass is used verbatim when the subject is adopted**, so use the same key your
facts already point at.

## Arguments

- `subject` (required) — the subject's name, e.g. `Dave Kim`. `title` is
  accepted in its place.
- `natural_key` (required) — its identity, `<type>:<slug>`, e.g.
  `person:dave-kim`. The part before `:` becomes its entity type.
- `path` — the name segment the adopted subject gets under `/facts/`, e.g.
  `dave-kim` (a single segment, not a full path). Default: the key after its
  `<type>:` prefix.
- `body` — evidence for the operator, at most 4000 bytes.
- `scope` — see above.

## Returns

`{proposed, chunk_id, natural_key, note}`. If this subject was already
proposed you get `{proposed, already: "<status>", note}` instead — not an
error, and nothing new is filed.

## Errors

- `propose_subject: subject is required ...`.
- `propose_subject: natural_key is required ...`.
- `propose_subject: body is N bytes, over the 4000-byte limit`.
- `propose_subject: this tenant has no ontology document yet ...` — an operator
  must open the ontology once. Retrying is pointless.

## Examples

Propose a person the user keeps mentioning:

```json
{"op": "propose_subject", "subject": "Dave Kim", "natural_key": "person:dave-kim", "body": "Named in 6 conversations as the shop's floor manager."}
```

```json result
{"proposed": "Dave Kim", "chunk_id": "0f1e2d3c4b5a69788796a5b4c3d2e1f0", "natural_key": "person:dave-kim", "note": "Filed as a suggestion, not in force. ..."}
```

Propose a place with an explicit path slug:

```json
{"op": "propose_subject", "subject": "Cluj-Napoca office", "natural_key": "place:cluj-office", "path": "cluj-office", "scope": "agent"}
```
