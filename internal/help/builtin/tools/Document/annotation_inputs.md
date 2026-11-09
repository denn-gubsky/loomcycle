---
name: Document/annotation_inputs
description: "Document op=annotation_inputs — what a section's or a document's annotation is written from, and the hash a write must match."
---
The material an annotating driver hands to a model, and the hash
`set_annotation` checks. The runtime computes both, so every driver hashes
the same way:

- a section (`branch`): its title, class, language, its own text, and each
  child's annotation — or, for a child with none, the child's text;
- a document (`document`): the same over the root chunk and its top-level
  sections, plus the root's identity fields and the document's asserted
  relations to other documents, by kind and title.

A change anywhere below changes the hash only through the children, so a
section goes stale when one of its parts changed and its sibling sections do
not. `above` is the annotation one level up (the parent section's abstract,
else the document's brief), given so a model does not repeat it; it is not
part of the hash.

Only the operator, or a code agent granted `statement_methods: [annotate]`,
reads annotation inputs.

## Arguments

- `id` — the chunk: a section, or a document's root chunk.
- `document_id` — a document, for its `document` annotation.
- `path` — the document's Path-tree name, instead of `document_id`.
- `level` — `branch` or `document`. Default: what the chunk is.
- `prompt_version` — the version of the prompt the annotation will be written
  with. It is part of the hash, so a new prompt makes earlier annotations
  stale.
- `body_chars` — how much of the target's own text to return (default 6000,
  at most 20000). The hash always covers the whole text.
- `scope` — `tenant`.

## Returns

`{chunk_id, document_id, level, class?, title, document_title, place, path?,
identity?, body, body_chars, body_truncated?, children: [{chunk_id, title,
type?, has_children?, body_chars, brief?, abstract?, body?,
body_truncated?}], relations?: [{kind, count, titles}], above?, language,
prompt_version, inputs_sha256, stale, record?}`. `stale` is true when there is
no annotation yet or the stored one was written from other inputs; `record`
is the stored annotation.

## Errors

- `annotation_inputs: only the operator or an annotating driver reads
  annotation inputs`.
- `annotation_inputs: annotations are kept for the tenant knowledge base only`.

## Examples

```json
{"op": "annotation_inputs", "scope": "tenant", "id": "5c409a5230ada95068c0efda767832ba", "prompt_version": 1}
```

```json result
{"chunk_id": "5c409a5230ada95068c0efda767832ba", "document_id": "71c8d1ce942bf85e3f49a4f00a5b7ff6", "level": "branch", "class": "Guide", "title": "Setup", "document_title": "Widget guide", "place": ["Widget guide"], "path": "/kb/widgets/guide", "body": "Before you start, read the safety notes.", "body_chars": 40, "children": [{"chunk_id": "8a57ee2aedc8add35bd3e6a21e35ae43", "title": "Step one", "body_chars": 29, "body": "Unpack the alpha widget box."}], "language": "en", "prompt_version": 1, "inputs_sha256": "9b1e…", "stale": true}
```
