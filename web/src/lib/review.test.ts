import { describe, expect, it } from "vitest";
import type { RulingOutcome } from "@loomcycle/explorer";
import type { ReviewGroupCount } from "../api";
import {
  canUndo,
  filtersFromSearch,
  groupsFromSummary,
  judgeOf,
  reviewKey,
  reviewScopeFromSearch,
  reviewScopeId,
  rulingSelection,
  rulingSummary,
  searchFromFilters,
  undoSummary,
} from "./review";

describe("filters in the URL", () => {
  it("round-trips every filter field through the query string", () => {
    const f = {
      status: "proposed", kind: "cites", kinds: ["cites", "repeals"], method: "rule:citation", actor: "kb/linker",
      document_id: "d0", under_path: "/board", from_document_id: "d1", to_document_id: "d2",
      from_under_path: "/board/regulatory-external", to_under_path: "/board/internal",
      created_after: "2026-10-01T00:00:00Z", created_before: "1793145600000000000",
      min_confidence: 0.85, has_judge_verdict: true, judge_verdict: "accept",
      method_exact: "rule:citation", kind_exact: "cites", from_path_prefix: "/board/regulatory-external", from_unfiled: true,
    };
    expect(filtersFromSearch(searchFromFilters(f))).toEqual(f);
  });

  it("rewrites only the filter's parameters and keeps the rest of the URL", () => {
    const base = new URLSearchParams("tenant=globex&scope=user&kind=old&min_confidence=0.2");
    const p = searchFromFilters({ method: "rule:" }, base);
    expect(p.get("tenant")).toBe("globex");
    expect(reviewScopeFromSearch(p)).toBe("user");
    expect(p.get("kind")).toBeNull();
    expect(p.get("min_confidence")).toBeNull();
    expect(p.get("method")).toBe("rule:");
  });

  it("drops a malformed bound rather than guessing it", () => {
    const f = filtersFromSearch(new URLSearchParams("min_confidence=lots&has_judge_verdict=maybe&kinds=,%20,"));
    expect(f).toEqual({});
    expect(filtersFromSearch(new URLSearchParams("min_confidence=1.5"))).toEqual({});
  });

  it("reads the tenant scope unless the link names the user's own", () => {
    expect(reviewScopeFromSearch(new URLSearchParams(""))).toBe("tenant");
    expect(reviewScopeFromSearch(new URLSearchParams("scope=agent"))).toBe("tenant");
  });
});

describe("groups from the summary", () => {
  const g = (over: Partial<ReviewGroupCount> = {}): ReviewGroupCount => ({
    kind: "cites", method: "rule:citation", from_path_prefix: "/board/regulatory-external", judge_verdict: "", count: 1118, ...over,
  });

  it("takes each group's filter from the server, as it is", () => {
    const exact = { status: "proposed", kind_exact: "cites", method_exact: "rule:citation", from_path_prefix: "/board/regulatory-external", has_judge_verdict: false };
    const unfiled = { status: "proposed", kind_exact: "subject", method_exact: "classify", from_unfiled: true, judge_verdict: "accept" };
    const groups = groupsFromSummary(
      [g({ filter: exact }), g({ kind: "subject", method: "classify", from_path_prefix: "", judge_verdict: "accept", count: 239, filter: unfiled })],
      { status: "proposed" },
    );
    expect(groups.map((x) => [x.kind, x.count])).toEqual([["cites", 1118], ["subject", 239]]);
    expect(groups[0].filter).toEqual(exact);
    // Filed nowhere is a group of its own now, with its own exact filter.
    expect(groups[1].filter).toEqual(unfiled);
    expect(new Set(groups.map((x) => x.key)).size).toBe(2);
  });

  it("puts the page's own time bounds back over the server's nanosecond numbers", () => {
    // 1793145600123456789 is not a JS number: it arrives rounded.
    const fromServer = { kind_exact: "cites", created_before: 1793145600123456800 as unknown as string, created_after: 1 as unknown as string };
    const [grp] = groupsFromSummary([g({ filter: fromServer })], { created_before: "1793145600123456789" });
    expect(grp.filter).toEqual({ kind_exact: "cites", created_before: "1793145600123456789" });
  });

  it("offers no group action when the server sends no filter", () => {
    const [grp] = groupsFromSummary([g()], { status: "proposed" });
    expect(grp.filter).toBeUndefined();
  });

  it("reads a judge verdict only when one is recorded", () => {
    expect(judgeOf({ id: "s", from_id: "a", to_id: "b", kind: "cites" })).toBeUndefined();
    expect(
      judgeOf({ id: "s", from_id: "a", to_id: "b", kind: "cites", attributes: { judge: { verdict: "reject", confidence: 0.91, rationale: "amending act" } } }),
    ).toEqual({ verdict: "reject", confidence: 0.91, rationale: "amending act" });
  });
});

