---
name: md-import
description: Import a Markdown file into a chunked-graph Document — deterministic round-trip vs semantic chunking.
tools: [Document]
license: Apache-2.0
---

# Markdown import

Two paths, pick by the input:

## 1. A loomcycle export (round-trip) → `import_md`
If the Markdown was produced by `export_md` — it contains `<!-- loom: {…} -->`
comments and maybe a `<!-- loom-edges: … -->` trailer — use the deterministic op
and let the runtime rebuild the structure:

- New document: `Document op=import_md markdown="<the file>"` (the first heading
  becomes the root; pass `path:` to also name it in the Path tree).
- Into an existing document: `op=import_md document_id=<id> markdown="…"`
  (optionally `parent_id=<chunk>` to graft it under a specific chunk).

`import_md` recreates the heading hierarchy, applies each chunk's type/status/
fields, and remaps the edge graph. Chunks get fresh ids.

## 2. Arbitrary prose / a plain `.md` → semantic chunking
If the Markdown has no `loom` metadata (a hand-written file, pasted notes), do
NOT use `import_md` blindly — its chunk boundaries are the heading lines, so a
body containing `##` lines would be re-chunked. Instead apply the
**semantic-chunking** skill: read the structure and `create_chunk` each piece
with a good title.

## After importing
Tell the operator the new `document_id` and how many chunks were created. They
can open it from the Path tree.
