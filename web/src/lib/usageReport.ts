import type { UsageAggregate } from "../api";

// The Usage dashboard's logic, kept free of React and of the chart library so
// it is tested on its own: periods, steps, measures, the per-tab dimension, the
// share and series shapes the charts draw, stable colours and CSV.
//
// Every period is UTC: the server buckets in UTC and the archive holds UTC days
// (RFC FM D5), so a local-time window would split a day the data cannot split.

export type UsageTab = "overview" | "models" | "providers" | "tiers" | "users" | "tenants" | "background";
export type Measure = "cost" | "tokens" | "calls" | "audio" | "chars";
export type Step = "hour" | "day" | "week" | "month";
export type PeriodPreset = "today" | "7d" | "30d" | "this-month" | "last-month" | "90d" | "ytd" | "custom";

export const TABS: { id: UsageTab; label: string; adminOnly?: boolean }[] = [
  { id: "overview", label: "overview" },
  { id: "models", label: "models" },
  { id: "providers", label: "providers" },
  { id: "tiers", label: "tiers" },
  { id: "users", label: "users" },
  { id: "tenants", label: "tenants", adminOnly: true },
  { id: "background", label: "background" },
];

export const PRESETS: { id: PeriodPreset; label: string }[] = [
  { id: "today", label: "today" },
  { id: "7d", label: "last 7 days" },
  { id: "30d", label: "last 30 days" },
  { id: "this-month", label: "this month" },
  { id: "last-month", label: "last month" },
  { id: "90d", label: "last 90 days" },
  { id: "ytd", label: "year to date" },
  { id: "custom", label: "custom" },
];

export const MEASURES: { id: Measure; label: string }[] = [
  { id: "cost", label: "cost" },
  { id: "tokens", label: "tokens" },
  { id: "calls", label: "calls" },
  { id: "audio", label: "audio minutes" },
  { id: "chars", label: "characters" },
];

// The dimensions a filter chip may hold — every report dimension but tenant,
// which is the page's focus instead (the server keeps it apart the same way).
export const FILTER_DIMS = ["user", "provider", "model", "source", "purpose", "tier", "user_tier", "agent", "class"] as const;
export type FilterDim = (typeof FILTER_DIMS)[number];
export type Filters = Partial<Record<FilterDim, string[]>>;

// The donut and the series keep this many values and fold the rest into
// "other": a part-to-whole chart reads at six segments or fewer.
export const TOP_N = 5;
export const OTHER_KEY = "__other";

export interface Range {
  from: Date;
  to: Date;
}

const DAY = 24 * 60 * 60 * 1000;

function utcMidnight(d: Date): Date {
  return new Date(Date.UTC(d.getUTCFullYear(), d.getUTCMonth(), d.getUTCDate()));
}

// periodRange turns a preset into its UTC window ending now; custom takes the
// caller's bounds.
export function periodRange(preset: PeriodPreset, now: Date, custom?: { from?: string; to?: string }): Range {
  switch (preset) {
    case "today":
      return { from: utcMidnight(now), to: now };
    case "7d":
      return { from: new Date(now.getTime() - 7 * DAY), to: now };
    case "90d":
      return { from: new Date(now.getTime() - 90 * DAY), to: now };
    case "this-month":
      return { from: new Date(Date.UTC(now.getUTCFullYear(), now.getUTCMonth(), 1)), to: now };
    case "last-month": {
      const start = new Date(Date.UTC(now.getUTCFullYear(), now.getUTCMonth() - 1, 1));
      const end = new Date(Date.UTC(now.getUTCFullYear(), now.getUTCMonth(), 1) - 1);
      return { from: start, to: end };
    }
    case "ytd":
      return { from: new Date(Date.UTC(now.getUTCFullYear(), 0, 1)), to: now };
    case "custom": {
      const from = custom?.from ? new Date(custom.from) : new Date(now.getTime() - 30 * DAY);
      const to = custom?.to ? new Date(custom.to) : now;
      if (!isNaN(from.getTime()) && !isNaN(to.getTime()) && from < to) return { from, to };
      return { from: new Date(now.getTime() - 30 * DAY), to: now };
    }
    case "30d":
    default:
      return { from: new Date(now.getTime() - 30 * DAY), to: now };
  }
}

// previousRange is the window of the same length immediately before r, what
// "compare with the previous period" compares against.
export function previousRange(r: Range): Range {
  const len = r.to.getTime() - r.from.getTime();
  return { from: new Date(r.from.getTime() - len - 1), to: new Date(r.from.getTime() - 1) };
}

