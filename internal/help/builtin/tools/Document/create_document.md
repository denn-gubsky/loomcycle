---
name: Document/create_document
description: "Document op=create_document — create a new document with an empty root chunk and name it in the Path tree; returns document_id and root_chunk_id."
---
`create_document` makes a new, empty document: a documents row plus a **root
chunk** that holds the title and has an empty body. It always names the
document in the Path tree, at your `path` or at `/documents/<title>`. Keep both
ids it returns: **`document_id` is what `create_chunk` needs, and
`root_chunk_id` is the `parent_id` for chunks that should sit directly under
the title.**

## Arguments

- `title` (required) — the document's title, and its root chunk's title.
- `path` — the Path-tree name, e.g. `/docs/launch-plan`. Default
  `/documents/<title>` with the title slugged to letters, digits, `.`, `_`, `-`.
  A path already in use is **taken over**: the old resource keeps existing but
  loses that name. Two documents with the same title therefore fight over one
  default path — pass an explicit `path` when titles may repeat.
- `scope` — `user` (default), `agent` or `tenant`.
- `type`, `status` — set on the root chunk and shown as the document's own type
  and status.
- `tags` — the document's tags (separate from any chunk's tags). Nested tags use
  a slash: `area/sub`.
- `subject` + `natural_key` (+ `type`) — make the root chunk itself a fact
  subject, so the document IS that subject and its facts are its children.
  `natural_key` must be unused in the scope. Leave these, and the fact fields
  below, out for an ordinary document.
- `class`, `confidence` — only with the `subject` + `natural_key` pair: the
  subject's class (`derived`, the default, or `evidential`) and a confidence
  between 0 and 1. Ignored without the pair.
- `valid_at`, `invalid_at`, `observed_at` — only with the pair: unix nanos when
  the subject's claim became true, stopped being true, and was said. Ignored
  without the pair.
- `source_quote` — only with the pair: the text span the subject was taken
  from. Ignored without the pair.
- `source_session_id` — only with the pair, rarely used: the chat the subject
  was distilled from, recorded as provenance. Ignored without the pair.

There is no `body`: a new root body is always empty. Write text with
`create_chunk`, or `update_chunk` the root (its revision is 1).

## Returns

`{document_id, root_chunk_id, title, path}`. If naming failed the document
still exists and you get `path_warning` instead of `path`; fix it with
`set_path`.

## Errors

- `create_document: missing required field: title`.
- `chunk <id> already holds the natural key "..."` — that subject already
  exists in this scope. Use the existing one (`list_facts`, `get_chunk`)
  instead of creating a second.
- `"<type>" is not an entity type this tenant declares ...` — only with the
  `subject` pair: use one of the declared types it lists, or drop
  `type`/`subject`.
- `Document: scope=user requires a user_id on the run` — pass `scope: "agent"`,
  or run with a user.
- `Document: scope=tenant is not granted to this agent ...` — the operator has
  not granted the tenant scope. Retrying is pointless; use `user` or `agent`.

## Examples

Create a document under a chosen path in the user's store:

```json
{"op": "create_document", "title": "Launch plan", "path": "/docs/launch-plan", "type": "plan", "status": "draft"}
```

```json result
{"document_id": "b4407b522dc11495d3de371311db17f0", "root_chunk_id": "9e1c3a7d2b5f4e6081a2c3d4e5f60718", "title": "Launch plan", "path": "/docs/launch-plan"}
```

A shared tenant document with tags (needs the tenant grants):

```json
{"op": "create_document", "title": "Support runbook", "scope": "tenant", "path": "/runbooks/support", "tags": ["ops/support", "reference"]}
```
