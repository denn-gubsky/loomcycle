---
name: Document/get_edges
description: "Document op=get_edges — the relations touching a document's chunks, in or out, with their status, provenance, evidence and both endpoints."
---
`get_edges` lists the relations (statements) that start or end at any chunk of
one document, so you can see the document's link graph in one call. **It takes
`document_id`, not a chunk `id`.** For the links pointing at ONE chunk, use
`backlinks`; for one statement with all its evidence, use `get_edge`.

## Arguments

- `document_id` (required) — the document.
- `direction` — `out` (relations leaving the document), `in` (arriving) or
  `both` (default).
- `kind` / `kinds` — only these relation kinds, each with the narrower kinds
  the tenant declares under it (the response's `kind_expanded_to` lists them).
- `as_of` — only relations that held at this moment (unix nanoseconds); an
  undated relation always matches.
- `include_proposed` — include relations proposed for review (default true).
- `include_refuted` — also include rejected relations and ones judged
  unsupported (withheld by default, never deleted). A writer's low
  `confidence` never withholds a relation; only a verdict does.
- `include_retired` — also include retired relations.
- `limit` — page size (default 500, at most 5000).
- `after` — the `next_cursor` of the previous page.
- `scope` — `user` (default), `agent` or `tenant`. See
  `{"op":"help","topic":"scopes"}`.

## Returns

`{edges: [...]}`, ordered by kind, then age. Each edge carries `id`, `from_id`,
`to_id`, `kind`, `auto` (true = made by a `[[name]]` link in a body), `status`
(`asserted` or `proposed`; `rejected` only with `include_refuted`), `method`
(how it was made: `manual`, `rule:<name>`, `extractor:<name>`, `import`,
`sync`, `wikilink`, `runtime`, …), `created_at`, the
endpoints' `from_title` / `from_type` / `from_status` / `from_document_id` and
`to_…` twins, and when present `confidence` (the writer's own), `valid_at`,
`invalid_at`, `attributes`, `actor`, a verdict (`verdict_confidence`,
`judged_at`, `judge_reason`, `judged_by`), `operator_ruled_at` (an operator
accepted or rejected it), `verified_at` (when a maintainer last re-checked it),
`expired_at` (when it was retired; only with `include_retired`),
`discriminator`, `run_id`, `ontology_version` (the vocabulary it was written
under), and up to three `evidence` rows with
`evidence_count` — both counting only the evidence you may read. `truncated: true` with a
`next_cursor` means there is another page.

## Errors

- `get_edges: missing required field: document_id` — passing the id as `id`
  does not work here.
- `get_edges: direction must be out, in or both`.
- `get_edges: after is not a cursor this op returned` — pass the
  `next_cursor` of the previous page unchanged.
- An unknown document id is not an error: it returns `{edges: []}`. Check the
  id and the scope before concluding a document has no links.

## Examples

All links in and out of a document:

```json
{"op": "get_edges", "document_id": "b4407b522dc11495d3de371311db17f0"}
```

```json result
{"edges": [
  {"id": "4b1d9c2e7a3f4e5d8c6b0a1f2e3d4c5b", "from_id": "9a3e7c1f2b4d4a6e8c0f1a2b3c4d5e6f", "to_id": "c7d2e4f6a8b04c1d9e3f5a7b9c1d3e5f", "kind": "blocks", "auto": false,
   "status": "asserted", "method": "manual", "created_at": 1767225600000000000,
   "from_title": "Migrate the database", "from_document_id": "b4407b522dc11495d3de371311db17f0",
   "to_title": "Launch", "to_document_id": "b4407b522dc11495d3de371311db17f0"}]}
```

Only the citations leaving a document:

```json
{"op": "get_edges", "document_id": "b4407b522dc11495d3de371311db17f0", "direction": "out", "kind": "cites"}
```

What cites or implements a law's articles as of 1 January 2026, without the
proposals still in review:

```json
{"op": "get_edges", "scope": "tenant", "document_id": "0c1d2e3f4a5b4c6d8e9f0a1b2c3d4e5f", "direction": "in", "kinds": ["cites", "implements"], "as_of": 1767225600000000000, "include_proposed": false}
```
