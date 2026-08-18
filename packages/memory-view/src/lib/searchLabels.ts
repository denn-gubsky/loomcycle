import type { MemorySearchEntry } from "../types";

// searchLabels — the readable identity of a semantic-search hit.
//
// A document hit is addressed by an opaque `doc.chunk:<hex>` id, which tells a
// reader nothing about where the prose came from: a page of six of them cannot be
// attributed without opening each one. The server annotates those hits with the
// document's title and the chunk's own heading — best-effort, because the titles
// live in SQL Memory rather than beside the bodies — and this renders whichever of
// them arrived, falling back to the id that was always there.

// The two label fields are declared LOCALLY rather than read off MemorySearchEntry
// so the viewer keeps typechecking against an older published @loomcycle/client:
// they are additive on the wire and optional here. Fold this into the imported type
// once the package's peer floor includes them.
export type LabelledSearchEntry = MemorySearchEntry & {
  document?: string;
  title?: string;
};

const clean = (s: string | undefined): string => s?.trim() ?? "";

/** The hit's readable identity. Never empty: a document hit degrades to its chunk
 *  id and then to its key, so a missing label costs legibility and not the row. */
export function searchHitLabel(hit: LabelledSearchEntry): string {
  if (hit.kind !== "document") return hit.key;
  const doc = clean(hit.document);
  const heading = clean(hit.title);
  // A document's ROOT chunk carries the document's own title, so showing both
  // would read "Verified writes › Verified writes".
  if (doc && heading && doc !== heading) return `${doc} › ${heading}`;
  return heading || doc || hit.chunk_id || hit.key;
}
