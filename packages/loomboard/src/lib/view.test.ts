import { describe, it, expect } from "vitest";
import {
  parseView,
  normalizeQuery,
  normalizeLayout,
  docRowToBoardRow,
  chunkRowToBoardRow,
  groupRows,
  sortRows,
  UNGROUPED,
} from "./view";
import type { BoardRow, ChunkDetail } from "../types";

describe("normalizeQuery", () => {
  it("defaults a missing/garbled query to a documents source", () => {
    expect(normalizeQuery(undefined)).toEqual({ source: "documents" });
    expect(normalizeQuery("nope")).toEqual({ source: "documents" });
  });
  it("keeps only string axes + a numeric limit", () => {
    expect(
      normalizeQuery({ source: "chunks", type: "task", status: "", limit: 50, bogus: 1 }),
    ).toEqual({ source: "chunks", type: "task", limit: 50 });
  });
});

describe("normalizeLayout", () => {
  it("defaults a garbled layout to a table grouped by status", () => {
    expect(normalizeLayout(null)).toMatchObject({ kind: "table", groupBy: "status", sortDir: "desc" });
  });
  it("clamps an unknown kind/axis and filters columns", () => {
    const l = normalizeLayout({ kind: "wat", groupBy: "tag", sortBy: "title", sortDir: "asc", columns: ["type", "x", "updated"] });
    expect(l.kind).toBe("table");
    expect(l.groupBy).toBe("status");
    expect(l.sortBy).toBe("title");
    expect(l.sortDir).toBe("asc");
    expect(l.columns).toEqual(["type", "updated"]);
  });
});

describe("parseView", () => {
  it("reads {query, layout} from the root chunk's fields", () => {
    const root = {
      id: "root-1",
      title: "RFCs",
      fields: { query: { source: "documents", type: "rfc" }, layout: { kind: "kanban", groupBy: "status" } },
    } as unknown as ChunkDetail;
    const v = parseView("doc-1", root);
    expect(v).toMatchObject({
      documentId: "doc-1",
      rootChunkId: "root-1",
      title: "RFCs",
      query: { source: "documents", type: "rfc" },
      layout: { kind: "kanban", groupBy: "status" },
    });
  });
  it("still opens a view with no fields (safe defaults)", () => {
    const root = { id: "r", title: "empty" } as unknown as ChunkDetail;
    expect(parseView("d", root).layout.kind).toBe("table");
  });
});

describe("row normalization", () => {
  it("maps a document row", () => {
    expect(
      docRowToBoardRow({ document_id: "d1", title: "T", root_chunk_id: "r", type: "rfc", status: "draft", updated_at: 5 }),
    ).toEqual({ id: "d1", documentId: "d1", title: "T", type: "rfc", status: "draft", updatedAt: 5 });
  });
  it("maps a chunk row (id != document_id)", () => {
    expect(
      chunkRowToBoardRow({ id: "c1", document_id: "d1", title: "step", position: 2, revision: 1, status: "todo" }),
    ).toEqual({ id: "c1", documentId: "d1", title: "step", type: undefined, status: "todo", position: 2 });
  });
});

describe("groupRows", () => {
  it("buckets by status in first-seen order; missing → UNGROUPED", () => {
    const rows: BoardRow[] = [
      { id: "1", documentId: "d", title: "a", status: "todo" },
      { id: "2", documentId: "d", title: "b", status: "done" },
      { id: "3", documentId: "d", title: "c", status: "todo" },
      { id: "4", documentId: "d", title: "d" },
    ];
    const g = groupRows(rows, "status");
    expect(g.map((x) => x.key)).toEqual(["todo", "done", UNGROUPED]);
    expect(g[0].rows.map((r) => r.id)).toEqual(["1", "3"]);
  });
});

describe("sortRows", () => {
  const rows: BoardRow[] = [
    { id: "1", documentId: "d", title: "banana", updatedAt: 10 },
    { id: "2", documentId: "d", title: "apple", updatedAt: 30 },
    { id: "3", documentId: "d", title: "cherry", updatedAt: 20 },
  ];
  it("sorts by updated desc by default", () => {
    expect(sortRows(rows, "updated").map((r) => r.id)).toEqual(["2", "3", "1"]);
  });
  it("sorts by title asc", () => {
    expect(sortRows(rows, "title", "asc").map((r) => r.id)).toEqual(["2", "1", "3"]);
  });
  it("is a no-op copy when no sort field", () => {
    const out = sortRows(rows);
    expect(out).not.toBe(rows);
    expect(out.map((r) => r.id)).toEqual(["1", "2", "3"]);
  });
});
