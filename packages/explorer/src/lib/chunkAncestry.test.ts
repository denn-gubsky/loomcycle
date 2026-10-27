import { describe, it, expect } from "vitest";
import type { ChunkRow } from "../types";
import { chunkAncestors } from "./chunkAncestry";

const row = (id: string, parent_id?: string): ChunkRow => ({
  id,
  document_id: "doc",
  parent_id,
  position: 0,
  title: id,
  revision: 1,
});

// root → sec → sub → leaf, plus a sibling section.
const chunks = [row("leaf", "sub"), row("root"), row("sec", "root"), row("sub", "sec"), row("sec2", "root")];

describe("chunkAncestors", () => {
  it("lists the levels above a chunk, root first and the parent last", () => {
    expect(chunkAncestors(chunks, "leaf").map((c) => c.id)).toEqual(["root", "sec", "sub"]);
    expect(chunkAncestors(chunks, "sec2").map((c) => c.id)).toEqual(["root"]);
  });

  it("is empty where there is no level to go up to", () => {
    expect(chunkAncestors(chunks, "root")).toEqual([]);
    expect(chunkAncestors(chunks, "missing")).toEqual([]);
    expect(chunkAncestors(chunks, undefined)).toEqual([]);
    expect(chunkAncestors([row("orphan", "gone")], "orphan")).toEqual([]);
  });

  it("stops on a parent cycle instead of looping", () => {
    const cyclic = [row("a", "b"), row("b", "c"), row("c", "a")];
    expect(chunkAncestors(cyclic, "a").map((c) => c.id)).toEqual(["c", "b"]);
  });
});