// autoStep picks a step that gives a readable number of points for the range.
export function autoStep(r: Range): Step {
  const days = (r.to.getTime() - r.from.getTime()) / DAY;
  if (days <= 2) return "hour";
  if (days <= 60) return "day";
  if (days <= 366) return "week";
  return "month";
}

// hourAllowed says whether hour steps reach the whole range: before
// detailSince only whole days survive, and the server refuses the request.
export function hourAllowed(r: Range, detailSince?: string): boolean {
  if (!detailSince) return true;
  const since = new Date(detailSince);
  return isNaN(since.getTime()) || r.from >= since;
}

// effectiveStep is the step the page asks for: the chosen one, or the
// automatic one, never hour where hour is refused.
export function effectiveStep(chosen: Step | "auto", r: Range, detailSince?: string): Step {
  const step = chosen === "auto" ? autoStep(r) : chosen;
  return step === "hour" && !hourAllowed(r, detailSince) ? "day" : step;
}

// The top_by the series query ranks by, closest to the measure on screen.
export function topByFor(m: Measure): "cost" | "tokens" | "calls" {
  if (m === "tokens" || m === "chars") return "tokens";
  if (m === "calls" || m === "audio") return "calls";
  return "cost";
}

export function totalTokens(r: UsageAggregate): number {
  return r.input_tokens + r.output_tokens + r.cache_creation_tokens + r.cache_read_tokens;
}

export function measureOf(r: UsageAggregate, m: Measure): number {
  switch (m) {
    case "cost":
      return r.cost;
    case "tokens":
      return totalTokens(r);
    case "calls":
      return r.call_count;
    case "audio":
      return (r.audio_seconds ?? 0) / 60;
    case "chars":
      return r.input_chars ?? 0;
  }
}

export function compact(n: number): string {
  const a = Math.abs(n);
  if (a >= 1e9) return (n / 1e9).toFixed(a >= 1e10 ? 0 : 1) + "B";
  if (a >= 1e6) return (n / 1e6).toFixed(a >= 1e7 ? 0 : 1) + "M";
  if (a >= 1e3) return (n / 1e3).toFixed(a >= 1e4 ? 0 : 1) + "K";
  return Number.isInteger(n) ? String(n) : n.toFixed(1);
}

export function formatMeasure(v: number, m: Measure, currency?: string): string {
  switch (m) {
    case "cost": {
      const digits = v !== 0 && Math.abs(v) < 1 ? 4 : 2;
      return `${v.toFixed(digits)} ${currency || "USD"}`;
    }
    case "audio":
      return `${v.toFixed(1)} min`;
    default:
      return compact(v);
  }
}

// --- dimensions ---------------------------------------------------------

// DimSpec is how one tab reads its rows: what it groups by, the key and label
// of a row's value, and the filter a click on it adds. names maps a user
// subject to a display name where the identity store knows one (D9).
export interface DimSpec {
  groupBy: string[];
  key(r: UsageAggregate): string;
  label(r: UsageAggregate, names?: Map<string, string>): string;
  // The filter a click adds; tenant is returned apart, as it sets the focus.
  click(r: UsageAggregate): { filters?: Filters; tenant?: string };
}

const plain = (dim: FilterDim, field: (r: UsageAggregate) => string | undefined, empty: string): DimSpec => ({
  groupBy: [dim],
  key: (r) => field(r) ?? "",
  label: (r) => field(r) || empty,
  click: (r) => ({ filters: { [dim]: [field(r) ?? ""] } }),
});

export const DIMS = {
  model: plain("model", (r) => r.model, "(unknown)"),
  provider: plain("provider", (r) => r.provider, "(unknown)"),
  tier: plain("tier", (r) => r.tier, "unknown"),
  user_tier: plain("user_tier", (r) => r.user_tier, "none"),
  purpose: plain("purpose", (r) => r.purpose, "chat"),
  class: plain("class", (r) => r.class, "unknown"),
  source: plain("source", (r) => r.credential_source, "operator"),
  tenant: {
    groupBy: ["tenant"],
    key: (r) => r.tenant_id ?? "",
    label: (r) => r.tenant_id || "(default)",
    click: (r) => ({ tenant: r.tenant_id ?? "" }),
  } as DimSpec,
  // A user is shown with its tenant: the same subject may exist in two. user
  // comes first because the server's top folds by the first dimension.
  user: {
    groupBy: ["user", "tenant"],
    key: (r) => `${r.tenant_id ?? ""}/${r.user_id ?? ""}`,
    label: (r, names) => {
      const who = r.user_id ? (names?.get(r.user_id) ?? r.user_id) : "(no user)";
      return r.tenant_id ? `${who} · ${r.tenant_id}` : who;
    },
    click: (r) => ({ tenant: r.tenant_id ?? "", filters: { user: [r.user_id ?? ""] } }),
  } as DimSpec,
} satisfies Record<string, DimSpec>;

