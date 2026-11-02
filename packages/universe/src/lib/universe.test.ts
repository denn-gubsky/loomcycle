import { describe, expect, it } from "vitest";
import { addPage, emptyModel } from "./model";
import { computeLayout } from "./layout";
import { adjacencyOf, computeLod, drawnLinks, neighbourhood, representative, starVisible } from "./lod";
import { syntheticSnapshot } from "./synthetic";
import type { GraphSnapshot } from "../types";

function model(pages: GraphSnapshot[]) {
  const m = emptyModel();
  for (const p of pages) addPage(m, p);
  return m;
}

describe("model", () => {
  it("TestModel_DocumentChunksAreContiguousAcrossPages", () => {
    const m = model([
      syntheticSnapshot({ docs: 3, chunksPerDoc: 5, idPrefix: "p1" }),
      syntheticSnapshot({ docs: 2, chunksPerDoc: 4, idPrefix: "p2" }),
    ]);
    expect(m.docCount).toBe(5);
    expect(m.chunkCount).toBe(3 * 5 + 2 * 4);
    for (let d = 0; d < m.docCount; d++) {
      for (let c = m.doc.chunkStart[d]; c < m.doc.chunkStart[d] + m.doc.chunkCount[d]; c++) {
        expect(m.chunk.doc[c]).toBe(d);
      }
      expect(m.chunk.parent[m.doc.root[d]]).toBe(-1);
    }
  });

  it("TestModel_ParentsPointBackwardsAndDepthFollows", () => {
    const m = model([syntheticSnapshot({ docs: 4, chunksPerDoc: 30 })]);
    for (let c = 0; c < m.chunkCount; c++) {
      const p = m.chunk.parent[c];
      expect(p).toBeLessThan(c);
      expect(m.chunk.depth[c]).toBe(p < 0 ? 0 : m.chunk.depth[p] + 1);
    }
  });

  it("TestModel_CrossPageEdgeResolvesWhenItsTargetLoads", () => {
    const p1 = syntheticSnapshot({ docs: 1, chunksPerDoc: 2, idPrefix: "a" });
    p1.edges = {
      ids: ["x"],
      from_idx: [0],
      to_idx: [-1],
      kind_code: [0],
      external: [{ idx: -1, id: "bd000000c1", document_id: "bd000000" }],
    };
    const p2 = syntheticSnapshot({ docs: 1, chunksPerDoc: 2, idPrefix: "b" });
    // The same edge again, as the backlink into page two.
    p2.edges = {
      ids: ["x"],
      from_idx: [-1],
      to_idx: [1],
      kind_code: [0],
      external: [{ idx: -1, id: "ad000000c0", document_id: "ad000000" }],
    };
    const m = emptyModel();
    addPage(m, p1);
    expect(m.edges).toHaveLength(1);
    expect(m.edges[0].to).toBe(-1);
    addPage(m, p2);
    expect(m.edges).toHaveLength(1);
    expect(m.edges[0].to).toBe(m.chunkIndex.get("bd000000c1"));
  });

  it("TestModel_CodesAreInternedPerPage", () => {
    const p1 = syntheticSnapshot({ docs: 1, chunksPerDoc: 1, idPrefix: "a" });
    const p2 = syntheticSnapshot({ docs: 1, chunksPerDoc: 1, idPrefix: "b" });
    p2.codes = { ...p2.codes, statuses: ["done", "draft"] };
    const m = model([p1, p2]);
    expect(m.doc.status[0]).toBe("draft");
    expect(m.doc.status[1]).toBe("done");
  });
});

describe("layout", () => {
  it("TestLayout_IsByteStableAcrossRuns", () => {
    const a = computeLayout(model([syntheticSnapshot({ docs: 40, chunksPerDoc: 12 })]));
    const b = computeLayout(model([syntheticSnapshot({ docs: 40, chunksPerDoc: 12 })]));
    expect(Buffer.from(a.chunkPos.buffer).equals(Buffer.from(b.chunkPos.buffer))).toBe(true);
  });

  it("TestLayout_DoesNotDependOnPageOrder", () => {
    const one = syntheticSnapshot({ docs: 6, chunksPerDoc: 4, idPrefix: "a" });
    const two = syntheticSnapshot({ docs: 6, chunksPerDoc: 4, idPrefix: "b", dirs: ["/q", "/r"] });
    const m1 = model([one, two]);
    const m2 = model([two, one]);
    const l1 = computeLayout(m1);
    const l2 = computeLayout(m2);
    for (const id of m1.chunk.id) {
      const i = m1.chunkIndex.get(id)!;
      const j = m2.chunkIndex.get(id)!;
      for (let k = 0; k < 3; k++) expect(l1.chunkPos[i * 3 + k]).toBeCloseTo(l2.chunkPos[j * 3 + k], 4);
    }
  });

  it("TestLayout_SiblingFormationsDoNotOverlap", () => {
    const m = model([syntheticSnapshot({ docs: 60, chunksPerDoc: 10 })]);
    const l = computeLayout(m);
    for (const d of l.dirs) {
      for (let i = 0; i < d.children.length; i++) {
        for (let j = i + 1; j < d.children.length; j++) {
          const a = l.dirs[d.children[i]];
          const b = l.dirs[d.children[j]];
          const dist = Math.hypot(a.center[0] - b.center[0], a.center[1] - b.center[1], a.center[2] - b.center[2]);
          expect(dist).toBeGreaterThan(Math.max(a.radius, b.radius));
        }
      }
    }
  });

  it("TestLayout_RootChunkSitsAtTheDocumentCentre", () => {
    const m = model([syntheticSnapshot({ docs: 5, chunksPerDoc: 8 })]);
    const l = computeLayout(m);
    for (let d = 0; d < m.docCount; d++) {
      const r = m.doc.root[d];
      for (let k = 0; k < 3; k++) expect(l.chunkPos[r * 3 + k]).toBeCloseTo(l.docCenter[d * 3 + k], 5);
    }
  });

  it("TestLayout_HundredThousandChunksUnderABudget", () => {
    const m = model([syntheticSnapshot({ docs: 5000, chunksPerDoc: 20 })]);
    const t = performance.now();
    computeLayout(m);
    const ms = performance.now() - t;
    // The main-thread budget the plan sets for skipping a worker: one frame
    // burst, well under half a second on a laptop.
    expect(ms).toBeLessThan(1500);
  });
});

