---
name: doc/restructuring
description: Safely move, reparent, reorder, promote, or delete chunks in a chunked-graph Document.
tools: [Document]
license: Apache-2.0
---

# Restructuring

Reshaping the tree — "promote the risks section to a top-level chunk", "move the
deployment steps under Runbooks", "reorder these" — is your job (the Web UI
deliberately has no drag-edit). Be careful: these operations cascade.

## Method
1. Map the current shape first: `query_chunks document_id=<id>` to see parents
   and positions before moving anything.
2. Reparent / reorder with `Document op=move_chunk id=<chunk> new_parent_id=<p>`
   (and an optional `position`). Moving a chunk takes its whole subtree with it.
   A move into a chunk's own subtree is refused (it would orphan the tree).
3. Promote = move to a shallower parent (e.g. `new_parent_id` = the document
   root). Demote = move under a sibling.
4. Delete with `op=delete_chunk id=<chunk>` — this **cascades** to all
   descendants and returns the count. You cannot delete a document's root chunk.

## Safety
- For any cascade touching more than a couple of chunks, summarize the effect in
  one sentence and get the operator's confirmation BEFORE acting
  ("Deleting Section 3 removes it and its 4 sub-chunks — proceed?").
- Re-read after a move (`query_chunks`) if you're chaining further changes — ids
  are stable but positions shift.