describe("whose user documents", () => {
  it("names the picked subject only for a user-scope review of someone else", () => {
    expect(reviewScopeId("user", "bob", "ada")).toBe("bob");
    expect(reviewScopeId("user", "ada", "ada")).toBeUndefined();
    expect(reviewScopeId("user", undefined, "ada")).toBeUndefined();
    // The tenant scope names nobody: the runtime refuses scope_id there.
    expect(reviewScopeId("tenant", "bob", "ada")).toBeUndefined();
  });
});

describe("earlier rulings", () => {
  const rec = { action: "accept", reverted: false, ruled: 3 };

  it("offers Undo for an accept or reject not yet undone, never for a revert", () => {
    expect(canUndo(rec)).toBe(true);
    expect(canUndo({ ...rec, action: "reject" })).toBe(true);
    expect(canUndo({ ...rec, reverted: true })).toBe(false);
    expect(canUndo({ ...rec, action: "revert" })).toBe(false);
    expect(canUndo({ ...rec, ruled: 0 })).toBe(false);
  });

  it("says what a ruling selected without naming statements", () => {
    expect(rulingSelection({ action: "accept", selector: { filter: { kind_exact: "cites", has_judge_verdict: false } } })).toBe(
      "kind_exact=cites has_judge_verdict=false",
    );
    expect(rulingSelection({ action: "reject", selector: { ids_count: 4 } })).toBe("4 picked by hand");
    expect(rulingSelection({ action: "accept", selector: {} })).toBe("all proposals");
    expect(rulingSelection({ action: "revert", selector: { reverts: "7e8eb2ec3a2b971c" } })).toBe("undid 7e8eb2ec");
  });
});

describe("keyboard review", () => {
  it("moves with j and k within the rows", () => {
    expect(reviewKey({ key: "j" }, -1, 3)).toEqual({ type: "move", index: 0 });
    expect(reviewKey({ key: "j" }, 0, 3)).toEqual({ type: "move", index: 1 });
    expect(reviewKey({ key: "j" }, 2, 3)).toEqual({ type: "move", index: 2 });
    expect(reviewKey({ key: "k" }, 2, 3)).toEqual({ type: "move", index: 1 });
    expect(reviewKey({ key: "k" }, 0, 3)).toEqual({ type: "move", index: 0 });
  });

  it("rules and expands the focused row with a, r and e", () => {
    expect(reviewKey({ key: "a" }, 1, 3)).toEqual({ type: "accept", index: 1 });
    expect(reviewKey({ key: "r" }, 1, 3)).toEqual({ type: "reject", index: 1 });
    expect(reviewKey({ key: "e" }, 1, 3)).toEqual({ type: "expand", index: 1 });
    expect(reviewKey({ key: "a" }, -1, 3)).toBeNull();
  });

  it("leaves keys alone while typing or with a modifier", () => {
    expect(reviewKey({ key: "a", editing: true }, 0, 3)).toBeNull();
    expect(reviewKey({ key: "r", metaKey: true }, 0, 3)).toBeNull();
    expect(reviewKey({ key: "j", ctrlKey: true }, 0, 3)).toBeNull();
    expect(reviewKey({ key: "x" }, 0, 3)).toBeNull();
    expect(reviewKey({ key: "j" }, -1, 0)).toBeNull();
  });
});

describe("what a ruling and its undo report", () => {
  const outcome = (over: Partial<RulingOutcome> = {}): RulingOutcome => ({
    rulingIds: ["r1"], ruled: 2, failed: 0, skipped: 0, total: 2, cancelled: false, failures: [], ...over,
  });

  it("says what was ruled, what did not take it and what was skipped", () => {
    expect(rulingSummary("reject", outcome())).toBe("Rejected 2 statements.");
    expect(rulingSummary("accept", outcome({ ruled: 1000, failed: 3, skipped: 12, cancelled: true }))).toBe(
      "Accepted 1,000 statements; 3 did not take the ruling; 12 skipped: not yours to rule; stopped before the rest.",
    );
  });

  it("says what came back and what had changed since", () => {
    expect(undoSummary({ reverted: 1980, changed: 20, errors: ["ruling_not_found"] })).toBe(
      "1,980 statements came back to review; 20 changed since and were left as ruled; 1 ruling could not be undone.",
    );
    expect(undoSummary({ reverted: 1, changed: 0, errors: [] })).toBe("1 statement came back to review.");
  });
});
