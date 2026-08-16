import { describe, expect, it } from "vitest";
import { coverageSummary } from "./coverage";

describe("coverageSummary", () => {
  it("an empty store reports no share at all, not 0%", () => {
    // 0% reads as "nothing here is verified". "There is nothing to verify" is a
    // different statement, and the one an operator would act on differently.
    expect(coverageSummary({ facts: 0 })).toEqual({
      empty: true,
      sharePct: null,
      showUnjudgedNote: false,
    });
    expect(coverageSummary(null).empty).toBe(true);
    expect(coverageSummary(undefined).empty).toBe(true);
  });

  it("uses the server's share and never recomputes one", () => {
    // supported/facts would be 0.5 here; the server says 0.25. The server's definition
    // is what the phase gate uses, so a second one computed in a client could disagree
    // with the number a decision is made on.
    expect(coverageSummary({ facts: 4, supported: 2, verified_share: 0.25 }).sharePct).toBe(25);
  });

  it("omits the share when facts exist but the server sent none", () => {
    expect(coverageSummary({ facts: 4 }).sharePct).toBeNull();
  });

  it("explains a zero judged count instead of leaving it looking broken", () => {
    expect(coverageSummary({ facts: 10, judged: 0 }).showUnjudgedNote).toBe(true);
    expect(coverageSummary({ facts: 10, judged: 3 }).showUnjudgedNote).toBe(false);
  });
});
