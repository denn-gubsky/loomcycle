import { describe, expect, it, vi } from "vitest";
import type { ReviewGroupCount, RuleStatementsRequest, RuleStatementsResponse } from "../api";
import { ApiError } from "./apiError";
import {
  chunkCount,
  filtersFromSearch,
  groupFilter,
  groupsFromSummary,
  judgeOf,
  reviewKey,
  reviewScopeFromSearch,
  rulingSummary,
  runRuling,
  searchFromFilters,
  undoRulings,
  undoSummary,
  type ConfirmReason,
} from "./review";

// fakeQueue models the runtime's ruling over a queue of `size` proposals: a dry
// run reports what is left, a real call refuses an expect_count that is not
// the current match count (queue_changed) and rules the first `window` of it.
// `onRule` runs before each real call, to change the queue under the ruling.
function fakeQueue(
  size: number,
  opts: { window?: number; onRule?: (call: number) => void; afterRule?: (call: number) => void; skipped?: number } = {},
) {
  const window = opts.window ?? 1000;
  const q = { left: size, calls: [] as RuleStatementsRequest[], real: 0 };
  const result = (dry: boolean, over: Partial<RuleStatementsResponse> = {}): RuleStatementsResponse => {
    const selected = Math.min(q.left, window);
    const skipped = Math.min(opts.skipped ?? 0, selected);
    return {
      tenant: "acme", scope: "tenant", action: "accept", dry_run: dry,
      matched: q.left, selected, skipped, to_rule: selected - skipped, ruled: 0, failed: 0,
      breakdown: [], sample: dry ? [] : undefined, ...over,
    };
  };
  const rule = vi.fn(async (req: RuleStatementsRequest): Promise<RuleStatementsResponse> => {
    q.calls.push(req);
    if (req.dry_run) return result(true);
    q.real++;
    opts.onRule?.(q.real);
    if (req.expect_count !== q.left) {
      throw new ApiError("409 Conflict: queue changed", 409, "queue_changed");
    }
    const r = result(false);
    q.left -= r.to_rule;
    opts.afterRule?.(q.real);
    return { ...r, ruled: r.to_rule, ruling_id: `r${q.real}` };
  });
  return { q, api: { rule } };
}

