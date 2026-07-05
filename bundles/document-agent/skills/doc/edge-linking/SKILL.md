---
name: doc/edge-linking
description: Create and curate graph edges between chunks (and across documents) in a chunked-graph Document.
tools: [Document]
license: Apache-2.0
---

# Edge linking

Chunks form a tree by `parent_id`, but they also carry a free-form **graph** of
typed edges (`link_chunks`) — "this publication promotes that blog post", "this
task targets that requirement". Use edges for cross-cutting relations that the
parent/child hierarchy can't express.

## Method
1. Resolve both endpoints first with `query_chunks` / `get_chunk` — both chunks
   must already exist (an edge to a missing chunk is refused). Cross-document
   edges are allowed as long as both chunks are in this scope.
2. Pick a clear `kind` verb the operator will recognize: `references`,
   `promotes`, `targets`, `depends_on`, `supersedes`. Reuse existing kinds in
   the document rather than inventing near-duplicates.
3. Link: `Document op=link_chunks from_id=<a> to_id=<b> kind="references"`
   (idempotent — re-linking the same triple is a no-op).
4. Remove a stale edge with `op=unlink_chunks from_id=<a> to_id=<b> kind="…"`.

## Notes
- Edges are directional (`from → to`). State the direction back to the operator.
- Don't use an edge where a parent/child move is what's meant — see the
  doc/restructuring skill.
