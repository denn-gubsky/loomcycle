import type { ChunkRow } from "../types";

// chunkAncestors returns the chunks above `id`, root first and its direct parent
// last — the levels a reader can climb back up through from a selected chunk.
// It is empty for the root, for an id that is not loaded, and for an orphan whose
// parent is not in the list (there is nothing on screen to go up to).
//
// The walk stops on a repeated id, so a corrupt parent cycle cannot hang the
// viewer; it then returns the ancestors met before the repeat.
export function chunkAncestors(chunks: readonly ChunkRow[], id: string | undefined): ChunkRow[] {
  if (!id) return [];
  const byId = new Map(chunks.map((c) => [c.id, c]));
  const seen = new Set<string>([id]);
  const up: ChunkRow[] = [];
  let parentId = byId.get(id)?.parent_id;
  while (parentId && !seen.has(parentId)) {
    const parent = byId.get(parentId);
    if (!parent) break;
    seen.add(parentId);
    up.push(parent);
    parentId = parent.parent_id;
  }
  return up.reverse();
}
