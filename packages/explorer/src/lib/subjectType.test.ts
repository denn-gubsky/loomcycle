import { describe, it, expect } from "vitest";
import type { ChunkDetail } from "../types";
import { entityTypeNames, isSubjectRoot, subjectTypeChoices } from "./subjectType";

const chunk = (over: Partial<ChunkDetail>): ChunkDetail => ({
  id: "c1",
  document_id: "d1",
  position: 0,
  title: "Ada",
  revision: 1,
  body: "",
  ...over,
});

describe("isSubjectRoot", () => {
  it("is true only for a document's root chunk that carries a natural key", () => {
    expect(isSubjectRoot(chunk({ entity: { natural_key: "person:ada" } }))).toBe(true);
    // A keyed fact filed under a subject is not the subject.
    expect(isSubjectRoot(chunk({ parent_id: "root", entity: { natural_key: "fact:x" } }))).toBe(false);
    // A plain document root, and a root with entity metadata but no key.
    expect(isSubjectRoot(chunk({}))).toBe(false);
    expect(isSubjectRoot(chunk({ entity: {} }))).toBe(false);
    expect(isSubjectRoot(null)).toBe(false);
  });
});

describe("entityTypeNames", () => {
  it("keeps chunk classes, drops document classes, dedupes and sorts", () => {
    expect(
      entityTypeNames({
        classes: [
          { name: "person", kind: "chunk" },
          { name: "statute", kind: "document" },
          { name: "event" },
          { name: "person", kind: "chunk" },
          { name: "", kind: "chunk" },
          { kind: "chunk" },
        ],
      }),
    ).toEqual(["event", "person"]);
  });

  it("reads an unexpected answer as no types", () => {
    expect(entityTypeNames(null)).toEqual([]);
    expect(entityTypeNames({ classes: "nope" })).toEqual([]);
  });
});

describe("subjectTypeChoices", () => {
  it("adds the current type when it is not in force, so the picker shows where the subject stands", () => {
    expect(subjectTypeChoices(["event", "person"], "starship")).toEqual(["starship", "event", "person"]);
  });

  it("does not repeat a current type already in force, in any case", () => {
    expect(subjectTypeChoices(["event", "person"], "Person")).toEqual(["event", "person"]);
    expect(subjectTypeChoices(["event"], undefined)).toEqual(["event"]);
  });
});
