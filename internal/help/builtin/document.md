---
name: document
description: Document — a chunked-graph document where each chunk is a first-class unit (UUID, hierarchy, type, fields, graph edges, Markdown body) that agents and humans co-author and query (RFC AK).
---
A `Document` is not a flat Markdown file — it's a **chunked graph**. Each
**chunk** is a first-class unit with a UUID, a place in a hierarchy, an optional
type, structured fields, a Markdown body, and graph edges to other chunks. You
restructure a document by moving/linking chunks, and you find things by
*querying* rather than scrolling.

## How it's stored

Two stores, split by access pattern (you don't manage this — it's automatic):
- **Memory** holds each chunk's **body + fields** (keyed by the chunk UUID).
- **SQL Memory** holds the **structure** — parent/position/type/status/title/
  revision, edges, type schemas — so you can run real queries over it.

Documents live in either the **agent** or **user** scope (tenant-scoped
documents aren't supported yet). Requires SQL Memory enabled on the server.

## Ops

**Documents:** `create_document` (`title`, optional `path:` to name it in the
Path tree), `get_document` (`id` or `path`), `delete_document`.

**Chunks:** `create_chunk` (`document_id`, optional `parent_id`/`type`/`status`/
`fields`/`position`, required `title`+`body`), `get_chunk`, `update_chunk`,
`delete_chunk` (cascades to descendants), `move_chunk` (`new_parent_id` +
`position`).

**Edges:** `link_chunks` / `unlink_chunks` (`from_id`, `to_id`, `kind` — e.g.
`promotes` / `targets` / `implements`).

**Query:** `query_chunks` — structured filters (`document_id` / `type` /
`status` / `parent_id` / `under_path:` to scope to a Path-tree subtree) **or** a
raw `sql:` SELECT against the chunk tables (validator-gated: read-only, no
`ATTACH`/`PRAGMA`/etc.). The structured query returns light rows (no bodies);
use `get_chunk` for a chunk's body.

**Types (supertag-like):** `define_type` (`name`, `fields`; omit `document_id`
for a scope-wide type), `list_types`.

**Markdown:** `export_md` (`document_id` or `id`/`path`) renders the document to
Markdown — each chunk a heading (level = depth) + its body. `include_metadata`
(default `true`) embeds round-trippable chunk metadata + edges as HTML comments;
`include_metadata:false` is clean human-facing Markdown. `import_md` is the
inverse: `markdown` (an export_md-shaped doc) builds a NEW document (omit
`document_id`; the first heading becomes the root) or imports under an existing
chunk (`document_id` + optional `parent_id`). Chunks get fresh ids; the edge
graph is remapped. Bodies with `##` heading lines re-chunk — do your own
semantic chunking (`create_chunk`) for arbitrary prose.

## Concurrency — `update_chunk` revision

Every chunk has a `revision`. `update_chunk` requires you to pass the current
`revision`; if it doesn't match, the update is rejected ("revision conflict") —
re-`get_chunk` and retry. This is how concurrent edits stay safe.

## Path-tree naming

`create_document(path:"/docs/launch")` registers the document in the Path tree
(RFC AL) so `Path resolve /docs/launch` finds it and `Document get_document
path:/docs/launch` opens it. `query_chunks under_path:/docs` restricts a query
to documents at or under that path. (See the `path` help topic.)

## Change events

Chunk mutations publish a `{op, chunk_id, timestamp, actor}` event to the
`documents/<id>/chunks` channel — how live viewers stay in sync without polling.
