import { describe, expect, it } from "vitest";
import type { UsageAggregate } from "../api";
import {
  DIMS,
  OTHER_KEY,
  addFilter,
  assignSlots,
  autoStep,
  currencies,
  effectiveStep,
  filtersFromSearch,
  inCurrency,
  periodRange,
  previousRange,
  series,
  slices,
  alignPrevious,
  bucketIndex,
  budgetFor,
  formatMs,
  monthFraction,
  speedByModel,
  toCSV,
} from "./usageReport";

const row = (o: Partial<UsageAggregate>): UsageAggregate => ({
  input_tokens: 0,
  output_tokens: 0,
  cache_creation_tokens: 0,
  cache_read_tokens: 0,
  cost: 0,
  call_count: 1,
  unpriced_calls: 0,
  ...o,
});

const now = new Date("2026-10-28T12:00:00Z");

describe("periods", () => {
  it("anchors the calendar presets to UTC", () => {
    expect(periodRange("today", now).from.toISOString()).toBe("2026-10-28T00:00:00.000Z");
    expect(periodRange("this-month", now).from.toISOString()).toBe("2026-10-01T00:00:00.000Z");
    const last = periodRange("last-month", now);
    expect(last.from.toISOString()).toBe("2026-09-01T00:00:00.000Z");
    expect(last.to.toISOString()).toBe("2026-09-30T23:59:59.999Z");
    expect(periodRange("ytd", now).from.toISOString()).toBe("2026-01-01T00:00:00.000Z");
  });

  it("falls back to 30 days for a custom range that is not one", () => {
    const r = periodRange("custom", now, { from: "2026-10-20T00:00:00Z", to: "2026-10-01T00:00:00Z" });
    expect(r.to).toEqual(now);
    expect(now.getTime() - r.from.getTime()).toBe(30 * 24 * 3600 * 1000);
  });

  it("compares with the window of the same length just before", () => {
    const r = periodRange("7d", now);
    const p = previousRange(r);
    expect(p.to.getTime()).toBe(r.from.getTime() - 1);
    expect(p.to.getTime() - p.from.getTime()).toBe(r.to.getTime() - r.from.getTime());
  });

  it("picks a step from the range and never asks for hours the archive cannot give", () => {
    expect(autoStep(periodRange("today", now))).toBe("hour");
    expect(autoStep(periodRange("30d", now))).toBe("day");
    expect(autoStep(periodRange("90d", now))).toBe("week");
    expect(autoStep({ from: new Date("2024-01-01T00:00:00Z"), to: now })).toBe("month");
    const r = periodRange("30d", now);
    expect(effectiveStep("hour", r, "2026-10-01T00:00:00Z")).toBe("day");
    expect(effectiveStep("hour", r, undefined)).toBe("hour");
  });
});

describe("slices", () => {
  it("keeps the five largest and folds the rest into other", () => {
    const rows = ["a", "b", "c", "d", "e", "f", "g"].map((m, i) => row({ model: m, cost: 10 - i }));
    const s = slices(rows, DIMS.model, "cost");
    expect(s.map((x) => x.key)).toEqual(["a", "b", "c", "d", "e", OTHER_KEY]);
    expect(s[5]?.value).toBe(9); // f (5) + g (4)
    expect(s[5]?.other).toBe(true);
  });

  it("sums a value's rows across currencies and drops what shows nothing", () => {
    const s = slices(
      [row({ model: "a", cost: 1, currency: "USD" }), row({ model: "a", cost: 2, currency: "EUR" }), row({ model: "z", cost: 0 })],
      DIMS.model,
      "cost",
    );
    expect(s).toHaveLength(1);
    expect(s[0]?.value).toBe(3);
    expect(s[0]?.share).toBe(1);
  });

  it("labels a user with their display name and tenant", () => {
    const names = new Map([["u1", "Ada"]]);
    const s = slices([row({ tenant_id: "acme", user_id: "u1", call_count: 3 })], DIMS.user, "calls", names);
    expect(s[0]?.label).toBe("Ada · acme");
    expect(s[0]?.key).toBe("acme/u1");
  });
});

describe("series", () => {
  it("puts every series at every period and other last", () => {
    const d1 = "2026-10-01T00:00:00Z";
    const d2 = "2026-10-02T00:00:00Z";
    const s = series(
      [
        row({ period: d1, model: "a", cost: 1 }),
        row({ period: d2, model: "b", cost: 5 }),
        row({ period: d2, other: true, cost: 2 }),
      ],
      DIMS.model,
      "cost",
    );
    expect(s.keys).toEqual(["b", "a", OTHER_KEY]);
    expect(s.points).toEqual([
      { period: d1, a: 1, b: 0, [OTHER_KEY]: 0 },
      { period: d2, a: 0, b: 5, [OTHER_KEY]: 2 },
    ]);
    expect(s.labels.get(OTHER_KEY)).toBe("other");
  });

  it("places the previous window beside the period at the same distance from its start", () => {
    const cur = { from: new Date("2026-10-01T06:00:00Z"), to: new Date("2026-10-04T06:00:00Z") };
    const prev = previousRange(cur); // starts 2026-09-28T06:00Z
    // The previous window has data only on its second and third days.
    const prevRows = [
      row({ period: "2026-09-29T00:00:00Z", call_count: 2 }),
      row({ period: "2026-09-30T00:00:00Z", call_count: 3 }),
      row({ period: "2026-09-30T00:00:00Z", call_count: 1 }),
    ];
    const curPeriods = ["2026-10-01T00:00:00Z", "2026-10-02T00:00:00Z", "2026-10-03T00:00:00Z"];
    expect(alignPrevious(prevRows, "calls", prev, curPeriods, cur, "day")).toEqual([undefined, 2, 4]);
  });

  it("counts weeks from the Monday and months from the first", () => {
    expect(bucketIndex(new Date("2026-10-12T00:00:00Z"), new Date("2026-10-01T12:00:00Z"), "week")).toBe(2); // origin week starts Mon 28 Sep
    expect(bucketIndex(new Date("2027-01-01T00:00:00Z"), new Date("2026-10-15T00:00:00Z"), "month")).toBe(3);
    expect(bucketIndex(new Date("2026-10-01T09:00:00Z"), new Date("2026-10-01T06:30:00Z"), "hour")).toBe(3);
  });
});

