import { describe, expect, it, vi } from "vitest";
import {
  chunkCount,
  runRuling,
  undoRulings,
  type ConfirmReason,
  type RulingCounts,
  type RulingRequest,
} from "./ruling";

// A refusal as a client throws it: a message, the HTTP status and the code.
class Refusal extends Error {
  constructor(
    message: string,
    readonly status: number,
    readonly code: string,
  ) {
    super(message);
  }
}

type Filter = Record<string, unknown>;
type RuleStatementsRequest = RulingRequest<Filter>;
type RuleStatementsResponse = RulingCounts & {
  tenant: string;
  scope: string;
  action: string;
  breakdown: unknown[];
  sample?: unknown[];
};

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
      throw new Refusal("409 Conflict: queue changed", 409, "queue_changed");
    }
    const r = result(false);
    q.left -= r.to_rule;
    opts.afterRule?.(q.real);
    return { ...r, ruled: r.to_rule, ruling_id: `r${q.real}` };
  });
  return { q, api: { rule } };
}

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
    expect(out.error).toBeInstanceOf(Refusal);
    expect((out.error as Refusal).code).toBe("queue_changed");
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
        if (!req.dry_run && ++real === 2) throw new Refusal("503 Service Unavailable: down", 503, "unavailable");
        return rule(req);
      },
    };
    const out = await runRuling(api, { action: "accept", filter: {} });
    expect(out.rulingIds).toEqual(["r1"]);
    expect(out.ruled).toBe(1000);
    expect((out.error as Refusal).status).toBe(503);
  });

  it("names hand-picked statements by id and asks nothing without a confirm", async () => {
    const { q, api } = fakeQueue(2);
    const out = await runRuling(api, { action: "reject", ids: ["s1", "s2"], reason: "wrong target", scope: "user" });
    expect(q.calls[0]).toEqual({ action: "reject", ids: ["s1", "s2"], reason: "wrong target", scope: "user", dry_run: true });
    expect(q.calls[1]).toMatchObject({ ids: ["s1", "s2"], dry_run: false, expect_count: 2 });
    expect(out.ruled).toBe(2);
  });
});

describe("undo", () => {
  it("reverts every ruling of a chunked ruling, newest first, and adds up what came back", async () => {
    const order: string[] = [];
    const revert = vi.fn(async (id: string) => {
      order.push(id);
      if (id === "r2") throw new Refusal("404 Not Found: ruling_not_found", 404, "ruling_not_found");
      return { tenant: "acme", scope: "tenant", ruling_id: id, action: "accept", reverted: 990, changed: 10, skipped: 0, items: [] };
    });
    const u = await undoRulings(revert, ["r1", "r2", "r3"]);
    expect(order).toEqual(["r3", "r2", "r1"]);
    expect(u).toMatchObject({ reverted: 1980, changed: 20 });
    expect(u.errors).toHaveLength(1);
  });
});
