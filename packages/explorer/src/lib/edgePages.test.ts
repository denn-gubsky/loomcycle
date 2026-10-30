import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { describe, it, expect, vi } from "vitest";
import type { DocEdge } from "../types";
import type { DocEdgesPage, ExplorerDataLayer } from "./dataLayer";
import { EDGE_PAGE_CAP, loadDocumentEdges } from "./edgePages";
import CrossReferences from "../components/CrossReferences";

const edge = (id: string, kind = "cites"): DocEdge => ({ id, from_id: "a", to_id: "b", kind, auto: false });

// pagedLayer serves `pages` in order, keyed by the cursor the previous page
// handed out, the way the runtime's get_edges does.
function pagedLayer(pages: DocEdgesPage[]) {
  const documentGetEdges = vi.fn(
    async (_doc: string, _scope: string, _browse?: unknown, page?: { after?: string }) => {
      const i = page?.after ? Number(page.after.slice(1)) : 0;
      return pages[i];
    },
  );
  return { documentGetEdges } as unknown as Pick<ExplorerDataLayer, "documentGetEdges"> & {
    documentGetEdges: typeof documentGetEdges;
  };
}

describe("loadDocumentEdges", () => {
  it("follows next_cursor to the edge on the second page", async () => {
    const dl = pagedLayer([
      { edges: [edge("e1")], truncated: true, next_cursor: "c1" },
      { edges: [edge("e2", "zz_late")] },
    ]);
    const got = await loadDocumentEdges(dl, "doc1", "user", { scopeId: "u1" });
    expect(got.edges.map((e) => e.id)).toEqual(["e1", "e2"]);
    expect(got.incomplete).toBe(false);
    expect(dl.documentGetEdges).toHaveBeenNthCalledWith(1, "doc1", "user", { scopeId: "u1" }, undefined);
    expect(dl.documentGetEdges).toHaveBeenNthCalledWith(2, "doc1", "user", { scopeId: "u1" }, { after: "c1" });
  });

  it("stops at the page cap and reports the list incomplete", async () => {
    const pages: DocEdgesPage[] = Array.from({ length: EDGE_PAGE_CAP + 5 }, (_, i) => ({
      edges: [edge(`e${i}`)],
      truncated: true,
      next_cursor: `c${i + 1}`,
    }));
    const dl = pagedLayer(pages);
    const got = await loadDocumentEdges(dl, "doc1", "user");
    expect(dl.documentGetEdges).toHaveBeenCalledTimes(EDGE_PAGE_CAP);
    expect(got.edges).toHaveLength(EDGE_PAGE_CAP);
    expect(got.incomplete).toBe(true);
  });

  it("a layer that ignores the cursor is neither looped nor double-counted", async () => {
    const documentGetEdges = vi.fn().mockResolvedValue({ edges: [edge("e1")], truncated: true, next_cursor: "same" });
    const got = await loadDocumentEdges({ documentGetEdges }, "doc1", "user");
    expect(documentGetEdges).toHaveBeenCalledTimes(2);
    expect(got.edges.map((e) => e.id)).toEqual(["e1"]);
    expect(got.incomplete).toBe(true);
  });

  it("truncated without a cursor is incomplete, a page without truncated is complete", async () => {
    const cut = await loadDocumentEdges(pagedLayer([{ edges: [edge("e1")], truncated: true }]), "d", "user");
    expect(cut.incomplete).toBe(true);
    const whole = await loadDocumentEdges(pagedLayer([{ edges: [edge("e1")] }]), "d", "user");
    expect(whole.incomplete).toBe(false);
  });
});

describe("CrossReferences — an incomplete edge list", () => {
  const props = { edges: [edge("e1")], documentId: "doc1", onSelectChunk: () => {} };

  it("says the list stopped before the end", () => {
    const html = renderToStaticMarkup(createElement(CrossReferences, { ...props, incomplete: true }));
    expect(html).toContain("doc-refs-incomplete");
  });

  it("says nothing when the list is whole", () => {
    const html = renderToStaticMarkup(createElement(CrossReferences, props));
    expect(html).not.toContain("doc-refs-incomplete");
  });
});
