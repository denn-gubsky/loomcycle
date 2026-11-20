---
name: Document/annotation_inputs
description: "Document op=annotation_inputs — what a section's or a document's annotation is written from, and the hash a write must match."
---
The material an annotating driver hands to a model, and the hash
`set_annotation` checks. The runtime computes both, so every driver hashes
the same way:

- a section (`branch`): its title, its document's title and identity, class,
  language, its own text, and each child's annotation — or, for a child with
  none, the child's text;
- a document (`document`): the same over the root chunk and its top-level
  sections, plus the document's asserted relations to other documents, by
  kind and title;
- a folder (`path`, with `path` the folder): each direct child's annotation,
  a document's or a sub-folder's, by name (the first 60, the rest grouped by
  class in `children_more`), the classes and topics of the documents it covers,
  and the kinds of relation among them. It also returns `covers`, the ids of
  every document below it. From `prompt_version` 3 on it adds
  `member_statuses` (the covered documents by stored status; not hashed),
  `exemplars` (up to three typical covered documents with their brief and
  abstract: of the commonest classes, one per sub-folder before a second from
  any) and `siblings` (the folders beside it that have an annotation, with
  their briefs; `siblings_more` counts those past ten). A sibling is named
  only when every member of the tenant may read the documents its annotation
  was written from, and those documents are added to what the folder's stored
  annotation is withheld on. Siblings are not hashed, so a sibling's rewrite
  makes nothing stale; only a sibling left out for access is. Below version 3
  the answer and its hash are unchanged;
- a node of a confirmed taxonomy (`scheme`, `concept`, or `class` for a
  document class the ontology declares): see "Taxonomy nodes" below.

## Taxonomy nodes

Every scheme, concept and declared document class has an annotation on its
chunk in the ontology document. `id` is the chunk `describe_ontology` gives a
scheme or concept; a class may be named with `name` instead. A standard class
with no entry in the ontology (the seed `Document`) is refused with
`standard_class`. The answer says which of three cases the node is in
(`basis`):

- `written` — at least `min_members` members (documents placed under it,
  narrower nodes included): the definition (`title`, `meaning`, `place`, the
  `scheme` header), `members`, `member_statuses` (how many members are done,
  confirmed, draft …; the brief must follow them; a stored status that is not
  one word is counted as `other`), `member_classes`, the
  `narrower` nodes with their own records, `exemplars` (from the stored
  `node_stats` row, else a sample), `siblings` and `terms`;
- `thin` — one or two members: the same, with those members' own briefs as
  `exemplars`;
- `declared` — no member: `composed` holds the brief and abstract the runtime
  wrote from the definition. Write it as it is; no model is needed.

A node's members are only the documents every member of the tenant may read:
one the tenant's access-control rules keep from some member is not counted,
not sampled, not in the class histogram, the statuses or the terms. A member
whose document annotation is metadata-only is counted and never an exemplar.

The hash covers the definition, the basis, the member set, the exemplars'
titles and annotation texts, the narrower records, the sibling names, the
terms, the class histogram, the language and the prompt version. Member
statuses are not hashed. A record written before members were narrowed this way
is stale. Node inputs are refused to a caller whose access-control rules hide
any document of the scope, as `node_stats` is.

`identity` says which work the document is: the root's natural key and
identity-like fields (`letter`, `code`, `work_key` …), and the fields its
class marks `@identity`, read from the root or, when the root does not carry
one, from the class's `@identifier_pattern` over the document title. A section
gets its document's, so a model summarising a section that names no code has
the document's own to write. Renaming a document makes its sections stale.

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
- `path` — the document's Path-tree name, instead of `document_id`; with
  `level: path`, the folder.
- `level` — `branch`, `document`, `path`, `scheme`, `concept` or `class`.
  Default: what the chunk is (a node level is always named).
- `name` — with `level: class`, the class instead of its chunk.
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
