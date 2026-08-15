import { describe, expect, it } from "vitest";
import { verificationSummary } from "./verificationSummary";

describe("verificationSummary", () => {
  it("an empty store reports no share at all, not 0%", () => {
    // 0% reads as "nothing here is verified". "There is nothing to verify" is a
    // different statement, and the one an operator would act on differently.
    const s = verificationSummary({ facts: 0 });
    expect(s.empty).toBe(true);
    expect(s.sharePct).toBeNull();
  });

  it("a null response is treated as empty rather than as zero coverage", () => {
    expect(verificationSummary(null).empty).toBe(true);
    expect(verificationSummary(null).sharePct).toBeNull();
  });

  it("uses the server's share and never recomputes one", () => {
    // supported/facts would be 0.5 here; the server says 0.25. The server's definition
    // is what the gate uses, so a second one computed in the browser could disagree
    // with the number a decision is made on.
    const s = verificationSummary({ facts: 4, supported: 2, verified_share: 0.25 });
    expect(s.sharePct).toBe(25);
  });

  it("omits the share when facts exist but the server sent none", () => {
    expect(verificationSummary({ facts: 4 }).sharePct).toBeNull();
  });

  it("explains a zero judged count instead of leaving it looking broken", () => {
    expect(verificationSummary({ facts: 10, judged: 0 }).showUnjudgedNote).toBe(true);
    expect(verificationSummary({ facts: 10, judged: 3 }).showUnjudgedNote).toBe(false);
  });

  it("says withheld means hidden only when something actually is", () => {
    expect(verificationSummary({ facts: 10, judged: 5, withheld: 2 }).showWithheldNote).toBe(true);
    expect(verificationSummary({ facts: 10, judged: 5, withheld: 0 }).showWithheldNote).toBe(false);
    expect(verificationSummary({ facts: 10, judged: 5 }).showWithheldNote).toBe(false);
  });
});