describe("filters in the URL", () => {
  it("round-trips every filter field through the query string", () => {
    const f = {
      status: "proposed", kind: "cites", kinds: ["cites", "repeals"], method: "rule:citation", actor: "kb/linker",
      document_id: "d0", under_path: "/board", from_document_id: "d1", to_document_id: "d2",
      from_under_path: "/board/regulatory-external", to_under_path: "/board/internal",
      created_after: "2026-10-01T00:00:00Z", created_before: "1793145600000000000",
      min_confidence: 0.85, has_judge_verdict: true, judge_verdict: "accept",
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

  it("makes one group per summary row, each with the filter that selects it", () => {
    const groups = groupsFromSummary([g(), g({ kind: "repeals", count: 34, judge_verdict: "accept" })], { status: "proposed" });
    expect(groups.map((x) => [x.kind, x.count])).toEqual([["cites", 1118], ["repeals", 34]]);
    expect(groups[0].filter).toEqual({
      status: "proposed", kind: "cites", method: "rule:citation",
      from_under_path: "/board/regulatory-external", has_judge_verdict: false,
    });
    expect(groups[1].filter).toEqual({
      status: "proposed", kind: "repeals", method: "rule:citation",
      from_under_path: "/board/regulatory-external", judge_verdict: "accept",
    });
    expect(new Set(groups.map((x) => x.key)).size).toBe(2);
  });

  it("keeps the page's narrower path and method, and takes the group's where it is narrower", () => {
    expect(groupFilter(g(), { from_under_path: "/board/regulatory-external/laws", method: "rule:" })).toMatchObject({
      from_under_path: "/board/regulatory-external/laws", method: "rule:citation",
    });
    expect(groupFilter(g(), { from_under_path: "/board" })).toMatchObject({ from_under_path: "/board/regulatory-external" });
  });

  it("names no filter for a group the queue's filters would widen", () => {
    // Filed nowhere: an empty from_under_path is "any path", the whole queue.
    expect(groupFilter(g({ from_path_prefix: "" }), {})).toBeUndefined();
    expect(groupFilter(g({ method: "" }), {})).toBeUndefined();
    // A document filed twice can land in a group outside the page's path.
    expect(groupFilter(g({ from_path_prefix: "/other/place" }), { from_under_path: "/board" })).toBeUndefined();
    // "/board/regulatory" is not a parent of "/board/regulatory-external".
    expect(groupFilter(g(), { from_under_path: "/board/regulatory" })).toBeUndefined();
    expect(groupFilter(g(), { has_judge_verdict: true })).toBeUndefined();
  });

  it("reads a judge verdict only when one is recorded", () => {
    expect(judgeOf({ id: "s", from_id: "a", to_id: "b", kind: "cites" })).toBeUndefined();
    expect(
      judgeOf({ id: "s", from_id: "a", to_id: "b", kind: "cites", attributes: { judge: { verdict: "reject", confidence: 0.91, rationale: "amending act" } } }),
    ).toEqual({ verdict: "reject", confidence: 0.91, rationale: "amending act" });
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

describe("runRuling", () => {
  it("runs the dry run first and rules with its count as expect_count", async () => {
    const { q, api } = fakeQueue(40);
    const confirm = vi.fn(async (_dry: RuleStatementsResponse, _why: ConfirmReason) => true);
    const out = await runRuling(api, { action: "accept", filter: { kind: "cites" } }, { confirm });
    expect(q.calls.map((c) => c.dry_run)).toEqual([true, false]);
    expect(q.calls[0]).toEqual({ action: "accept", filter: { kind: "cites" }, dry_run: true });
    expect(q.calls[1].expect_count).toBe(40);
    expect(confirm).toHaveBeenCalledTimes(1);
    expect(confirm.mock.calls[0][0]).toMatchObject({ dry_run: true, matched: 40 });
    expect(out).toMatchObject({ ruled: 40, total: 40, rulingIds: ["r1"], cancelled: false });
  });

  it("rules nothing when the reviewer declines the dry run", async () => {
    const { q, api } = fakeQueue(40);
    const out = await runRuling(api, { action: "reject", filter: {}, reason: "noise" }, { confirm: async () => false });
    expect(q.calls).toHaveLength(1);
    expect(q.calls[0]).toMatchObject({ dry_run: true, reason: "noise" });
    expect(out).toMatchObject({ ruled: 0, cancelled: true, rulingIds: [] });
  });

  it("re-runs the dry run and asks again when the queue changed under it", async () => {
    // Two proposals arrive between the first dry run and its ruling.
    const fq = fakeQueue(40, { onRule: (n) => n === 1 && (fq.q.left += 2) });
    const reasons: ConfirmReason[] = [];
    const counts: number[] = [];
    const out = await runRuling(fq.api, { action: "accept", filter: {} }, {
      confirm: async (d, why) => {
        reasons.push(why);
        counts.push(d.matched);
        return true;
      },
    });
    expect(reasons).toEqual(["first", "queue_changed"]);
    expect(counts).toEqual([40, 42]);
    expect(fq.q.calls.map((c) => [c.dry_run, c.expect_count])).toEqual([
      [true, undefined], [false, 40], [true, undefined], [false, 42],
    ]);
    expect(out).toMatchObject({ ruled: 42, total: 42 });
  });

  it("gives up on a queue that keeps changing and reports the refusal", async () => {
    const fq = fakeQueue(10, { onRule: () => (fq.q.left += 1) });
    const out = await runRuling(fq.api, { action: "accept", filter: {} }, { confirm: async () => true });
    expect(out.ruled).toBe(0);
    expect(out.error).toBeInstanceOf(ApiError);
    expect((out.error as ApiError).code).toBe("queue_changed");
  });

  it("rules over 1000 matches in chunks: dry run, rule, dry run", async () => {
    const { q, api } = fakeQueue(2500);
    const progress = vi.fn();
    const confirm = vi.fn(async () => true);
    const out = await runRuling(api, { action: "accept", filter: { method: "rule:" } }, { confirm, onProgress: progress });
    expect(q.calls.map((c) => (c.dry_run ? "dry" : `rule:${c.expect_count}`))).toEqual([
      "dry", "rule:2500", "dry", "rule:1500", "dry", "rule:500",
    ]);
    expect(confirm).toHaveBeenCalledTimes(1);
    expect(progress.mock.calls.map((c) => c[0])).toEqual([
      { ruled: 1000, failed: 0, total: 2500, chunks: 1 },
      { ruled: 2000, failed: 0, total: 2500, chunks: 2 },
      { ruled: 2500, failed: 0, total: 2500, chunks: 3 },
    ]);
    expect(out).toMatchObject({ ruled: 2500, rulingIds: ["r1", "r2", "r3"] });
    expect(chunkCount(2500)).toBe(3);
  });

  it("asks again when the queue grew past what was confirmed while chunking", async () => {
    // Seven proposals arrive once the first chunk is ruled.
    const fq = fakeQueue(1500, { afterRule: (n) => n === 1 && (fq.q.left += 7) });
    const reasons: ConfirmReason[] = [];
    const out = await runRuling(fq.api, { action: "accept", filter: {} }, {
      confirm: async (_d, why) => {
        reasons.push(why);
        return why === "first";
      },
    });
    // 1500 were confirmed and 1000 ruled; the next dry run finds 507, not 500.
    expect(reasons).toEqual(["first", "grew"]);
    expect(out).toMatchObject({ ruled: 1000, cancelled: true, rulingIds: ["r1"] });
  });

  it("stops when a chunk rules nothing rather than looping", async () => {
    const { q, api } = fakeQueue(3000, { skipped: 1000 });
    const out = await runRuling(api, { action: "accept", filter: {} });
    expect(q.calls).toHaveLength(1);
    expect(out).toMatchObject({ ruled: 0, skipped: 1000, rulingIds: [] });
  });

  it("keeps what was ruled before a call failed", async () => {
    const fq = fakeQueue(1500);
    const rule = fq.api.rule;
    let real = 0;
    const api = {
      rule: async (req: RuleStatementsRequest) => {
        if (!req.dry_run && ++real === 2) throw new ApiError("503 Service Unavailable: down", 503, "unavailable");
        return rule(req);
      },
    };
    const out = await runRuling(api, { action: "accept", filter: {} });
    expect(out.rulingIds).toEqual(["r1"]);
    expect(out.ruled).toBe(1000);
    expect((out.error as ApiError).status).toBe(503);
  });

  it("names hand-picked statements by id and asks nothing without a confirm", async () => {
    const { q, api } = fakeQueue(2);
    const out = await runRuling(api, { action: "reject", ids: ["s1", "s2"], reason: "wrong target", scope: "user" });
    expect(q.calls[0]).toEqual({ action: "reject", ids: ["s1", "s2"], reason: "wrong target", scope: "user", dry_run: true });
    expect(q.calls[1]).toMatchObject({ ids: ["s1", "s2"], dry_run: false, expect_count: 2 });
    expect(out.ruled).toBe(2);
    expect(rulingSummary("reject", out)).toBe("Rejected 2 statements.");
  });
});

describe("undo", () => {
  it("reverts every ruling of a chunked ruling, newest first, and adds up what came back", async () => {
    const order: string[] = [];
    const revert = vi.fn(async (id: string) => {
      order.push(id);
      if (id === "r2") throw new ApiError("404 Not Found: ruling_not_found", 404, "ruling_not_found");
      return { tenant: "acme", scope: "tenant", ruling_id: id, action: "accept", reverted: 990, changed: 10, skipped: 0, items: [] };
    });
    const u = await undoRulings(revert, ["r1", "r2", "r3"]);
    expect(order).toEqual(["r3", "r2", "r1"]);
    expect(u).toMatchObject({ reverted: 1980, changed: 20 });
    expect(u.errors).toHaveLength(1);
    expect(undoSummary(u)).toBe(
      "1,980 statements came back to review; 20 changed since and were left as ruled; 1 ruling could not be undone.",
    );
    expect(undoSummary({ reverted: 1, changed: 0, errors: [] })).toBe("1 statement came back to review.");
  });
});
