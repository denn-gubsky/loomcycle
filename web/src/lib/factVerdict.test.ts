import { describe, expect, it } from "vitest";
import { factActions, factVerdict, OPERATOR } from "./factVerdict";

describe("factVerdict", () => {
  it("a fact with no verdict is unverified, not refuted", () => {
    // The distinction the whole line rests on: never assessed is not the same as
    // assessed and rejected, and only the second withholds anything.
    expect(factVerdict({}).state).toBe("unverified");
    expect(factVerdict(undefined).state).toBe("unverified");
  });

  it("does not read a confidence as a verdict", () => {
    // A confidence can exist without anyone having judged — an extractor could supply
    // one. Treating that as "checked" would report a machine's guess as a decision.
    expect(factVerdict({ confidence: 0.9 }).state).toBe("unverified");
    expect(factVerdict({ confidence: 0.0 }).state).toBe("unverified");
  });

  it("uses the server's withheld flag rather than re-deriving it from the number", () => {
    // If this ever compared confidence against a floor copied into the browser, the two
    // copies would drift the first time the server's floor moved.
    expect(factVerdict({ judged_at: 1, withheld: true, confidence: 0.0 }).state).toBe("withheld");
    expect(factVerdict({ judged_at: 1, withheld: false, confidence: 0.4 }).state).toBe("checked");
    // Deliberately: a low confidence the server did NOT withhold still reads as checked.
    expect(factVerdict({ judged_at: 1, confidence: 0.1 }).state).toBe("checked");
  });

  it("reads who judged from the server's column, not from the reason text", () => {
    // The reason is free text a judge writes. Inferring provenance from it would let a
    // model that happens to start its reason with the marker word be rendered as a human.
    const v = factVerdict({
      judged_at: 1,
      judged_by: OPERATOR,
      judge_reason: "this is out of date",
    });
    expect(v.byOperator).toBe(true);
    expect(v.reason).toBe("this is out of date");
  });

  it("does not read a reason that merely mentions the operator as an operator verdict", () => {
    const v = factVerdict({
      judged_at: 1,
      judged_by: "memory/judge",
      judge_reason: "operator: the quote gives no duration",
    });
    expect(v.byOperator).toBe(false);
    expect(v.judgedBy).toBe("memory/judge");
  });

  it("names the agent that judged", () => {
    const v = factVerdict({ judged_at: 1, judged_by: "memory/judge", judge_reason: "no city" });
    expect(v.byOperator).toBe(false);
    expect(v.judgedBy).toBe("memory/judge");
    expect(v.reason).toBe("no city");
  });

  it("says nothing about who when the store does not know", () => {
    // A verdict recorded before the column existed. Claiming either party would be a
    // guess presented as a record.
    const v = factVerdict({ judged_at: 1, judge_reason: "an older verdict" });
    expect(v.byOperator).toBe(false);
    expect(v.judgedBy).toBe("");
  });

  it("reports no reason for an unverified fact even if one is somehow stored", () => {
    // Rendering a reason next to "unverified" would imply someone decided something.
    expect(factVerdict({ judge_reason: "stale leftover" }).reason).toBe("");
  });
});

describe("factActions", () => {
  it("offers restoring a withheld fact — the safety valve", () => {
    // The reason this panel exists: a judge that wrongly refuses a true fact must be
    // correctable by a person, and only "mark good" does that.
    const a = factActions(factVerdict({ judged_at: 1, withheld: true }));
    expect(a.canMarkGood).toBe(true);
    expect(a.canMarkWrong).toBe(false);
  });

  it("offers withholding a fact that is currently returned", () => {
    const a = factActions(factVerdict({ judged_at: 1, withheld: false }));
    expect(a.canMarkWrong).toBe(true);
    expect(a.canMarkGood).toBe(false);
  });

  it("offers both directions on an unverified fact", () => {
    const a = factActions(factVerdict({}));
    expect(a.canMarkWrong).toBe(true);
    expect(a.canMarkGood).toBe(true);
  });
});
