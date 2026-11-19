import { describe, expect, it, vi } from "vitest";

import { ApiError } from "./apiError";
import { applyWithPlan, retireConfirmText, retireWithPlan, type OntologyPlanResult } from "./ontologyRetirePlan";

function plan(fingerprint: string, n = 3): OntologyPlanResult {
  return {
    fingerprint,
    complete: true,
    changes: [
      {
        op: "retire",
        entry: { chunk_id: "c1", name: "Memo", section: "classes", kind: "class" },
        risk: n > 0 ? "stales" : "none",
        counts: {
          typed_documents: { n, effect: "stale", tenant_scope: n },
          statements: { n: 0, effect: "stale", tenant_scope: 0 },
        },
        subtree: [{ chunk_id: "c2", name: "Board Memo", kind: "class" }],
      },
    ],
  };
}

describe("retireConfirmText", () => {
  it("names the non-zero counts and the subtree retired with the entry", () => {
    const text = retireConfirmText("Memo", plan("f1"));
    expect(text).toContain('Retire "Memo"?');
    expect(text).toContain("What rests on it: typed documents: 3.");
    expect(text).not.toContain("statements");
    expect(text).toContain('Retired with it: class "Board Memo".');
  });

  it("says when nothing rests on the entry, and when the count is incomplete", () => {
    const p = { ...plan("f1", 0), complete: false, scopes_skipped: 2 };
    const text = retireConfirmText("Memo", p);
    expect(text).toContain("Nothing rests on it.");
    expect(text).toContain("2 skipped");
  });
});

describe("retireWithPlan", () => {
  it("sends the plan's fingerprint after the operator confirms", async () => {
    const retire = vi.fn(async (fp: string) => `retired with ${fp}`);
    const out = await retireWithPlan("Memo", { plan: async () => plan("f1"), retire, confirm: () => true });
    expect(out).toBe("retired with f1");
    expect(retire).toHaveBeenCalledWith("f1");
  });

  it("retires nothing when the operator declines", async () => {
    const retire = vi.fn();
    const out = await retireWithPlan("Memo", { plan: async () => plan("f1"), retire, confirm: () => false });
    expect(out).toBeNull();
    expect(retire).not.toHaveBeenCalled();
  });

  it("re-plans once on plan_stale and retires with the new fingerprint", async () => {
    const plans = [plan("old"), plan("new", 4)];
    const confirm = vi.fn((_text: string) => true);
    const retire = vi.fn(async (fp: string) => {
      if (fp === "old") throw new ApiError("plan_stale", 409, "plan_stale");
      return fp;
    });
    const out = await retireWithPlan("Memo", { plan: async () => plans.shift()!, retire, confirm });
    expect(out).toBe("new");
    expect(confirm).toHaveBeenCalledTimes(2);
    expect(confirm.mock.calls[1]![0]).toContain("typed documents: 4");
  });

  it("throws a second plan_stale and any other refusal", async () => {
    const stale = new ApiError("plan_stale", 409, "plan_stale");
    await expect(
      retireWithPlan("Memo", { plan: async () => plan("f"), retire: async () => Promise.reject(stale), confirm: () => true }),
    ).rejects.toBe(stale);
    const locked = new ApiError("entry_locked", 422, "tool_refused");
    const retire = vi.fn(async () => Promise.reject(locked));
    await expect(retireWithPlan("Memo", { plan: async () => plan("f"), retire, confirm: () => true })).rejects.toBe(locked);
    expect(retire).toHaveBeenCalledTimes(1);
  });
});

describe("applyWithPlan", () => {
  it("applies with the plan's fingerprint and re-plans once on plan_stale", async () => {
    const plans = [plan("old"), plan("new")];
    const apply = vi.fn(async (fp: string) => {
      if (fp === "old") throw new ApiError("plan_stale", 409, "plan_stale");
      return `accepted with ${fp}`;
    });
    const out = await applyWithPlan(() => "ok?", { plan: async () => plans.shift()!, apply, confirm: () => true });
    expect(out).toBe("accepted with new");
    expect(apply).toHaveBeenCalledTimes(2);
  });

  it("never retries a partially applied accept", async () => {
    const partial = new ApiError("partially_applied", 409, "partially_applied");
    const apply = vi.fn(async () => Promise.reject(partial));
    await expect(applyWithPlan(() => "ok?", { plan: async () => plan("f"), apply, confirm: () => true })).rejects.toBe(
      partial,
    );
    expect(apply).toHaveBeenCalledTimes(1);
  });
});
