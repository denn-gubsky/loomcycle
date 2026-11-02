import { describe, expect, it } from "vitest";
import { addPage, emptyModel, refreshEdges, type ModelEdge } from "./model";
import { computeLayout } from "./layout";
import { computeLod, drawnLinks } from "./lod";
import { dataLayerFromClient } from "./dataLayer";
import { linkKindChoices, linkRows } from "../components/Links";
import type { GraphSnapshot } from "../types";
import type { LoomcycleClient } from "@loomcycle/client";

// Three documents: A (A0 root, A1), B (B0 root, B1), C (C0 root).
const DOCS = { A: ["A0", "A1"], B: ["B0", "B1"], C: ["C0"] } as const;
type DocName = keyof typeof DOCS;

interface EdgeSpec {
  id: string;
  from: string;
  to: string;
  kind: string;
  status?: string;
  confidence?: number | null;
  invalidAt?: number | null;
}

const docOf = (chunk: string) => chunk[0] as DocName;

// page builds a graph_snapshot page holding `docs` and every edge touching
// them; an endpoint outside the page goes to `external`, as the runtime sends it.
function page(docs: DocName[], edges: EdgeSpec[]): GraphSnapshot {
  const chunkIds: string[] = [];
  const docIdx: number[] = [];
  const parent: number[] = [];
  docs.forEach((d, di) => {
    const base = chunkIds.length;
    DOCS[d].forEach((c, i) => {
      chunkIds.push(c);
      docIdx.push(di);
      parent.push(i === 0 ? -1 : base);
    });
  });
  const kinds: string[] = [];
  const statuses: string[] = [];
  const intern = (t: string[], v: string) => (t.includes(v) ? t.indexOf(v) : t.push(v) - 1);
  const external: { idx: number; id: string; document_id: string }[] = [];
  const end = (c: string) => {
    const i = chunkIds.indexOf(c);
    if (i >= 0) return i;
    const idx = -(external.length + 1);
    external.push({ idx, id: c, document_id: docOf(c) });
    return idx;
  };
  const on = edges.filter((e) => docs.includes(docOf(e.from)) || docs.includes(docOf(e.to)));
  return {
    page: { documents: docs.length },
    codes: { types: ["document"], statuses, kinds },
    documents: {
      ids: [...docs],
      titles: docs.map((d) => `Doc ${d}`),
      root_chunk_ids: docs.map((d) => DOCS[d][0]),
      type_code: docs.map(() => 0),
      status_code: docs.map(() => -1),
      updated_at: docs.map(() => 0),
      color_enabled: docs.map(() => false),
      color_scheme: docs.map(() => null),
      paths: docs.map((d) => [`/kb/${d}`]),
    },
    chunks: {
      ids: chunkIds,
      document_idx: docIdx,
      parent_idx: parent,
      position: chunkIds.map(() => 0),
      type_code: chunkIds.map(() => 0),
      status_code: chunkIds.map(() => -1),
      titles: chunkIds.map((c) => `Chunk ${c}`),
      updated_at: chunkIds.map(() => 0),
    },
    edges: {
      ids: on.map((e) => e.id),
      from_idx: on.map((e) => end(e.from)),
      to_idx: on.map((e) => end(e.to)),
      kind_code: on.map((e) => intern(kinds, e.kind)),
      status_code: on.map((e) => intern(statuses, e.status ?? "asserted")),
      confidence: on.map((e) => e.confidence ?? null),
      invalid_at: on.map((e) => e.invalidAt ?? null),
      external,
      truncated: false,
    },
  };
}

const START: EdgeSpec[] = [
  { id: "e1", from: "A1", to: "B1", kind: "related" },
  { id: "e2", from: "B0", to: "C0", kind: "related" },
  { id: "e3", from: "A0", to: "C0", kind: "supports", status: "proposed" },
  { id: "e5", from: "C0", to: "A0", kind: "related" },
];

function loaded() {
  const m = emptyModel();
  addPage(m, page(["A", "B", "C"], START));
  return m;
}

const byId = (es: ModelEdge[]) => new Map(es.map((e) => [e.id, e]));

describe("refreshEdges", () => {
  it("TestRefreshEdges_ReplacesOnlyTheLinksOfTheRefreshedDocuments", () => {
    const m = loaded();
    expect(m.edges.map((e) => e.id).sort()).toEqual(["e1", "e2", "e3", "e5"]);
    // After a write on A: e3 accepted, e1 re-scored, e4 proposed, e5 deleted.
    const after: EdgeSpec[] = [
      { id: "e1", from: "A1", to: "B1", kind: "related", confidence: 0.4 },
      { id: "e2", from: "B0", to: "C0", kind: "related" },
      { id: "e3", from: "A0", to: "C0", kind: "supports", status: "asserted" },
      { id: "e4", from: "A0", to: "B0", kind: "contradicts", status: "proposed" },
    ];
    const v = m.version;
    refreshEdges(m, page(["A"], after), new Set(["A"]));
    const got = byId(m.edges);
    expect([...got.keys()].sort()).toEqual(["e1", "e2", "e3", "e4"]);
    expect(got.get("e1")?.confidence).toBe(0.4);
    expect(got.get("e3")?.status).toBe("asserted");
    expect(got.get("e4")?.status).toBe("proposed");
    // Endpoints resolve against the whole model, not only the page's chunks.
    expect(got.get("e4")?.to).toBe(m.chunkIndex.get("B0"));
    expect(m.kinds).toContain("contradicts");
    expect(m.version).toBeGreaterThan(v);
  });

  it("TestRefreshEdges_LeavesOtherDocumentsLinksAlone", () => {
    const m = loaded();
    const e2 = m.edges.find((e) => e.id === "e2");
    refreshEdges(m, page(["A"], []), new Set(["A"]));
    expect(m.edges.map((e) => e.id)).toEqual(["e2"]);
    expect(m.edges[0]).toBe(e2);
  });
});