// --- shapes for the charts ---------------------------------------------

export interface Slice {
  key: string;
  label: string;
  value: number;
  share: number; // 0..1
  other: boolean;
  row?: UsageAggregate; // the summed row, for a click
}

// sumBy folds rows by the spec's key (rows of several currencies share a key).
export function sumBy(rows: UsageAggregate[], spec: DimSpec): Map<string, UsageAggregate> {
  const out = new Map<string, UsageAggregate>();
  for (const r of rows) {
    const k = spec.key(r);
    const cur = out.get(k);
    if (!cur) {
      out.set(k, { ...r });
      continue;
    }
    cur.input_tokens += r.input_tokens;
    cur.output_tokens += r.output_tokens;
    cur.cache_creation_tokens += r.cache_creation_tokens;
    cur.cache_read_tokens += r.cache_read_tokens;
    cur.cost += r.cost;
    cur.call_count += r.call_count;
    cur.unpriced_calls += r.unpriced_calls;
    cur.audio_seconds = (cur.audio_seconds ?? 0) + (r.audio_seconds ?? 0);
    cur.input_chars = (cur.input_chars ?? 0) + (r.input_chars ?? 0);
  }
  return out;
}

// slices is the donut: the TOP_N largest values of the measure and one
// "other" holding the rest. Values with nothing to show are dropped.
export function slices(rows: UsageAggregate[], spec: DimSpec, m: Measure, names?: Map<string, string>): Slice[] {
  const sums = [...sumBy(rows, spec).values()]
    .map((r) => ({ r, v: measureOf(r, m) }))
    .filter((x) => x.v > 0)
    .sort((a, b) => b.v - a.v || spec.key(a.r).localeCompare(spec.key(b.r)));
  const total = sums.reduce((s, x) => s + x.v, 0);
  if (total === 0) return [];
  const head = sums.slice(0, TOP_N).map(
    (x): Slice => ({ key: spec.key(x.r), label: spec.label(x.r, names), value: x.v, share: x.v / total, other: false, row: x.r }),
  );
  const rest = sums.slice(TOP_N).reduce((s, x) => s + x.v, 0);
  if (rest > 0) head.push({ key: OTHER_KEY, label: "other", value: rest, share: rest / total, other: true });
  return head;
}

export interface SeriesData {
  keys: string[]; // series in order of their total, "other" last
  labels: Map<string, string>;
  points: Array<Record<string, number | string>>; // { period, [key]: value }
}

// series reshapes a bucketed, top-folded report into one point per period with
// a value per series, every series present at every point (0 when absent), so
// lines do not jump and stacks do not gap.
export function series(rows: UsageAggregate[], spec: DimSpec, m: Measure, names?: Map<string, string>): SeriesData {
  const totals = new Map<string, number>();
  const labels = new Map<string, string>();
  const byPeriod = new Map<string, Record<string, number | string>>();
  for (const r of rows) {
    if (!r.period) continue;
    const k = r.other ? OTHER_KEY : spec.key(r);
    labels.set(k, r.other ? "other" : spec.label(r, names));
    const v = measureOf(r, m);
    totals.set(k, (totals.get(k) ?? 0) + v);
    const p = byPeriod.get(r.period) ?? { period: r.period };
    p[k] = ((p[k] as number | undefined) ?? 0) + v;
    byPeriod.set(r.period, p);
  }
  const keys = [...totals.keys()]
    .filter((k) => k !== OTHER_KEY)
    .sort((a, b) => (totals.get(b) ?? 0) - (totals.get(a) ?? 0) || a.localeCompare(b));
  if (totals.has(OTHER_KEY)) keys.push(OTHER_KEY);
  const points = [...byPeriod.values()].sort((a, b) => String(a.period).localeCompare(String(b.period)));
  for (const p of points) for (const k of keys) p[k] ??= 0;
  return { keys, labels, points };
}

// bucketIndex is how many steps a period starts after the bucket holding
// origin, in UTC — the position a period has within its window.
export function bucketIndex(period: Date, origin: Date, step: Step): number {
  switch (step) {
    case "month":
      return (
        period.getUTCFullYear() * 12 + period.getUTCMonth() - (origin.getUTCFullYear() * 12 + origin.getUTCMonth())
      );
    case "week": {
      const o = utcMidnight(origin);
      const monday = new Date(o.getTime() - ((o.getUTCDay() + 6) % 7) * DAY);
      return Math.floor((period.getTime() - monday.getTime()) / (7 * DAY));
    }
    case "day":
      return Math.floor((period.getTime() - utcMidnight(origin).getTime()) / DAY);
    case "hour": {
      const h = 3600 * 1000;
      return Math.floor((period.getTime() - Math.floor(origin.getTime() / h) * h) / h);
    }
  }
}

