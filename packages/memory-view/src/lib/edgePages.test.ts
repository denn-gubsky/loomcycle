import { describe, it, expect, vi } from "vitest";
import type { DocEdge, DocEdgesResponse } from "../types";
import type { MemoryDataLayer } from "./dataLayer";
import { EDGE_PAGE_CAP, loadFactEdges } from "./edgePages";

const edge = (from: string, to: string, kind: string): DocEdge => ({ from_id: from, to_id: to, kind, auto: false });

type GetEdges = MemoryDataLayer["getEdges"];

// layer answers get_edges like the runtime: an unfiltered call is served from
// `all` page by page (cursor "cN" = page N), a `kinds` call from `byKind`.
function layer(all: DocEdgesResponse[], byKind: Record<string, DocEdge[]> = {}) {
  const getEdges = vi.fn<GetEdges>(async (_scope, _doc, opts) => {
    if (opts?.kinds) return { edges: opts.kinds.flatMap((k) => byKind[k] ?? []) };
    return all[opts?.after ? Number(opts.after.slice(1)) : 0];
  });
  return { getEdges };
}

describe("loadFactEdges", () => {
  it("finds a relation that sits on the second get_edges page", async () => {
    const dl = layer([
      { edges: [edge("x", "y", "about")], truncated: true, next_cursor: "c1" },
      { edges: [edge("f1", "f9", "zz_late")] },
    ]);
    const got = await loadFactEdges(dl, "user", "doc-1", { scopeId: "bob" });
    expect(got.edges.some((e) => e.kind === "zz_late")).toBe(true);
    expect(got.incomplete).toBe(false);
    expect(dl.getEdges).toHaveBeenCalledWith("user", "doc-1", { scopeId: "bob", after: "c1" });
  });

  it("asks for the supersedes chain by kind, so it survives a capped listing", async () => {
    const endless: DocEdgesResponse[] = Array.from({ length: EDGE_PAGE_CAP + 5 }, (_, i) => ({
      edges: [edge("x", "y", "about")],
      truncated: true,
      next_cursor: `c${i + 1}`,
    }));
    const dl = layer(endless, { supersedes: [edge("f2", "f1", "supersedes")] });
    const got = await loadFactEdges(dl, "user", "doc-1");
    expect(got.edges.filter((e) => e.kind === "supersedes")).toEqual([edge("f2", "f1", "supersedes")]);
    expect(got.incomplete).toBe(true);
    expect(dl.getEdges).toHaveBeenCalledWith("user", "doc-1", { kinds: ["supersedes"] });
    // The cap holds for the unfiltered listing: EDGE_PAGE_CAP pages, plus the kinds call.
    expect(dl.getEdges).toHaveBeenCalledTimes(EDGE_PAGE_CAP + 1);
  });

  it("does not list a supersedes edge twice when both loads return it", async () => {
    const sup = edge("f2", "f1", "supersedes");
    const dl = layer([{ edges: [sup, edge("f1", "f3", "about")] }], { supersedes: [sup] });
    const got = await loadFactEdges(dl, "user", "doc-1");
    expect(got.edges.filter((e) => e.kind === "supersedes")).toHaveLength(1);
    expect(got.edges).toHaveLength(2);
  });

  it("a layer that hands back the cursor it was given is not looped", async () => {
    const getEdges = vi.fn<GetEdges>(async (_s, _d, opts) =>
      opts?.kinds ? { edges: [] } : { edges: [edge("x", "y", "about")], truncated: true, next_cursor: "same" },
    );
    const got = await loadFactEdges({ getEdges }, "user", "doc-1");
    expect(getEdges).toHaveBeenCalledTimes(3);
    expect(got.edges).toHaveLength(1);
    expect(got.incomplete).toBe(true);
  });
});