describe("drawn links", () => {
  const NOW = Date.UTC(2026, 10, 2);
  const nsAt = (ms: number) => ms * 1e6;

  function drawn(edges: EdgeSpec[]) {
    const m = emptyModel();
    addPage(m, page(["A", "B", "C"], edges));
    const l = computeLayout(m);
    const far = { cam: [0, 0, l.radius * 400] as [number, number, number], pxPerUnit: 900 };
    // Every document pinned open, so each endpoint draws as itself.
    const lod = computeLod(m, l, far, null, new Set([0, 1, 2]));
    return drawnLinks(m, l, lod, m.edges, NOW);
  }

  it("TestDrawnLinks_StrengthIsTheMeanConfidenceAndOneWithoutAny", () => {
    const [scored] = drawn([{ id: "x", from: "A1", to: "B1", kind: "related", confidence: 0.3 }]);
    expect(scored.confidence).toBeCloseTo(0.3);
    const [plain] = drawn([{ id: "y", from: "A1", to: "B1", kind: "related" }]);
    expect(plain.confidence).toBe(1);
  });

  it("TestDrawnLinks_ExpiredOnlyWhenEveryMergedStatementHasEnded", () => {
    const past = nsAt(NOW - 86_400_000);
    const future = nsAt(NOW + 86_400_000);
    const [ended] = drawn([{ id: "x", from: "A1", to: "B1", kind: "related", invalidAt: past }]);
    expect(ended.expired).toBe(true);
    const [running] = drawn([{ id: "y", from: "A1", to: "B1", kind: "related", invalidAt: future }]);
    expect(running.expired).toBe(false);
  });
});

describe("link list", () => {
  it("TestLinkRows_ReadFromTheSelectionsSideProposedFirst", () => {
    const m = loaded();
    const a0 = m.chunkIndex.get("A0")!;
    const a1 = m.chunkIndex.get("A1")!;
    const groups = linkRows(m, m.edges, new Set([a0, a1]), Date.now());
    // e2 (B → C) does not touch A.
    expect([...groups.values()].flat().map((r) => r.edge.id).sort()).toEqual(["e1", "e3", "e5"]);
    const related = groups.get("related")!;
    const e5 = related.find((r) => r.edge.id === "e5")!;
    expect(e5.outgoing).toBe(false);
    expect(e5.other).toBe(m.chunkIndex.get("C0"));
    const e1 = related.find((r) => r.edge.id === "e1")!;
    expect(e1.outgoing).toBe(true);
    expect(e1.otherTitle).toBe("Chunk B1 — Doc B");
    expect(groups.get("supports")![0].edge.status).toBe("proposed");
  });

  it("TestLinkKindChoices_NeverOffersReferences", () => {
    expect(linkKindChoices(["references", "supports"])).toEqual(["related", "supports"]);
    expect(linkKindChoices(["related", "references"])).toEqual(["related"]);
  });
});

describe("statement writes", () => {
  function recording() {
    const calls: Record<string, unknown>[] = [];
    const client = {
      document: async (input: Record<string, unknown>) => {
        calls.push(input);
        return { id: "s1", status: input.op === "link_chunks" ? "asserted" : "proposed" };
      },
    } as unknown as LoomcycleClient;
    return { calls, data: dataLayerFromClient(client) };
  }

  it("TestWriteLink_ProposeIsTheReviewedPathAndAssertLinksAtOnce", async () => {
    const { calls, data } = recording();
    await data.writeLink!({ scope: "user", fromId: "A1", toId: "B1", kind: "supports", mode: "propose" });
    await data.writeLink!({ scope: "user", fromId: "A1", toId: "B1", kind: "supports", mode: "assert" });
    expect(calls).toEqual([
      { op: "propose_statement", scope: "user", from_id: "A1", to_id: "B1", kind: "supports" },
      { op: "link_chunks", scope: "user", from_id: "A1", to_id: "B1", kind: "supports" },
    ]);
  });

  it("TestReviewLink_RejectCarriesTheReason", async () => {
    const { calls, data } = recording();
    await data.reviewLink!({ scope: "tenant", id: "s1", verdict: "accept" });
    await data.reviewLink!({ scope: "tenant", id: "s1", verdict: "reject", reason: "wrong target" });
    expect(calls).toEqual([
      { op: "accept_statement", scope: "tenant", id: "s1" },
      { op: "reject_statement", scope: "tenant", id: "s1", reason: "wrong target" },
    ]);
  });
});
