---
name: Document/find_duplicates
description: "Document op=find_duplicates — find the copies in a set of documents, by the same text or the same title, each pointed at its original; a report unless propose is true."
---
`find_duplicates` looks through a set of documents for copies of one another
and says, for each copy, which document is its original. It uses no model:
two documents match when

- **sha256** — their text is the same. The op hashes the chunk bodies in
  document order, with every run of whitespace made one space. The titles
  and fields are not part of it, so a renamed copy still matches, and a
  document with no text matches nothing. Confidence 1.0.
- **title** — their titles are the same once case, surrounding spaces and copy
  markers are set aside, and they are the same kind of document: the same
  root `type` and, when both were imported, the same media type. The markers
  are a leading `Copy of `, `Копия `, `Копия - `, `Kopie von ` or `Copie de `,
  and a trailing ` (1)` to ` (99)` or ` - Copy`, as often as they repeat. A
  longer number, such as a year in ` (2023)`, is part of the title.
  Confidence 0.7.

A pair that matches both ways is reported once, as `sha256`.

**Which side is the copy.** In a title match where only one title carries a
copy marker, that one is the copy. Otherwise the earlier document is the
original: by its source's own update time (`source_updated_at` in the
provenance) when both documents have one, else by when it was created, then
by id. Three or more matching
documents make a star: every copy points at the one original, never at each
other.

**It is a report unless you pass `propose: true`.** Then each pair is filed as
a `duplicate_of` proposal from the copy's root chunk to the original's, with
`basis` in its attributes, through `propose_statement` — so it needs what a
proposal needs (the `propose` permission on both documents; in a tenant whose
ontology is confirmed with a Relations section, `duplicate_of` is a standard
kind and is accepted). Each side's evidence is its root chunk's title, quoted
with `part: "title"`, so every pair carries two evidence rows a reviewer or
judge can rule on; only a root with no title quotes the first line of its text
instead. Nothing is merged, hidden, deleted or renamed: a reviewer decides.

A pair already joined by a `duplicate_of` statement, in either direction and
whatever its status — proposed, asserted, rejected or expired — is skipped and
counted, in a report as well. A rejected one stays rejected.

## Arguments

- `scope` — `user` (default), `agent` or `tenant`.
- `document_ids` — the documents to compare, by id.
- `under_path` / `under_paths` — every document named at or under these
  paths. Combine freely; at least one document is required, and at most 5000
  per call.
- `basis` — `["sha256"]`, `["title"]` or both (the default).
- `propose` — `false` (default) reports; `true` files the proposals.
- `method` — `rule:<name>` or `extractor:<name>`, only for an agent granted
  that kind of statement method, as for `propose_statement`. Omit it and the
  proposals are your own.

A document you may not read is left out of the set, as if it did not exist.

## Returns

```
{pairs: [{copy_id, original_id, copy_root_id, original_root_id,
          copy_title, original_title, basis, confidence,
          statement_id?, status?, ontology_valid?, skipped?, detail?}],
 counts: {documents, pairs, sha256, title, proposed, skipped_existing, refused},
 truncated}
```

`copy_id` and `original_id` are document ids. `skipped` is `exists` for a pair
that already has a statement, or `refused:<category>` (with the refusal in
`detail`) when `propose_statement` refused it; the rest of the set goes ahead.
The list holds at most 1000 pairs (`truncated` says so); the counts stay exact
and every pair is still proposed.

## Errors

- `find_duplicates: no documents named ...` — pass `document_ids` or a path
  with documents under it.
- `find_duplicates: the set holds N documents, at most 5000 per call` — narrow
  `under_path`, or pass `document_ids` in batches.
- `find_duplicates: basis "x" is not sha256 or title`.
- `method_not_granted` — `method` was sent by an agent without that grant.

## Examples

Report the copies in a folder:

```json
{"op": "find_duplicates", "scope": "user", "under_path": "/notes"}
```

```json result
{"pairs": [{"copy_id": "6f1c0e2a9b7d4c3e8a5f1b2c3d4e5f60", "original_id": "1a2b3c4d5e6f708192a3b4c5d6e7f809",
            "copy_root_id": "9c8b7a6f5e4d3c2b1a0f9e8d7c6b5a49", "original_root_id": "2b3c4d5e6f7a8b9c0d1e2f3a4b5c6d7e",
            "copy_title": "Copy of Trip plan", "original_title": "Trip plan",
            "basis": "title", "confidence": 0.7}],
 "counts": {"documents": 12, "pairs": 1, "sha256": 0, "title": 1, "proposed": 0, "skipped_existing": 0, "refused": 0},
 "truncated": false}
```

File them for review, by content only:

```json
{"op": "find_duplicates", "scope": "user", "under_path": "/notes", "basis": ["sha256"], "propose": true}
```
