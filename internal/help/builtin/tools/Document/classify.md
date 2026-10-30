---
name: Document/classify
description: "Document op=classify — say what a document is from the confirmed ontology: its class and work key by the classes' patterns, its issuer, and its neighbours' classes and concepts. One class matching by rule is applied; the rest is proposed."
---
`classify` says what a document is, from the tenant's confirmed ontology and
without a model. Two steps, each reported in `steps`:

1. **Rules.** Each document class's `@title_pattern` is matched against the
   document's title, its import source title and its first section titles. A
   class with only an `@identifier_pattern` is matched against the title. The
   pattern's named groups fill the class's fields, so a class with `@identity`
   fields gets its work key. When a class and its own parent class both match,
   the more specific class counts. An **issuer** is an instance from the
   ontology's Instances section whose name or `@alt` label appears as a whole
   phrase in the title or the first sections.
2. **Neighbourhood vote.** The documents most like this one's first section
   (`related`), in the same Path folder, that you may read and that already
   carry a declared class. It reports how many of them agree on a class, and
   which concepts at least half of them share. It needs a configured embedder;
   without one this step abstains and says so.

A draft ontology classifies nothing.

## What is written

With `write: "propose"` (the default):
- **Exactly one class matched by rule:** it is applied. The root chunk's type
  becomes the class and the identity fields the pattern filled are set, so the
  work key is recorded. When exactly one adopted instance is named as issuer, an
  `issued_by` statement is asserted.
- **Anything else** (several classes matched, or only the vote decided) is a
  proposal. It waits in the root's `classification` field until an operator
  runs `accept_classification`.
- **Concepts** from the vote, and an issuer beside a proposed class, are always
  proposals.
- **In the tenant scope**, proposals are filed as proposed statements (`subject`,
  `classified_as`, `issued_by`) from the root chunk, so they also appear in
  `list_statements`. In a user or agent scope no statement can reach a concept
  or a subject. There a proposed concept waits as its tag and becomes the root's
  tag on acceptance, and an issuer is reported only.
- **A type set by hand is never replaced.** The result is `kept`, and the class
  classify would choose is reported.
- **An identity another document already holds** stops the class from being
  applied; it is proposed instead, with the reason in `warnings`.

With `write: "report"` nothing is written; the answer is the same.

## Arguments

- `document_id` (or `id`, or `path`) — the document.
- `write` — `propose` (default) or `report`.
- `scope` — `user` (default), `agent` or `tenant`.

## Returns

`{document_id, write, result, class?, live, work_key?, fields?, issued_by,
concepts, steps, statements?, warnings?}`:

- `result` — `classified` (a class was applied), `proposed`, `kept` (a type set
  by hand stays) or `unclassified`.
- `live` — whether the class was applied rather than proposed.
- `fields` — the identity fields the pattern filled.
- `issued_by` — each `{instance, quote, subject_chunk_id?}`.
  `subject_chunk_id` is set only in the tenant scope, for an adopted instance.
- `concepts` — each `{concept, tag, kind?, chunk_id?, share}`.
- `steps` — `[{step: "rules"|"vote", candidates, chosen?, neighbours?,
  reason?}]`. A rule candidate carries the `quote` it matched and where
  (`from`), a vote candidate its `votes` and `share`.
- `statements` — the statements written, each `{id, status, created, ...}`.

## Errors

- `classify: write must be propose or report`.
- `classify: the ontology document is not classified`.
- `no such document` in this scope.
- A permission error when you may not read the document, or, with
  `write: "propose"`, may not write it. A statement the access-control rules do
  not allow is skipped with a warning.

## Examples

```json
{"op": "classify", "document_id": "b4407b522dc11495d3de371311db17f0", "scope": "tenant"}
```

```json result
{"document_id": "b4407b522dc11495d3de371311db17f0", "write": "propose", "result": "classified", "class": "Law", "live": true, "work_key": "law:2297-vi", "fields": {"number": "2297-VI"}, "issued_by": [{"instance": "Verkhovna Rada", "quote": "Верховна Рада", "subject_chunk_id": "7d1e2f3a4b5c4d6e8f9a0b1c2d3e4f5a"}], "concepts": [], "steps": [{"step": "rules", "candidates": [{"class": "Law", "quote": "Закон України № 2297-VI", "from": "title", "groups": {"number": "2297-VI"}}], "chosen": "Law"}, {"step": "vote", "candidates": [], "reason": "no neighbouring document is classified yet"}], "statements": [{"id": "5e6f7a8b9c0d4e1f2a3b4c5d6e7f8a9b", "status": "asserted", "created": true, "evidence_written": 0, "ontology_valid": true}]}
```
