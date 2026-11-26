import { describe, expect, it } from "vitest";
import type { OntologyRollbackReport } from "../api";
import type { OntologyPlannedChange, OntologyPlanResult } from "./ontologyRetirePlan";
import {
  rollbackMatchesText,
  rollbackPlanRisk,
  rollbackPlanRows,
  rollbackRefusal,
  rollbackRiskWarning,
} from "./ontologyRollback";

function change(over: Partial<OntologyPlannedChange>): OntologyPlannedChange {
  return {
    op: "rename",
    entry: { chunk_id: "c1", name: "Memo", section: "classes", kind: "class" },
    risk: "none",
    counts: {},
    release: 2,
    ...over,
  };
}

function plan(changes: OntologyPlannedChange[]): OntologyPlanResult {
  return { fingerprint: "fp1", changes, complete: true };
}

describe("rollbackPlanRows", () => {
  it("lists what each change stales or destroys, worst first, and leaves out what it keeps", () => {
    const rows = rollbackPlanRows(
      plan([
        change({
          risk: "destroys",
          counts: {
            typed_documents: { n: 3, effect: "stale", tenant_scope: 3 },
            node_stats: { n: 2, effect: "destroyed", tenant_scope: 2 },
            facts: { n: 9, effect: "kept", tenant_scope: 9 },
            measure_points: { n: 0, effect: "stale", tenant_scope: 0 },
          },
        }),
      ]),
    );
    expect(rows[0].effects).toEqual(["destroyed: node stats 2", "stale: typed documents 3"]);
    expect(rows[0].uncounted).toBe(false);
  });

  it("marks a change of risk stales with nothing counted as not counted (a fixed change)", () => {
    const grammar = change({
      op: "edit",
      entry: { chunk_id: "g1", name: "grammar:cite", section: "classes", kind: "grammar" },
      risk: "stales",
      notes: ["a grammar chunk of a class: the plan counts nothing that rests on it, so it is planned as stales"],
    });
    const add = change({ op: "add", risk: "none" });
    const rows = rollbackPlanRows(plan([grammar, add]));
    expect(rows.map((r) => [r.op, r.name, r.uncounted])).toEqual([
      ["edit", "grammar:cite", true],
      ["add", "Memo", false],
    ]);
    expect(rows[0].notes[0]).toContain("grammar chunk");
  });
});

describe("rollbackPlanRisk / rollbackRiskWarning", () => {
  it("is the worst risk of the plan, and warns only when it is not none", () => {
    expect(rollbackPlanRisk(plan([change({}), change({ risk: "stales" })]))).toBe("stales");
    expect(rollbackPlanRisk(plan([change({ risk: "stales" }), change({ risk: "destroys" })]))).toBe("destroys");
    expect(rollbackRiskWarning(plan([change({}), change({})]))).toBeNull();
    expect(rollbackRiskWarning(plan([change({ risk: "stales" })]))).toContain("stale");
    expect(rollbackRiskWarning(plan([change({ risk: "destroys" })]))).toContain("destroys");
  });
});

describe("rollbackMatchesText", () => {
  const rep: OntologyRollbackReport = {
    release: 2,
    release_content_version: "aaaa",
    content_version: "aaaa",
    matches: true,
    risk: "none",
    applied: [],
  };
  it("says nothing when the content versions match, and names both causes when they do not", () => {
    expect(rollbackMatchesText(rep)).toBeNull();
    const text = rollbackMatchesText({ ...rep, matches: false, content_version: "bbbb" });
    expect(text).toContain("added back under a new id");
    expect(text).toContain("renamed through the rename job");
  });
});

describe("rollbackRefusal", () => {
  it("reads the code and the server's sentence, and names a body without a code by its status", () => {
    expect(rollbackRefusal({ status: 409, body: { code: "entry_locked", error: "the entry is locked" } })).toEqual({
      code: "entry_locked",
      message: "the entry is locked",
    });
    expect(rollbackRefusal({ status: 422, body: null })).toEqual({ code: "http_422", message: "" });
  });
});
