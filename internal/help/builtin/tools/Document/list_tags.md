---
name: Document/list_tags
description: "Document op=list_tags — the distinct tags, with counts, on one chunk, one document, or across the whole scope."
---
`list_tags` shows which tags exist. Pass a chunk `id` or a `document_id` for
that one target's tags, or neither to see every tag in the scope with how often
it is used. Use the scope-wide listing before you tag something, so you reuse
an existing spelling instead of inventing a near-duplicate.

## Arguments

- `id` — a chunk: its tags.
- `document_id` — a document: the document's own tags (not its chunks' tags).
- Neither — every tag in the scope, counted across chunks AND documents.
- `scope` — `user` (default), `agent` or `tenant`. See
  `{"op":"help","topic":"scopes"}`.

## Returns

`{tags: [{tag, count}]}`, sorted by tag. For a single chunk or document each
count is 1.

## Errors

- An unknown id returns `{tags: []}`, not an error — check the id and scope.

## Examples

The tag vocabulary of the whole user store:

```json
{"op": "list_tags"}
```

```json result
{"tags": [{"tag": "billing", "count": 4}, {"tag": "billing/invoices", "count": 2}, {"tag": "runbook", "count": 1}]}
```

The tags on one document in the agent's own store:

```json
{"op": "list_tags", "document_id": "b4407b522dc11495d3de371311db17f0", "scope": "agent"}
```
