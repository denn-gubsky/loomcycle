import { describe, it, expect } from "vitest";
import type { ClassifyAnswer } from "../types";
import {
  acceptSummary,
  canApply,
  candidateDetail,
  classDisposition,
  classificationRecord,
  classifyErrorText,
  formatShare,
  pendingClassification,
  resultLabel,
  stepLabel,
  summarizeStep,
} from "./classify";

const answer = (over: Partial<ClassifyAnswer> = {}): ClassifyAnswer => ({
  document_id: "d1",
  write: "report",
  result: "unclassified",
  live: false,
  issued_by: [],
  concepts: [],
  steps: [],
  ...over,
});

describe("pending classification", () => {
  it("is pending on a proposed type alone", () => {
    expect(pendingClassification({ classification: { by: "classify", at: 1, proposed_type: "Law" } })).toEqual({
      proposedType: "Law",
      proposedTags: [],
      statementCount: 0,
    });
  });

  it("is pending on proposed statements alone", () => {
    expect(pendingClassification({ classification: { statements: ["s1", "s2"] } })).toEqual({
      proposedType: undefined,
      proposedTags: [],
      statementCount: 2,
    });
  });

  it("is pending on proposed tags alone", () => {
    expect(pendingClassification({ classification: { proposed_tags: ["topic/tax"] } })).toEqual({
      proposedType: undefined,
      proposedTags: ["topic/tax"],
      statementCount: 0,
    });
  });

  it("is not pending when the record only says what was applied, or proposes nothing", () => {
    expect(pendingClassification({ classification: { by: "classify", at: 1, applied_type: "Law" } })).toBeUndefined();
    expect(pendingClassification({ classification: { proposed_type: "", statements: [], proposed_tags: [] } })).toBeUndefined();
  });

  it("is not pending without a record or with a malformed one", () => {
    expect(pendingClassification(undefined)).toBeUndefined();
    expect(pendingClassification({ other: 1 })).toBeUndefined();
    expect(pendingClassification({ classification: "Law" })).toBeUndefined();
    expect(pendingClassification({ classification: ["Law"] })).toBeUndefined();
    // wrongly-typed fields are dropped, not trusted
    expect(pendingClassification({ classification: { proposed_type: 7, statements: [1, 2], proposed_tags: "x" } })).toBeUndefined();
  });

  it("classificationRecord keeps the well-typed fields", () => {
    expect(
      classificationRecord({
        classification: { by: "classify", at: 5, applied_type: "Order", proposed_fields: { number: "12" }, extra: true },
      }),
    ).toEqual({ by: "classify", at: 5, applied_type: "Order", proposed_fields: { number: "12" } });
  });
});

describe("classify answer labels", () => {
  it("labels each result, reading a report as what Apply would do", () => {
    expect(resultLabel({ result: "classified", write: "propose" })).toBe("classified");
    expect(resultLabel({ result: "classified", write: "report" })).toBe("would be classified");
    expect(resultLabel({ result: "proposed", write: "propose" })).toBe("proposed — waits for review");
    expect(resultLabel({ result: "proposed", write: "report" })).toBe("would be proposed for review");
    expect(resultLabel({ result: "kept", write: "report" })).toBe("kept — the type was set by hand");
    expect(resultLabel({ result: "unclassified", write: "propose" })).toBe("unclassified — nothing decided");
  });

  it("says whether the class was applied or proposed, and never applied when kept", () => {
    expect(classDisposition({ class: "Law", live: true, result: "classified", write: "propose" })).toBe("applied");
    expect(classDisposition({ class: "Law", live: true, result: "classified", write: "report" })).toBe("would be applied");
    expect(classDisposition({ class: "Law", live: false, result: "proposed", write: "propose" })).toBe("proposed");
    expect(classDisposition({ class: "Law", live: false, result: "proposed", write: "report" })).toBe("would be proposed");
    expect(classDisposition({ class: "Law", live: false, result: "kept", write: "propose" })).toBe("not applied");
    expect(classDisposition({ live: false, result: "unclassified", write: "report" })).toBeUndefined();
  });

  it("offers Apply only on a report that would write something", () => {
    expect(canApply(answer({ result: "classified", live: true }))).toBe(true);
    expect(canApply(answer({ result: "proposed" }))).toBe(true);
    expect(canApply(answer({ result: "kept", concepts: [{ concept: "Tax", tag: "topic/tax", share: 0.5 }] }))).toBe(true);
    expect(canApply(answer({ result: "unclassified" }))).toBe(false);
    expect(canApply(answer({ result: "kept" }))).toBe(false);
    expect(canApply(answer({ result: "classified", write: "propose", live: true }))).toBe(false);
  });

  it("formats shares as whole percentages", () => {
    expect(formatShare(0.6)).toBe("60%");
    expect(formatShare(1)).toBe("100%");
    expect(formatShare(undefined)).toBe("");
  });
});

