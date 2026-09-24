---
name: Document/remember
description: "Document op=remember — store one sentence a person told you as a fact that cites itself; additive only, never a way to delete."
---
`remember` stores a statement a PERSON asked you to remember ("remember that I
take my coffee black"). The text is stored verbatim as the fact AND as its own
source quote, marked `evidential` (source material, never aged out). It lands
in the scope's `/memory/entities` document, created on first use. It only adds:
there is no "forget" here. **Write the fact itself as one self-contained
sentence ("Ada takes her coffee black."), not an instruction about it ("remember
the coffee thing").**

## Arguments

- `text` (required) — the statement, at most 1000 characters. Longer content
  belongs in a document (`create_document` + `create_chunk`).
- `type` + `subject` — optionally what kind of thing it is about and what:
  pass both or neither; the type must be one the tenant ontology declares.
- `scope` — `user` (default), `agent` or `tenant`.

The fact's key is derived from the first 60 or so characters of the text, so
remembering the same sentence twice updates one fact instead of adding a second
— and two sentences that start with the same 60 characters share one fact, the
second overwriting the first. It starts unjudged; a judge can
confirm it against its own text.

## Returns

`{id, natural_key, created}` — the fact's chunk id, its derived key
(`memory/operator/<slug>`), and whether it is new.

## Errors

- `remember: missing required field: text ...`.
- `remember: that is longer than a fact — store it as a document instead`.
- `upsert_chunk: "<type>" is not an entity type this tenant declares ...` —
  use a listed type, or drop `type` and `subject`.
- `Document: scope=user requires a user_id on the run` — pass
  `scope: "agent"`, or run with a user.

## Examples

Remember a preference the user stated:

```json
{"op": "remember", "text": "Ada takes her coffee black, no sugar."}
```

```json result
{"id": "5d8e2f1a0b9c4d3e7f6a5b4c3d2e1f00", "natural_key": "memory/operator/ada-takes-her-coffee-black-no-sugar", "created": true}
```

Remember a fact about a named subject, typed:

```json
{"op": "remember", "text": "Dave Kim is the floor manager at the Cluj shop.", "type": "person", "subject": "Dave Kim"}
```
