import { describe, expect, it } from "vitest";
import type { DiffOntologyReleasesResult, OntologyRelease } from "../api";
import { apiError } from "./apiError";
import {
  diffRows,
  diffSideLabel,
  diffSummary,
  directiveLines,
  previousRelease,
  releaseReasonLabel,
  releaseTime,
  releasesRefused,
} from "./ontologyReleaseDiff";

function diff(changes: DiffOntologyReleasesResult["changes"]): DiffOntologyReleasesResult {
  return {
    tenant: "t",
    from: { seq: 2, content_version: "aaaaaaaaaaaaaaaa" },
    to: { current: true, content_version: "bbbbbbbbbbbbbbbb" },
    changes,
    issues: [],
  };
}

describe("diffRows", () => {
  it("shows a rename as one entry with the name before and after, and nothing it did not change", () => {
    const rows = diffRows(
      diff([
        {
          chunk_id: "c1",
          kind: "class",
          change: "changed",
          changed: ["renamed"],
          before: { name: "event", meaning: "a thing that happened", directives: { identity: ["title"] } },
          after: { name: "incident", meaning: "a thing that happened", directives: { identity: ["title"] } },
        },
      ]),
    );
    expect(rows).toEqual([
      {
        chunkId: "c1",
        kind: "class",
        change: "changed",
        name: "incident",
        lines: [{ field: "name", before: "event", after: "incident" }],
      },
    ]);
  });

  it("names the parent before and after a move, and the top of the section when there is none", () => {
    const [row] = diffRows(
      diff([
        {
          chunk_id: "c2",
          kind: "concept",
          change: "changed",
          changed: ["moved", "status"],
          before: { name: "risk", parent: "p1", parent_name: "finance", directives: {} },
          after: { name: "risk", status: "retired", directives: {} },
        },
      ]),
    );
    expect(row.lines).toEqual([
      { field: "under", before: "finance", after: "(top of its section)" },
      { field: "status", before: "in force", after: "retired" },
    ]);
  });

  it("lists only the directives that differ, one line per key", () => {
    const [row] = diffRows(
      diff([
        {
          chunk_id: "c3",
          kind: "relation",
          change: "changed",
          changed: ["meaning", "directives"],
          before: { name: "cites", meaning: "old", directives: { domain: "Law", symmetric: true, alt: ["a"] } },
          after: { name: "cites", meaning: "new", directives: { domain: "Act", symmetric: true, alt: ["a", "b"] } },
        },
      ]),
    );
    expect(row.lines).toEqual([
      { field: "meaning", before: "old", after: "new" },
      { field: "@alt", before: "a", after: "a, b" },
      { field: "@domain", before: "Law", after: "Act" },
    ]);
  });

  it("shows what an added entry declares on the after side and a removed one on the before side", () => {
    const rows = diffRows(
      diff([
        {
          chunk_id: "c4",
          kind: "relation",
          change: "added",
          after: { name: "caused_by", parent: "p", parent_name: "related", directives: { transitive: true } },
        },
        { chunk_id: "c5", kind: "class", change: "removed", before: { name: "memo", directives: null } },
      ]),
    );
    expect(rows[0].lines).toEqual([
      { field: "name", before: "", after: "caused_by" },
      { field: "under", before: "", after: "related" },
      { field: "@transitive", before: "", after: "yes" },
    ]);
    expect(rows[1]).toMatchObject({ name: "memo", lines: [{ field: "name", before: "memo", after: "" }] });
  });
});

describe("directiveLines", () => {
  it("treats objects that differ only in key order as equal", () => {
    expect(directiveLines({ f: { a: 1, b: 2 } }, { f: { b: 2, a: 1 } })).toEqual([]);
  });
  it("reads a key only one side has as empty on the other", () => {
    expect(directiveLines(null, { identity: ["title", "date"] })).toEqual([
      { field: "@identity", before: "", after: "title, date" },
    ]);
  });
});

describe("diffSummary", () => {
  it("counts each kind of change and says so when there is none", () => {
    expect(diffSummary(diff([]))).toBe("no differences");
    expect(diffSummary(diff(null))).toBe("no differences");
    expect(
      diffSummary(
        diff([
          { chunk_id: "a", kind: "class", change: "changed", changed: ["meaning"] },
          { chunk_id: "b", kind: "class", change: "added", after: { name: "b", directives: {} } },
        ]),
      ),
    ).toBe("1 changed, 1 added, 0 removed");
  });
});

describe("release labels", () => {
  it("names each reason the server records", () => {
    expect(["confirm", "apply", "rollback", "catch_up"].map(releaseReasonLabel)).toEqual([
      "confirmed",
      "change applied",
      "rolled back",
      "catch-up",
    ]);
  });
  it("renders unix nanoseconds as a UTC minute", () => {
    expect(releaseTime(1764000000000000000)).toBe("2025-11-24 16:00 UTC");
    expect(releaseTime(0)).toBe("—");
  });
  it("names a side as a release or the ontology in force", () => {
    expect(diffSideLabel({ seq: 3, content_version: "x" })).toBe("release 3");
    expect(diffSideLabel({ current: true, content_version: "x" })).toBe("the ontology in force");
  });
  it("finds the release before one, and none before the oldest kept", () => {
    const rs = [5, 3, 2].map((seq) => ({ seq }) as OntologyRelease);
    expect(previousRelease(rs, 5)?.seq).toBe(3);
    expect(previousRelease(rs, 2)).toBeUndefined();
  });
});

describe("releasesRefused", () => {
  it("is a 403 from the server and nothing else", () => {
    expect(releasesRefused(apiError(403, "Forbidden", JSON.stringify({ code: "forbidden", error: "operator only" })))).toBe(
      true,
    );
    expect(releasesRefused(apiError(404, "Not Found", JSON.stringify({ code: "release_not_found" })))).toBe(false);
    expect(releasesRefused(new Error("403 Forbidden"))).toBe(false);
  });
});
