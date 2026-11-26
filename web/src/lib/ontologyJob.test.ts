import { describe, expect, it } from "vitest";
import type { OntologyJobStatus } from "../api";
import { jobProgress, jobStateLabel, jobTitle, jobUnfinished } from "./ontologyJob";

function job(over: Partial<OntologyJobStatus>): OntologyJobStatus {
  return {
    id: "j1",
    kind: "class_rename",
    entry: "c1",
    from: ["Bulletin"],
    to: "Circular",
    state: "running",
    remaining: 0,
    ...over,
  };
}

describe("jobUnfinished", () => {
  it("is true for pending, running and blocked, and false once done or abandoned", () => {
    expect(["pending", "running", "blocked", "done", "abandoned"].map((s) => jobUnfinished(job({ state: s })))).toEqual([
      true,
      true,
      true,
      false,
      false,
    ]);
  });
});

describe("jobTitle", () => {
  it("names a class rename's old names and its new one", () => {
    expect(jobTitle(job({ from: ["Bulletin", "Notice"] }))).toBe("Class rename: Bulletin / Notice → Circular");
  });

  it("pairs each old tag of a re-tag with its new one", () => {
    expect(
      jobTitle(
        job({
          kind: "concept_retag",
          from: ["topics/risk", "topics/safety"],
          to_tags: ["topics/reporting/risk", "topics/reporting/safety"],
          to: "topics/reporting/risk, topics/reporting/safety",
        }),
      ),
    ).toBe("Concept re-tag: topics/risk → topics/reporting/risk, topics/safety → topics/reporting/safety");
  });
});

describe("jobStateLabel", () => {
  it("says why an abandoned job did nothing", () => {
    expect(jobStateLabel("abandoned")).toContain("never reached the ontology");
    expect(jobStateLabel("running")).toBe("in progress");
  });
});

describe("jobProgress", () => {
  it("places an unfinished job: its step and scope, the scopes done, the rows left and rewritten", () => {
    expect(
      jobProgress(
        job({ step: "chunks_type", scope: "user", scopes_done: 1, scopes_total: 3, remaining: 12, done: { documents_type: 4, chunks_type: 1 } }),
      ),
    ).toBe("step chunks_type in the user scope · 1 of 3 scopes done · 12 rows left in this scope · 5 rows rewritten");
  });

  it("leaves out what the server did not give", () => {
    expect(jobProgress(job({ step: "chunks_type" }))).toBe("step chunks_type");
  });

  it("gives a finished job the scopes it walked, never a step or rows left", () => {
    expect(jobProgress(job({ state: "done", step: "", scopes_done: 3, scopes_total: 3, remaining: 9, done: { documents_type: 1 } }))).toBe(
      "3 scopes walked · 1 row rewritten",
    );
  });
});