describe("classify steps", () => {
  it("names known steps and shows an unknown one verbatim", () => {
    expect(stepLabel("rules")).toBe("Rules");
    expect(stepLabel("vote")).toBe("Neighbourhood vote");
    expect(stepLabel("model")).toBe("Model");
    expect(stepLabel("oracle")).toBe("oracle");
  });

  it("explains a rule candidate by its quote and a vote candidate by its count", () => {
    expect(candidateDetail({ class: "Law", quote: "Закон № 2297-VI", from: "title" })).toBe("“Закон № 2297-VI” (title)");
    expect(candidateDetail({ class: "Law", votes: 3, share: 0.75 })).toBe("3 votes · 75%");
    expect(candidateDetail({ class: "Law", votes: 1 })).toBe("1 vote");
    expect(candidateDetail({ class: "Law" })).toBe("");
  });

  it("summarizes a model step with its method when present", () => {
    expect(
      summarizeStep({
        step: "model",
        method: "decision",
        chosen: "Order",
        reason: "the body reads as an order",
        candidates: [{ class: "Order", share: 0.82 }, { class: "Law", share: 0.18 }],
      }),
    ).toEqual({
      label: "Model",
      chosen: "Order",
      method: "decision",
      reason: "the body reads as an order",
      candidates: [
        { class: "Order", detail: "82%" },
        { class: "Law", detail: "18%" },
      ],
    });
  });

  it("summarizes a step that chose nothing, with no candidates field", () => {
    expect(summarizeStep({ step: "vote", reason: "no embedder" })).toEqual({
      label: "Neighbourhood vote",
      reason: "no embedder",
      candidates: [],
    });
  });
});

describe("accept_classification summary", () => {
  it("headlines what became live", () => {
    expect(acceptSummary({ document_id: "d1", type: "Law", accepted_statements: ["s1"], tags: ["topic/tax"] })).toEqual({
      applied: "Accepted type Law, 1 statement, tags topic/tax.",
      notices: [],
    });
  });

  it("notes a class left pending and every skipped item", () => {
    const s = acceptSummary({
      document_id: "d1",
      type_pending: true,
      accepted_statements: [],
      skipped_statements: ["s1", "s2"],
      skipped_type: "Memo",
      skipped_tags: ["junk"],
      warnings: ["identity_collision: law:1 is held"],
    });
    expect(s.applied).toBe("Nothing was accepted.");
    expect(s.notices).toEqual([
      "The proposed class stays pending: another document already holds its identity.",
      "Class Memo was not applied: it is not a declared document class.",
      "2 statements were skipped (already ruled on, or not this document's proposal).",
      "Tags not applied (not concepts of the ontology): junk.",
      "identity_collision: law:1 is held",
    ]);
  });

  it("puts nothing_pending in plain words and passes other refusals through", () => {
    expect(
      classifyErrorText(new Error("accept_classification: nothing_pending: the document has no pending classification")),
    ).toBe("Nothing is pending for this document — it may already have been accepted.");
    expect(classifyErrorText(new Error("permission denied"))).toBe("permission denied");
    expect(classifyErrorText("boom")).toBe("boom");
  });
});
