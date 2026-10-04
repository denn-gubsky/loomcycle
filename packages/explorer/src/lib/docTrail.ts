import type { ChunkRow, PathEntry } from "../types";
import type { DirMap } from "./lazyPathTree";

// The document trail: where the reader has been while following references from
// one document into another, so there is a way back.
//
// A reference between chunks can cross documents, and following one replaces the
// document in the viewer. Without a record of where the reader came from, the
// only way back is to find the original document again in the tree — and the
// chunk that was selected there is lost either way. The trail is that record. It
// is kept as plain data + pure functions (no React) so both hosts — the tree
// console and the single-document viewer — share one set of rules, and so those
// rules can be tested without a DOM.

// DocLocation is one stop on the trail: a document and the chunk selected in it.
// `path` is the document's Path-tree name when it has one and it is known — a
// document can exist without a dirent, and is then addressed by id alone.
export interface DocLocation {
  documentId: string;
  chunkId?: string;
  title?: string;
  path?: string;
}

// Trail is the visited documents, oldest first; the last entry is the one on
// screen. Empty = the document was opened directly (from the tree or by a host),
// and nothing has been followed yet.
export type Trail = readonly DocLocation[];

// followReference records a jump from `origin` into `target`. `origin` is the
// document being left, WITH the chunk selected at that moment — that is what
// Back has to restore, and it is only known at the moment of leaving. An empty
// trail is seeded with it; a trail already under way has its last entry (the
// same document) brought up to date instead of duplicated.
export function followReference(trail: Trail, origin: DocLocation, target: DocLocation): Trail {
  const last = trail[trail.length - 1];
  const head =
    last && last.documentId === origin.documentId
      ? [...trail.slice(0, -1), { ...last, ...defined(origin) }]
      : [...trail, origin];
  return [...head, target];
}

// jumpTo cuts the trail back to the entry at `index`, which becomes the document
// on screen (with its remembered chunk). An out-of-range index is a no-op.
export function jumpTo(trail: Trail, index: number): Trail {
  if (index < 0 || index >= trail.length - 1) return trail;
  return trail.slice(0, index + 1);
}

// back is jumpTo the previous entry.
export function back(trail: Trail): Trail {
  return jumpTo(trail, trail.length - 2);
}

// amendCurrent fills in what is learned about the document on screen AFTER the
// jump — its real title once it loads, its path once the lookup answers. It
// only applies while that document is still the one on screen, so a late answer
// for a document the reader has already left lands nowhere.
export function amendCurrent(
  trail: Trail,
  documentId: string,
  patch: Pick<DocLocation, "title" | "path">,
): Trail {
  const last = trail[trail.length - 1];
  if (!last || last.documentId !== documentId) return trail;
  const next = { ...last, ...defined(patch) };
  if (next.title === last.title && next.path === last.path) return trail;
  return [...trail.slice(0, -1), next];
}

// defined drops undefined values, so merging a partial location never erases a
// field the trail already knows (an origin snapshot carries no path of its own).
function defined<T extends object>(o: T): Partial<T> {
  return Object.fromEntries(Object.entries(o).filter(([, v]) => v !== undefined)) as Partial<T>;
}

// labelOf is what a crumb shows: the document's title, else its path's last
// segment, else a short id — never blank.
export function labelOf(loc: DocLocation): string {
  if (loc.title) return loc.title;
  if (loc.path) return loc.path.slice(loc.path.lastIndexOf("/") + 1) || loc.path;
  return loc.documentId.slice(0, 8);
}

// TrailItem is one rendered breadcrumb slot: an entry (by its index in the
// trail), or a gap standing in for the entries folded out of a long trail.
export type TrailItem = { kind: "crumb"; index: number } | { kind: "gap"; hidden: number[] };

// TRAIL_VISIBLE is how many crumbs a trail shows before it folds its middle.
export const TRAIL_VISIBLE = 4;

// collapseTrail folds a long trail to its first entry, a gap, and its last
// entries. The ends are what matter: where the reader started, and the steps
// that led directly here.
export function collapseTrail(length: number, max: number = TRAIL_VISIBLE): TrailItem[] {
  const all: TrailItem[] = Array.from({ length }, (_, index) => ({ kind: "crumb", index }));
  if (length <= max || max < 3) return all;
  const tail = max - 1; // crumbs kept at the end, after the first one
  const hidden = Array.from({ length: length - 1 - tail }, (_, i) => i + 1);
  return [all[0], { kind: "gap", hidden }, ...all.slice(length - tail)];
}

// findDocumentPath looks a document up among the directories the tree has
// already listed. Dirents point at documents (never the reverse), so this is the
// free half of the id → path lookup; a document in a directory nobody opened is
// not found here, and the data layer's documentPath is asked instead.
export function findDocumentPath(dirs: DirMap, documentId: string): string | undefined {
  for (const listing of dirs.values()) {
    const hit = listing.entries.find((e) => documentIdOf(e) === documentId);
    if (hit) return hit.full_path;
  }
  return undefined;
}

// documentIdOf reads the document id a `document` dirent points at.
export function documentIdOf(entry: PathEntry): string | undefined {
  if (entry.kind !== "document") return undefined;
  return (entry.resource_ref as { document_id?: string } | undefined)?.document_id || undefined;
}

// landingChunk picks the chunk a freshly loaded document opens on: the one asked
// for when it is among the loaded rows, else the root. A wanted chunk that is
// not there (deleted since the edge was written, or past the viewer's row cap)
// falls back to the root rather than selecting something the tree cannot show.
export function landingChunk(rows: ChunkRow[], wanted?: string): string | undefined {
  if (wanted && rows.some((c) => c.id === wanted)) return wanted;
  return rows.find((c) => !c.parent_id)?.id;
}