describe("assignSlots", () => {
  it("keeps a value's colour while it stays on screen", () => {
    const first = assignSlots(new Map(), ["a", "b", "c"]);
    expect([...first.entries()]).toEqual([["a", 0], ["b", 1], ["c", 2]]);
    // b is filtered away and d arrives: a and c keep theirs, d takes the free slot.
    const second = assignSlots(first, ["c", "a", "d"]);
    expect(second.get("a")).toBe(0);
    expect(second.get("c")).toBe(2);
    expect(second.get("d")).toBe(1);
    expect(second.has(OTHER_KEY)).toBe(false);
  });

  it("never gives two visible values the same slot", () => {
    const s = assignSlots(new Map([["x", 0], ["y", 0]]), ["x", "y"]);
    expect(s.get("x")).not.toBe(s.get("y"));
  });
});

describe("currencies", () => {
  it("lists the most spent first and keeps unpriced rows in any currency view", () => {
    const rows = [row({ cost: 1, currency: "EUR" }), row({ cost: 5, currency: "USD" }), row({ cost: 0 })];
    expect(currencies(rows)).toEqual(["USD", "EUR"]);
    expect(inCurrency(rows, "USD")).toHaveLength(2);
  });
});

describe("csv", () => {
  it("quotes what needs quoting and defuses formulas", () => {
    expect(toCSV(["a", "b"], [["x,y", '=SUM(A1)'], ["say \"hi\"", 3]])).toBe('a,b\n"x,y",\'=SUM(A1)\n"say ""hi""",3\n');
  });

  it("defuses every formula start a spreadsheet honours, and leaves numbers alone", () => {
    const cells = ["  =1+1", "\t=1", "\r=1", "＝1+1", "＋1", "－1", "＠SUM(A1)", "+1", "-1", "@x"];
    for (const c of cells) {
      expect(toCSV(["a"], [[c]]).split("\n")[1]?.replace(/^"/, "").startsWith("'")).toBe(true);
    }
    expect(toCSV(["n"], [[-5]])).toBe("n\n-5\n");
    expect(toCSV(["a"], [["claude-haiku-5-5"]])).toBe("a\nclaude-haiku-5-5\n");
  });
});

describe("filters", () => {
  it("reads every filter dimension but tenant from the URL", () => {
    const f = filtersFromSearch(new URLSearchParams("model=a,b&tenant=acme&class=background"));
    expect(f).toEqual({ model: ["a", "b"], class: ["background"] });
  });

  it("adds to a dimension without duplicates and skips empty values", () => {
    expect(addFilter({ model: ["a"] }, { model: ["a", "b"], tier: [""] })).toEqual({ model: ["a", "b"] });
  });
});

describe("budgets", () => {
  const limits = [
    { tenant_id: "acme", scope: "tenant", scope_id: "acme", soft_limit: 800, hard_limit: 1000, used: 300 },
    { tenant_id: "acme", scope: "user", scope_id: "ann", soft_limit: null, hard_limit: 100, used: 40 },
    { tenant_id: "globex", scope: "user", scope_id: "ann", soft_limit: null, hard_limit: 5, used: 5 },
  ];
  const mid = new Date("2026-10-16T12:00:00Z"); // half of October

  it("finds a tenant's and a user's budget, the user within its tenant", () => {
    expect(budgetFor(limits, "tenant", "acme", "", mid)?.hard).toBe(1000);
    expect(budgetFor(limits, "user", "acme", "ann", mid)?.hard).toBe(100);
    expect(budgetFor(limits, "user", "globex", "ann", mid)?.used).toBe(5);
    expect(budgetFor(limits, "user", "acme", "bob", mid)).toBeUndefined();
  });

  it("projects at this month's rate, but not from its first hours", () => {
    expect(monthFraction(mid)).toBeCloseTo(0.5, 2);
    expect(budgetFor(limits, "tenant", "acme", "", mid)?.projected).toBeCloseTo(600, 0);
    expect(budgetFor(limits, "tenant", "acme", "", new Date("2026-10-01T03:00:00Z"))?.projected).toBe(300);
  });
});

describe("speed", () => {
  it("keeps the provider that answered more of a model's calls", () => {
    const m = speedByModel([
      { provider: "a", model: "x", calls: 3, duration_p50_ms: 1, duration_p95_ms: 2 },
      { provider: "b", model: "x", calls: 9, duration_p50_ms: 5, duration_p95_ms: 6 },
    ]);
    expect(m.get("x")?.provider).toBe("b");
  });

  it("writes durations to scale", () => {
    expect(formatMs(0)).toBe("—");
    expect(formatMs(340)).toBe("340 ms");
    expect(formatMs(2500)).toBe("2.5 s");
    expect(formatMs(42000)).toBe("42 s");
  });
});
