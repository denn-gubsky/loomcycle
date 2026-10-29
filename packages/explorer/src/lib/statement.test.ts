import { describe, it, expect } from "vitest";
import type { EdgeEvidence } from "../types";
import {
  clipQuote,
  evidencePreview,
  formatNsDate,
  isProposed,
  methodLabel,
  statusLabel,
  validityLabel,
  verdictHint,
} from "./statement";

const ns = (iso: string) => Date.parse(iso) * 1e6;
const ev = (id: string, quote = `quote ${id}`): EdgeEvidence => ({ id, quote, created_at: 0 });

describe("statement display helpers", () => {
  it("badges only a status that is not plainly asserted", () => {
    expect(statusLabel({ status: "proposed" })).toBe("proposed");
    expect(statusLabel({ status: "rejected" })).toBe("rejected");
    expect(statusLabel({ status: "asserted" })).toBeUndefined();
    expect(statusLabel({})).toBeUndefined(); // an older server's edge
  });

  it("names the method only when it is not manual or a wikilink", () => {
    expect(methodLabel({ method: "import" })).toBe("import");
    expect(methodLabel({ method: "runtime" })).toBe("runtime");
    expect(methodLabel({ method: "manual" })).toBeUndefined();
    expect(methodLabel({ method: "wikilink" })).toBeUndefined();
    expect(methodLabel({})).toBeUndefined();
  });

  it("a proposal is reviewable only with an id to rule on", () => {
    expect(isProposed({ id: "e1", status: "proposed" })).toBe(true);
    expect(isProposed({ status: "proposed" })).toBe(false);
    expect(isProposed({ id: "e1", status: "asserted" })).toBe(false);
  });

  it("formats nanosecond timestamps as UTC dates", () => {
    expect(formatNsDate(ns("2026-03-04T23:30:00Z"))).toBe("2026-03-04");
  });

  it("describes validity from either or both ends", () => {
    const from = ns("2026-01-02T00:00:00Z");
    const until = ns("2026-03-04T00:00:00Z");
    expect(validityLabel({ valid_at: from, invalid_at: until })).toBe("valid 2026-01-02 → 2026-03-04");
    expect(validityLabel({ valid_at: from })).toBe("valid from 2026-01-02");
    expect(validityLabel({ invalid_at: until })).toBe("valid until 2026-03-04");
    expect(validityLabel({})).toBeUndefined();
  });

  it("summarizes a ruling for its tooltip, and nothing when unjudged", () => {
    const at = ns("2026-10-29T10:00:00Z");
    expect(verdictHint({ judged_at: at, judged_by: "operator", judge_reason: "wrong target" })).toBe(
      "judged by operator on 2026-10-29: wrong target",
    );
    expect(verdictHint({ judged_at: at })).toBe("judged by a judge on 2026-10-29");
    expect(verdictHint({ judge_reason: "orphan reason" })).toBeUndefined();
  });

  it("previews the first quotes and counts the rest against evidence_count", () => {
    expect(evidencePreview({ evidence: [ev("a"), ev("b"), ev("c")], evidence_count: 7 })).toEqual({
      rows: [ev("a"), ev("b"), ev("c")],
      more: 4,
    });
    expect(evidencePreview({ evidence: [ev("a")], evidence_count: 1 })).toEqual({ rows: [ev("a")], more: 0 });
    expect(evidencePreview({}).rows).toEqual([]);
    // a get_edge statement carries every row: the preview still caps what it shows
    const all = { evidence: [ev("a"), ev("b"), ev("c"), ev("d")], evidence_count: 4 };
    expect(evidencePreview(all).more).toBe(1);
  });

  it("clips a long quote and folds its whitespace", () => {
    expect(clipQuote("  a\n\nb  ")).toBe("a b");
    const long = "x".repeat(400);
    const clipped = clipQuote(long);
    expect(clipped).toHaveLength(280);
    expect(clipped.endsWith("…")).toBe(true);
  });
});
