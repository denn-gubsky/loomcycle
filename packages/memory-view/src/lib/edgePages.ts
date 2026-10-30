import type { DocEdge, MemoryScope } from "../types";
import type { MemoryDataLayer } from "./dataLayer";

// EDGE_PAGE_CAP bounds how many get_edges pages one listing follows. The runtime
// serves 500 edges a page, so a listing holds up to 10,000; a document with more
// says so rather than looking complete.
export const EDGE_PAGE_CAP = 20;

const SUPERSEDES = "supersedes";

type EdgeOpts = { scopeId?: string; kinds?: string[] };

// loadFactEdges collects the edges the fact inspector draws from the fact's
// document. get_edges pages at 500 ordered by kind, and a subject document can
// hold far more, so both loads follow next_cursor up to the cap. The
// supersession chain is fetched on its own by kind: it is what the inspector
// must not lose, and asking for one kind keeps it off the shared listing's cap.
// `incomplete` is true when either load stopped before the end.
export async function loadFactEdges(
  data: Pick<MemoryDataLayer, "getEdges">,
  scope: MemoryScope,
  documentId: string,
  opts?: { scopeId?: string },
  maxPages: number = EDGE_PAGE_CAP,
): Promise<{ edges: DocEdge[]; incomplete: boolean }> {
  const base: EdgeOpts = opts?.scopeId ? { scopeId: opts.scopeId } : {};
  const [chain, all] = await Promise.all([
    loadPages(data, scope, documentId, { ...base, kinds: [SUPERSEDES] }, maxPages),
    loadPages(data, scope, documentId, base, maxPages),
  ]);
  // Both loads return the chain's edges when the listing is whole; keep one.
  const seen = new Set<string>();
  const edges: DocEdge[] = [];
  for (const e of [...chain.edges, ...all.edges]) {
    const key = e.id ?? `${e.from_id}|${e.to_id}|${e.kind}`;
    if (seen.has(key)) continue;
    seen.add(key);
    edges.push(e);
  }
  return { edges, incomplete: chain.incomplete || all.incomplete };
}

// loadPages follows one get_edges listing. It stops early, reporting
// incomplete, at the cap, on a truncated page with no cursor, or when a data
// layer hands back the cursor it was given (one that ignores `after` would
// otherwise repeat its first page until the cap).
async function loadPages(
  data: Pick<MemoryDataLayer, "getEdges">,
  scope: MemoryScope,
  documentId: string,
  opts: EdgeOpts,
  maxPages: number,
): Promise<{ edges: DocEdge[]; incomplete: boolean }> {
  const edges: DocEdge[] = [];
  let after: string | undefined;
  for (let page = 0; page < maxPages; page++) {
    const r = await data.getEdges(scope, documentId, after ? { ...opts, after } : opts);
    if (after && r.next_cursor === after) return { edges, incomplete: true };
    edges.push(...(r.edges ?? []));
    if (!r.truncated) return { edges, incomplete: false };
    if (!r.next_cursor) return { edges, incomplete: true };
    after = r.next_cursor;
  }
  return { edges, incomplete: true };
}
