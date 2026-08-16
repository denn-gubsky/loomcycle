import { describe, expect, it } from "vitest";
import { countMeaning, exportMisconfigured, familyEffect } from "./retention";

describe("familyEffect", () => {
  it("names what each mode does", () => {
    expect(familyEffect("off")).toBe("inert");
    expect(familyEffect("prune")).toBe("deletes");
    expect(familyEffect("export+prune")).toBe("exports-then-deletes");
  });

  it("treats an unknown mode as inert rather than as deleting", () => {
    // A mode this UI does not know must not be announced as destroying data. The safe
    // reading of an unknown is that we cannot say it deletes.
    expect(familyEffect("something-new")).toBe("inert");
    expect(familyEffect("")).toBe("inert");
  });
});

describe("countMeaning", () => {
  it("does not imply a deletion that will never happen", () => {
    // `purgeable` is computed regardless of mode. Shown beside an `off` family, a bare
    // count reads as a countdown to deletion.
    expect(countMeaning("off", 38)).toContain("this family is off");
    expect(countMeaning("off", 38)).not.toContain("will be removed");
  });

  it("says plainly when data IS about to go", () => {
    expect(countMeaning("prune", 38)).toContain("will be removed");
    expect(countMeaning("export+prune", 38)).toContain("will be removed");
  });

  it("distinguishes zero from absent", () => {
    // Zero means the age matched nothing; absent means the caller may not see counts at
    // all (a tenant operator), and inventing "0" for them would assert something the
    // server did not say.
    expect(countMeaning("prune", 0)).toBe("nothing matches the current age");
    expect(countMeaning("prune", undefined)).toBe("");
  });
});

describe("exportMisconfigured", () => {
  it("catches the combination that silently does nothing", () => {
    // export+prune with no export dir: the sweeper refuses the family, so the data is
    // neither exported nor pruned — and this report is the only place that shows.
    expect(exportMisconfigured("export+prune", "")).toBe(true);
    expect(exportMisconfigured("export+prune", undefined)).toBe(true);
    expect(exportMisconfigured("export+prune", "/var/exports")).toBe(false);
  });

  it("is not raised for families that do not export", () => {
    expect(exportMisconfigured("prune", "")).toBe(false);
    expect(exportMisconfigured("off", "")).toBe(false);
  });
});
