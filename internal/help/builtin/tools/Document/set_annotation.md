---
name: Document/set_annotation
description: "Document op=set_annotation — write the brief and abstract of a section or a document in the tenant knowledge base."
---
An annotation is two short texts about one part of a document: a **brief**,
one sentence saying what the part is so a reader can choose it, and an
**abstract**, one paragraph saying what it states so a reader need not open
it. A section (a chunk with parts of its own, or a long one) carries a
`branch` annotation; a whole document carries a `document` annotation on its
root chunk; a Path folder carries a `path` annotation, kept on a document named
`<folder>/_annotation` (type `Annotation`, made on the first write) or, when the
folder is itself a document, on that document's root.

An annotation is not content. It is kept beside the document, so writing one
changes nothing the document says: not its text, its history, its export or
its sync.

Each annotation records the hash of what it was written from. Read the inputs
with `annotation_inputs`, write the annotation from them, and pass back the
`inputs_sha256` it returned. If the text moved in between, the write is
refused and you read again. Once written, the annotation reads `stale: true`
whenever its inputs change, until it is written again.

Only the operator, or a code agent granted `statement_methods: [annotate]`,
writes annotations, and only in the tenant scope. The operator may write one
by hand with `manual: true` and no hash; a manual annotation is never
overwritten by a driver.

## Arguments

- `id` — the chunk: a section, or a document's root chunk.
- `document_id` — a document, for its `document` annotation.
- `path` — the document's Path-tree name, instead of `document_id`; with
  `level: path`, the folder, e.g. `/kb/policies` (`/` for the top).
- `level` — `branch`, `document` or `path`. Default: what the chunk is.
- `brief` (required) — at most 240 characters.
- `abstract` (required) — at most 2000 characters.
- `inputs_sha256` — the hash from `annotation_inputs`. Required unless
  `manual` is true.
- `prompt_version` — the version of the prompt it was written with; must be
  the one passed to `annotation_inputs`.
- `language` — a two- or three-letter code. Default: the document's language.
- `model` — the model that wrote it.
- `manual` — an operator's own text.
- `scope` — `tenant`.

## Returns

`{"annotation": {chunk_id, document_id, level, brief, abstract, language,
class?, inputs_sha256, prompt_version, model?, manual, written_at, written_by,
stale}}`.

## Errors

- `set_annotation: annotation_inputs_changed: …` — the text changed since the
  inputs were read. Read them again.
- `set_annotation: manual_record: …` — an operator wrote this annotation.
- `set_annotation: missing required field: inputs_sha256` — pass the hash, or
  `manual: true` as the operator.
- `set_annotation: brief is N characters, at most 240` (and the same for the
  abstract) — shorten it.
- `set_annotation: level document is written on a document's root chunk; …` —
  pass `level: branch` for a section.
- `set_annotation: annotations are kept for the tenant knowledge base only` —
  pass `scope: tenant`.
- `set_annotation: only the operator or an annotating driver writes
  annotations`.

## Examples

```json
{"op": "set_annotation", "scope": "tenant", "id": "5c409a5230ada95068c0efda767832ba", "brief": "Unpacking and connecting the widget before first use.", "abstract": "Setup has two steps: unpack the widget and remove the transport lock, then connect it to the controller with the supplied cable.", "inputs_sha256": "9b1e4f6c2a7d8e3f0b5c1a9d7e2f4b6c8a0d3e5f7b9c1d2e4f6a8b0c2d4e6f8a", "prompt_version": 1, "model": "local-medium"}
```

```json result
{"annotation": {"chunk_id": "5c409a5230ada95068c0efda767832ba", "document_id": "71c8d1ce942bf85e3f49a4f00a5b7ff6", "level": "branch", "brief": "Unpacking and connecting the widget before first use.", "abstract": "Setup has two steps: unpack the widget and remove the transport lock, then connect it to the controller with the supplied cable.", "language": "en", "class": "Guide", "inputs_sha256": "9b1e4f6c2a7d8e3f0b5c1a9d7e2f4b6c8a0d3e5f7b9c1d2e4f6a8b0c2d4e6f8a", "prompt_version": 1, "model": "local-medium", "manual": false, "written_at": "2026-11-09T22:40:00Z", "written_by": "run:9f2c", "stale": false}}
```
