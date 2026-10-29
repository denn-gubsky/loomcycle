import type { BrowseScope, DocEdge, DocScope } from "../types";
import type { ExplorerDataLayer } from "./dataLayer";

// EDGE_PAGE_CAP bounds how many get_edges pages one document load follows. The
// runtime serves 500 edges a page, so the viewer holds up to 10,000 — far past
// what the References list or the graph can show usefully — and a document
// with more says so rather than looking complete.
export const EDGE_PAGE_CAP = 20;

// loadDocumentEdges collects every edge touching a document, following
// next_cursor until the runtime reports the list complete or the cap is hit.
// get_edges orders by kind, so an unfollowed cursor drops whole kinds, not a
// random sample. `incomplete` is true when the load stopped before the end: the
// cap, a page marked truncated with no cursor, or a data layer that hands back
// the cursor it was given (one that ignores `after` would otherwise repeat its
// first page until the cap).
export async function loadDocumentEdges(
  data: Pick<ExplorerDataLayer, "documentGetEdges">,
  documentId: string,
  scope: DocScope,
  browse?: BrowseScope,
  maxPages: number = EDGE_PAGE_CAP,
): Promise<{ edges: DocEdge[]; incomplete: boolean }> {
  const edges: DocEdge[] = [];
  let after: string | undefined;
  for (let page = 0; page < maxPages; page++) {
    const r = await data.documentGetEdges(documentId, scope, browse, after ? { after } : undefined);
    if (after && r.next_cursor === after) return { edges, incomplete: true };
    edges.push(...(r.edges ?? []));
    if (!r.truncated) return { edges, incomplete: false };
    if (!r.next_cursor) return { edges, incomplete: true };
    after = r.next_cursor;
  }
  return { edges, incomplete: true };
}
