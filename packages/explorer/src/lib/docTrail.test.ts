import { describe, it, expect } from "vitest";
import type { ChunkRow, PathEntry } from "../types";
import type { DirListing } from "./lazyPathTree";
import {
  amendCurrent,
  back,
  collapseTrail,
  findDocumentPath,
  followReference,
  jumpTo,
  labelOf,
  landingChunk,
  type Trail,
} from "./docTrail";

const A = { documentId: "doc-a", chunkId: "a-3", title: "Memory accuracy", path: "/research/memory" };
const B = { documentId: "doc-b", chunkId: "b-root", title: "One chunk store" };
const C = { documentId: "doc-c", chunkId: "c-7", title: "Memory architecture" };

describe("followReference — following a reference pushes", () => {
  it("seeds an empty trail with the document being left, at its selected chunk", () => {
    expect(followReference([], A, B)).toEqual([A, B]);
  });

  it("brings the entry being left up to date instead of duplicating it", () => {
    const trail = followReference([], A, { ...B, chunkId: "b-root" });
    // The reader moved to b-9 inside B before following on to C.
    const next = followReference(trail, { documentId: "doc-b", chunkId: "b-9", title: "RFC CV" }, C);
    expect(next.map((l) => l.documentId)).toEqual(["doc-a", "doc-b", "doc-c"]);
    expect(next[1]).toMatchObject({ chunkId: "b-9", title: "RFC CV" });
  });

  it("keeps a path the trail already knows when the leaving snapshot has none", () => {
    const trail: Trail = [A, { ...B, path: "/rfcs/cv" }];
    const next = followReference(trail, { documentId: "doc-b", chunkId: "b-2", path: undefined }, C);
    expect(next[1].path).toBe("/rfcs/cv");
  });

  it("revisiting a document is a new entry, so the way back runs through the whole chain", () => {
    const trail = followReference(followReference([], A, B), B, { ...A, chunkId: "a-9" });
    expect(trail.map((l) => l.chunkId)).toEqual(["a-3", "b-root", "a-9"]);
  });
});

describe("back / jumpTo — returning pops to the document AND its chunk", () => {
  const trail: Trail = [A, B, C];

  it("back returns to the previous document at the chunk selected there", () => {
    const next = back(trail);
    expect(next).toEqual([A, B]);
    expect(next[next.length - 1]).toMatchObject({ documentId: "doc-b", chunkId: "b-root" });
  });

  it("a crumb jump drops everything after it and restores its chunk", () => {
    const next = jumpTo(trail, 0);
    expect(next).toEqual([A]);
    expect(next[0].chunkId).toBe("a-3");
  });

  it("jumping to the current entry, or out of range, changes nothing", () => {
    expect(jumpTo(trail, 2)).toBe(trail);
    expect(jumpTo(trail, -1)).toBe(trail);
    expect(jumpTo(trail, 9)).toBe(trail);
    expect(back([A])).toEqual([A]);
    expect(back([])).toEqual([]);
  });
});

describe("amendCurrent — what is learned after the jump", () => {
  it("replaces the stand-in title and fills in the path of the document on screen", () => {
    const next = amendCurrent([A, B], "doc-b", { title: "RFC CV — One chunk store", path: "/rfcs/cv" });
    expect(next[1]).toMatchObject({ title: "RFC CV — One chunk store", path: "/rfcs/cv", chunkId: "b-root" });
  });

  it("drops an answer for a document the reader has already left", () => {
    const trail: Trail = [A, B];
    expect(amendCurrent(trail, "doc-a", { path: "/elsewhere" })).toBe(trail);
    expect(amendCurrent([], "doc-a", { title: "x" })).toEqual([]);
  });

  it("returns the same trail when nothing changed, so a host does not re-render", () => {
    const trail: Trail = [A, B];
    expect(amendCurrent(trail, "doc-b", { title: B.title })).toBe(trail);
  });
});

describe("collapseTrail", () => {
  it("shows a short trail whole", () => {
    expect(collapseTrail(4)).toEqual([0, 1, 2, 3].map((index) => ({ kind: "crumb", index })));
  });

  it("folds a long trail's middle, keeping where it started and the last steps", () => {
    expect(collapseTrail(7)).toEqual([
      { kind: "crumb", index: 0 },
      { kind: "gap", hidden: [1, 2, 3] },
      { kind: "crumb", index: 4 },
      { kind: "crumb", index: 5 },
      { kind: "crumb", index: 6 },
    ]);
  });
});

describe("labelOf", () => {
  it("prefers the title, then the path's last segment, then a short id", () => {
    expect(labelOf(A)).toBe("Memory accuracy");
    expect(labelOf({ documentId: "doc-a", path: "/research/memory" })).toBe("memory");
    expect(labelOf({ documentId: "0123456789abcdef" })).toBe("01234567");
  });
});

describe("findDocumentPath", () => {
  const entry = (full_path: string, kind: string, document_id?: string): PathEntry => ({
    name: full_path.slice(full_path.lastIndexOf("/") + 1),
    kind,
    full_path,
    resource_ref: document_id ? { document_id } : undefined,
  });
  const listing = (entries: PathEntry[]): DirListing => ({ entries, loaded: true, loading: false });
  const dirs = new Map<string, DirListing>([
    ["", listing([entry("/rfcs", "directory")])],
    ["/rfcs", listing([entry("/rfcs/cv", "document", "doc-b"), entry("/rfcs/note", "memory_entry", "doc-c")])],
  ]);

  it("finds a document in a directory the tree has listed", () => {
    expect(findDocumentPath(dirs, "doc-b")).toBe("/rfcs/cv");
  });

  it("does not find one it has not listed, or a non-document entry with that id", () => {
    expect(findDocumentPath(dirs, "doc-zzz")).toBeUndefined();
    expect(findDocumentPath(dirs, "doc-c")).toBeUndefined();
  });
});

describe("landingChunk", () => {
  const row = (id: string, parent_id?: string): ChunkRow => ({
    id,
    document_id: "doc-b",
    parent_id,
    position: 0,
    title: id,
    revision: 1,
  });
  const rows = [row("root"), row("s1", "root"), row("s2", "root")];

  it("lands on the referenced chunk", () => {
    expect(landingChunk(rows, "s2")).toBe("s2");
  });

  it("falls back to the root when no chunk was asked for, or it is gone", () => {
    expect(landingChunk(rows)).toBe("root");
    expect(landingChunk(rows, "deleted")).toBe("root");
    expect(landingChunk([], "s2")).toBeUndefined();
  });
});
