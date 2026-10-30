---
name: Document/accept_classification
description: "Document op=accept_classification — an operator accepts a document's pending classification: the proposed class becomes its type, and its proposed statements or tags become live. Refused inside a run."
---
`accept_classification` is an operator's ruling on what `classify` proposed for
a document. It is refused inside a run: an agent may classify, but it never
accepts its own classification.

It applies, in one call:
- the proposed class, as the root chunk's type, with the identity fields its
  pattern filled. When another document already holds that identity, the class
  stays pending and `type_pending` is set;
- each proposed statement classify filed (tenant scope), as `accept_statement`
  would. One already ruled on is listed in `skipped_statements`;
- each proposed concept outside the tenant scope, as a tag on the root chunk.

To refuse a proposal, reject its statements with `reject_statement`, or set the
document's type yourself with `update_chunk`. `classify` never replaces a type
set by hand.

## Arguments

- `document_id` (or `id`, or `path`) — the document.
- `scope` — `user` (default), `agent` or `tenant`.

## Returns

`{document_id, type?, type_pending?, tags?, accepted_statements,
skipped_statements?, warnings?}`.

## Errors

- `accept_classification: only an operator accepts a classification` — called
  inside a run.
- `nothing_pending` — the document has no pending classification.
- `no such document` in this scope; a permission error when you may not write
  the document.

## Examples

```json
{"op": "accept_classification", "document_id": "b4407b522dc11495d3de371311db17f0", "scope": "tenant"}
```

```json result
{"document_id": "b4407b522dc11495d3de371311db17f0", "type": "Law", "accepted_statements": ["5e6f7a8b9c0d4e1f2a3b4c5d6e7f8a9b"]}
```