describe("lod", () => {
  const m = model([syntheticSnapshot({ docs: 30, chunksPerDoc: 10, edgesPerDoc: 2 })]);
  const l = computeLayout(m);
  const far = { cam: [0, 0, l.radius * 400] as [number, number, number], pxPerUnit: 900 };
  const near = (di: number) => ({
    cam: [l.docCenter[di * 3], l.docCenter[di * 3 + 1], l.docCenter[di * 3 + 2] + l.docRadius[di] * 3] as [
      number,
      number,
      number,
    ],
    pxPerUnit: 900,
  });

  it("TestLod_FarSkyCollapsesFormationsToStars", () => {
    const lod = computeLod(m, l, far, null, new Set());
    expect([...lod.docExpanded].every((x) => x === 0)).toBe(true);
    expect(lod.dirCollapsed.some((x) => x === 1)).toBe(true);
  });

  it("TestLod_ZoomedDocumentShowsItsChunks", () => {
    const lod = computeLod(m, l, near(3), null, new Set());
    expect(lod.docExpanded[3]).toBe(1);
    const s = m.doc.chunkStart[3] + 2;
    expect(starVisible(m, l, lod, s)).toBe(true);
    expect(representative(m, l, lod, s)).toBe(s);
  });

  it("TestLod_HysteresisHoldsBetweenThresholds", () => {
    const di = 0;
    // Place the camera so the document projects to 32 px: between 24 and 40.
    const dist = (l.docRadius[di] * 900) / 32;
    const v = {
      cam: [l.docCenter[0], l.docCenter[1], l.docCenter[2] + dist] as [number, number, number],
      pxPerUnit: 900,
    };
    const expanded = computeLod(m, l, near(di), null, new Set());
    expect(computeLod(m, l, v, expanded, new Set()).docExpanded[di]).toBe(1);
    const collapsed = computeLod(m, l, far, null, new Set());
    expect(computeLod(m, l, v, collapsed, new Set()).docExpanded[di]).toBe(0);
  });

  it("TestLod_LinkEndpointsClimbToTheVisibleStar", () => {
    const lod = computeLod(m, l, far, null, new Set());
    for (let c = 0; c < m.chunkCount; c++) {
      const r = representative(m, l, lod, c);
      expect(starVisible(m, l, lod, r)).toBe(true);
    }
  });

  it("TestLod_ParallelLinksMergeIntoOneWithACount", () => {
    const lod = computeLod(m, l, far, null, new Set());
    const links = drawnLinks(m, l, lod, m.edges);
    const total = links.reduce((s, x) => s + x.count, 0);
    expect(links.length).toBeLessThan(m.edges.length);
    expect(total).toBeLessThanOrEqual(m.edges.length);
    for (const x of links) expect(x.a).not.toBe(x.b);
  });

  it("TestLod_PinnedDocumentStaysExpandedFarAway", () => {
    const lod = computeLod(m, l, far, null, new Set([7]));
    expect(lod.docExpanded[7]).toBe(1);
    expect(lod.dirTop[l.docDir[7]]).toBe(-1);
  });

  it("TestNeighbourhood_DocumentRootSelectsEveryChunksLinks", () => {
    const adj = adjacencyOf(m);
    const withEdges = m.edges[0];
    const di = m.chunk.doc[withEdges.from];
    const byRoot = neighbourhood(m, adj, [m.doc.root[di]], 1);
    expect(byRoot.edges.map((e) => e.id)).toContain(withEdges.id);
    const deeper = neighbourhood(m, adj, [m.doc.root[di]], 2);
    expect(deeper.edges.length).toBeGreaterThanOrEqual(byRoot.edges.length);
  });
});

describe("quick search", () => {
  it("TestQuickSearch_RanksPrefixAboveSubstringAndDocumentsFirst", async () => {
    const { quickSearch } = await import("./search");
    const m = model([syntheticSnapshot({ docs: 12, chunksPerDoc: 4 })]);
    const hits = quickSearch(m, "document 1");
    expect(hits.length).toBeGreaterThan(0);
    // "Document 1" and "Document 10/11" are document roots: they lead.
    expect(m.chunk.depth[hits[0].chunk]).toBe(0);
    expect(m.doc.title[m.chunk.doc[hits[0].chunk]].toLowerCase().startsWith("document 1")).toBe(true);
    expect(quickSearch(m, "x")).toHaveLength(0);
  });
});