// alignPrevious is the previous window's total per period, placed beside the
// current period at the same distance from its own window's start — the dashed
// line. A current period the previous window has nothing for gets undefined (a
// gap), never a zero it did not measure.
export function alignPrevious(
  prevRows: UsageAggregate[],
  m: Measure,
  prev: Range,
  curPeriods: string[],
  cur: Range,
  step: Step,
): Array<number | undefined> {
  const byIndex = new Map<number, number>();
  for (const r of prevRows) {
    if (!r.period) continue;
    const i = bucketIndex(new Date(r.period), prev.from, step);
    byIndex.set(i, (byIndex.get(i) ?? 0) + measureOf(r, m));
  }
  return curPeriods.map((p) => byIndex.get(bucketIndex(new Date(p), cur.from, step)));
}

// --- colour ---------------------------------------------------------------

export const SERIES_SLOTS = 7;

// assignSlots gives each visible value a palette slot. A value keeps the slot
// it had while it stays on screen, so a filter or a new period never repaints
// the survivors; a new value takes the lowest slot no visible value holds.
// "other" never takes a slot — it is the neutral colour.
export function assignSlots(prev: Map<string, number>, visible: string[]): Map<string, number> {
  const next = new Map<string, number>();
  const used = new Set<number>();
  const wanted = visible.filter((k) => k !== OTHER_KEY);
  for (const k of wanted) {
    const s = prev.get(k);
    if (s !== undefined && !used.has(s)) {
      next.set(k, s);
      used.add(s);
    }
  }
  for (const k of wanted) {
    if (next.has(k)) continue;
    let s = 0;
    while (used.has(s) && s < SERIES_SLOTS) s++;
    next.set(k, s % SERIES_SLOTS);
    used.add(s);
  }
  // Keep the slots of values not on screen now, so they return in their colour.
  for (const [k, s] of prev) if (!next.has(k) && ![...next.values()].includes(s)) next.set(k, s);
  return next;
}


// --- currency (D10) -------------------------------------------------------

// currencies lists the priced currencies in rows, the most spent first.
export function currencies(rows: UsageAggregate[]): string[] {
  const by = new Map<string, number>();
  for (const r of rows) if (r.currency) by.set(r.currency, (by.get(r.currency) ?? 0) + r.cost);
  return [...by.entries()].sort((a, b) => b[1] - a[1]).map(([c]) => c);
}

// inCurrency keeps the rows a cost chart may sum: one currency, and the
// unpriced rows (currency "") whose cost is 0 and whose calls still count.
export function inCurrency(rows: UsageAggregate[], currency: string): UsageAggregate[] {
  return rows.filter((r) => !r.currency || r.currency === currency);
}

// --- CSV ------------------------------------------------------------------

// A text cell a spreadsheet would read as a formula: one starting — after any
// whitespace — with = + - @, a tab or a carriage return, or the full-width
// forms of the first four, which some spreadsheets fold to ASCII. Names here
// (users, agents, models) come from callers, so a cell is never trusted.
const FORMULA_START = /^[\s]*[=+\-@\t\r\uFF1D\uFF0B\uFF0D\uFF20]/;

function csvCell(v: string | number): string {
  // Numbers are written as numbers: a negative one is a value, not a formula.
  const s = typeof v === "number" ? String(v) : FORMULA_START.test(v) ? "'" + v : v;
  return /[",\n\r]/.test(s) ? `"${s.replace(/"/g, '""')}"` : s;
}

export function toCSV(header: string[], rows: Array<Array<string | number>>): string {
  return [header, ...rows].map((r) => r.map(csvCell).join(",")).join("\n") + "\n";
}

// --- URL state -----------------------------------------------------------

export function filtersFromSearch(sp: URLSearchParams): Filters {
  const out: Filters = {};
  for (const d of FILTER_DIMS) {
    const v = sp.get(d);
    if (v) out[d] = v.split(",").filter((x) => x !== "");
  }
  return out;
}

// addFilter adds values to a dimension's filter (a click narrows; it never
// replaces another dimension's filter).
export function addFilter(f: Filters, add: Filters): Filters {
  const out: Filters = { ...f };
  for (const d of FILTER_DIMS) {
    // An empty value ("unknown" tier, "(no user)") is not a filter the
    // server can apply; it is skipped rather than shown as a chip that does nothing.
    const vals = add[d]?.filter((v) => v !== "");
    if (!vals || vals.length === 0) continue;
    out[d] = [...new Set([...(out[d] ?? []), ...vals])];
  }
  return out;
}
