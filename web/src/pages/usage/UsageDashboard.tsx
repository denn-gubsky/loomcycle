import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Link, useParams, useSearchParams } from "react-router-dom";
import { getUsage, listUsers, type UsageAggregate, type UsageReportParams } from "../../api";
import { useFocusTenant, usePrincipal } from "../../components/Layout";
import {
  DIMS,
  FILTER_DIMS,
  MEASURES,
  OTHER_KEY,
  PRESETS,
  TABS,
  addFilter,
  assignSlots,
  currencies,
  effectiveStep,
  filtersFromSearch,
  formatMeasure,
  hourAllowed,
  inCurrency,
  measureOf,
  periodRange,
  previousRange,
  series,
  slices,
  sumBy,
  toCSV,
  topByFor,
  alignPrevious,
  type DimSpec,
  type Filters,
  type Measure,
  type PeriodPreset,
  type Range,
  type Slice,
  type Step,
  type UsageTab,
} from "../../lib/usageReport";
import { Donut, TimeChart, colorOf, usePalette } from "./charts";

// UsageDashboard — the Usage pages (RFC FM): one tab per report, each the same
// template (figures, a donut of shares, use over time, a ranked table), under
// one filter bar whose state lives in the URL so a report can be linked.
// Clicking a slice, a legend entry or a table row narrows every tab.
//
// Loaded on its own (React.lazy in main.tsx): the chart library is in this
// chunk only.

type Rows = UsageAggregate[];

interface Loaded {
  main: Rows; // the tab's dimension over the window — donut and table
  series: Rows; // the same, bucketed and top-folded — the chart
  classTotals: Rows; // every class, for totals and the background share
  sources: Rows; // operator- vs tenant-paid
  prev?: Rows; // the previous window, bucketed — compare
  overview?: { model: Rows; provider: Rows; tier: Rows; class: Rows; top: Rows };
}

// The dimension a tab reports on, and any filter it always applies.
function tabSpec(tab: UsageTab, tierDim: "tier" | "user_tier"): { spec: DimSpec; noun: string; forced?: Filters } {
  switch (tab) {
    case "models":
      return { spec: DIMS.model, noun: "model" };
    case "providers":
      return { spec: DIMS.provider, noun: "provider" };
    case "tiers":
      return { spec: DIMS[tierDim], noun: tierDim === "tier" ? "model tier" : "user tier" };
    case "users":
      return { spec: DIMS.user, noun: "user" };
    case "tenants":
      return { spec: DIMS.tenant, noun: "tenant" };
    case "background":
      return { spec: DIMS.purpose, noun: "purpose", forced: { class: ["background"] } };
    case "overview":
    default:
      return { spec: DIMS.provider, noun: "provider" };
  }
}

const iso = (d: Date) => d.toISOString().replace(/\.\d{3}Z$/, "Z");

function sum(rows: Rows, m: Measure): number {
  return rows.reduce((s, r) => s + measureOf(r, m), 0);
}

export default function UsageDashboard() {
  const params = useParams();
  const principal = usePrincipal();
  const isAdmin = !principal || principal.is_admin;
  const globalFocus = useFocusTenant();
  const [sp, setSp] = useSearchParams();
  const palette = usePalette();

  const requested = (params.tab ?? "overview") as UsageTab;
  const tab: UsageTab = TABS.some((t) => t.id === requested && (!t.adminOnly || isAdmin)) ? requested : "overview";

  // --- state from the URL ---
  const preset = (sp.get("period") ?? "30d") as PeriodPreset;
  const customFrom = sp.get("from") ?? "";
  const customTo = sp.get("to") ?? "";
  const stepChoice = (sp.get("step") ?? "auto") as Step | "auto";
  const measure = (sp.get("measure") ?? "cost") as Measure;
  const mode = sp.get("chart") === "stacked" || (tab === "background" && !sp.get("chart")) ? "stacked" : "lines";
  const compare = sp.get("compare") === "1";
  const tenant = isAdmin ? (sp.get("tenant") ?? globalFocus) : "";
  const tierDim = sp.get("tierdim") === "user_tier" ? "user_tier" : "tier";
  const filters = useMemo(() => filtersFromSearch(sp), [sp]);
  const currencyChoice = sp.get("currency") ?? "";

  const setParam = useCallback(
    (changes: Record<string, string | null>) => {
      setSp(
        (cur) => {
          const next = new URLSearchParams(cur);
          for (const [k, v] of Object.entries(changes)) {
            if (v === null || v === "") next.delete(k);
            else next.set(k, v);
          }
          return next;
        },
        { replace: true },
      );
    },
    [setSp],
  );

  // A reload re-reads "now"; the range is otherwise stable across renders.
  const [tick, setTick] = useState(0);
  const range: Range = useMemo(
    () => periodRange(preset, new Date(), { from: customFrom, to: customTo }),
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [preset, customFrom, customTo, tick],
  );
  const [detailSince, setDetailSince] = useState<string | undefined>();
  const step = effectiveStep(stepChoice, range, detailSince);

  const { spec, noun, forced } = tabSpec(tab, tierDim);

  // --- loading ---
  const [data, setData] = useState<Loaded | null>(null);
  const [loading, setLoading] = useState(false);
  const [err, setErr] = useState("");
  const [names, setNames] = useState<Map<string, string>>(new Map());
  const filterKey = JSON.stringify(filters);

  useEffect(() => {
    let cancelled = false;
    const base = (r: Range, extra?: Filters): UsageReportParams => ({
      from: iso(r.from),
      to: iso(r.to),
      tenant: tenant || undefined,
      filters: { ...addFilter(filters, extra ?? {}) },
    });
    const g = (p: UsageReportParams) => getUsage(p);
    const topBy = topByFor(measure);
    const main = base(range, forced);
    const load = async (): Promise<Loaded> => {
      const [mainRes, seriesRes, classRes, sourceRes, prevRes] = await Promise.all([
        g({ ...main, group_by: spec.groupBy.join(",") }),
        g({ ...main, group_by: spec.groupBy.join(","), bucket: step, top: 5, top_by: topBy }),
        g({ ...base(range), group_by: "class" }),
        g({ ...main, group_by: "source" }),
        compare ? g({ ...base(previousRange(range), forced), group_by: "class", bucket: step }) : Promise.resolve(null),
      ]);
      const since = mainRes.detail_since;
      if (!cancelled) setDetailSince(since);
      const out: Loaded = {
        main: mainRes.rows ?? [],
        series: seriesRes.rows ?? [],
        classTotals: classRes.rows ?? [],
        sources: sourceRes.rows ?? [],
        prev: prevRes?.rows ?? undefined,
      };
      if (tab === "overview") {
        const [m, p, t, c, top] = await Promise.all([
          g({ ...main, group_by: "model" }),
          g({ ...main, group_by: "provider" }),
          g({ ...main, group_by: "tier" }),
          g({ ...main, group_by: "class" }),
          g({ ...main, group_by: isAdmin ? "tenant" : "user,tenant" }),
        ]);
        out.overview = { model: m.rows ?? [], provider: p.rows ?? [], tier: t.rows ?? [], class: c.rows ?? [], top: top.rows ?? [] };
      }
      return out;
    };
    setLoading(true);
    setErr("");
    load()
      .then((d) => {
        if (!cancelled) setData(d);
      })
      .catch((e) => {
        if (!cancelled) setErr(e instanceof Error ? e.message : String(e));
      })
      .finally(() => {
        if (!cancelled) setLoading(false);
      });
    return () => {
      cancelled = true;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [tab, range, step, measure, compare, tenant, tierDim, filterKey]);

  // Display names for users, where the identity store knows them (D9). Best
  // effort: a viewer who may not list users sees subject ids.
  useEffect(() => {
    if (tab !== "users" && !(tab === "overview" && !isAdmin)) return;
    let cancelled = false;
    listUsers(tenant || undefined)
      .then((r) => {
        if (cancelled) return;
        const m = new Map<string, string>();
        for (const u of r.users ?? []) if (u.display_name) m.set(u.user_id, u.display_name);
        setNames(m);
      })
      .catch(() => undefined);
    return () => {
      cancelled = true;
    };
  }, [tab, tenant, isAdmin]);

  // --- currency (D10): a cost chart shows one currency at a time ---
  const allCurrencies = useMemo(() => currencies(data?.main ?? []), [data]);
  const currency = allCurrencies.includes(currencyChoice) ? currencyChoice : (allCurrencies[0] ?? "");
  const cur = useCallback(
    (rows: Rows | undefined): Rows => (measure === "cost" && currency ? inCurrency(rows ?? [], currency) : (rows ?? [])),
    [measure, currency],
  );

  // --- stable colours: a value keeps its slot as filters and periods change ---
  const slotsRef = useRef<Record<string, Map<string, number>>>({});
  const slotsFor = (dimName: string, keys: string[]) => {
    const next = assignSlots(slotsRef.current[dimName] ?? new Map(), keys);
    slotsRef.current[dimName] = next;
    return next;
  };

  // --- clicking narrows every tab ---
  const tableRef = useRef<HTMLDivElement>(null);
  const pickRow = (s: DimSpec, r: UsageAggregate | undefined, other = false) => {
    if (other || !r) {
      tableRef.current?.scrollIntoView({ behavior: "smooth", block: "start" });
      return;
    }
    const c = s.click(r);
    const changes: Record<string, string | null> = {};
    if (c.tenant !== undefined && isAdmin && c.tenant !== "") changes.tenant = c.tenant;
    if (c.filters) {
      const next = addFilter(filters, c.filters);
      for (const d of FILTER_DIMS) changes[d] = next[d]?.join(",") ?? null;
    }
    setParam(changes);
  };
  const pickSlice = (s: DimSpec) => (sl: Slice) => pickRow(s, sl.row, sl.other);

  // --- figures ---
  const totals = cur(data?.classTotals);
  const scoped = cur(tab === "overview" ? data?.overview?.class : data?.main);
  const total = sum(scoped, measure);
  const prevTotal = data?.prev ? sum(cur(data.prev), measure) : undefined;
  const delta = prevTotal !== undefined && prevTotal > 0 ? (total - prevTotal) / prevTotal : undefined;
  const calls = scoped.reduce((s, r) => s + r.call_count, 0);
  const unpriced = scoped.reduce((s, r) => s + r.unpriced_calls, 0);
  const sources = cur(data?.sources);
  const operatorPaid = sources.filter((r) => (r.credential_source || "operator") === "operator").reduce((s, r) => s + r.cost, 0);
  const tenantPaid = sources.filter((r) => r.credential_source === "tenant" || r.credential_source === "user").reduce((s, r) => s + r.cost, 0);
  const bgTotal = totals.filter((r) => r.class === "background").reduce((s, r) => s + measureOf(r, measure), 0);
  const allTotal = sum(totals, measure);

  // --- the tab's charts ---
  const tabSlices = slices(cur(data?.main), spec, measure, names);
  const tabSeries = series(cur(data?.series), spec, measure, names);
  const tabSlots = slotsFor(`${tab}:${spec.groupBy.join(",")}`, [...tabSeries.keys, ...tabSlices.map((s) => s.key)]);
  const previous =
    compare && data?.prev
      ? alignPrevious(cur(data.prev), measure, previousRange(range), tabSeries.points.map((p) => String(p.period)), range, step)
      : undefined;

  // --- the table ---
  const tableRows = [...sumBy(cur(data?.main), spec).values()].sort((a, b) => measureOf(b, measure) - measureOf(a, measure));
  const tableTotal = tableRows.reduce((s, r) => s + measureOf(r, measure), 0);
  const hasAudio = tableRows.some((r) => (r.audio_seconds ?? 0) > 0);
  const hasChars = tableRows.some((r) => (r.input_chars ?? 0) > 0);

  const exportCSV = () => {
    const header = [noun, "input", "output", "cache", ...(hasAudio ? ["audio_minutes"] : []), ...(hasChars ? ["characters"] : []), "calls", "unpriced_calls", "cost", "currency"];
    const body = tableRows.map((r) => [
      spec.label(r, names),
      r.input_tokens,
      r.output_tokens,
      r.cache_creation_tokens + r.cache_read_tokens,
      ...(hasAudio ? [((r.audio_seconds ?? 0) / 60).toFixed(2)] : []),
      ...(hasChars ? [r.input_chars ?? 0] : []),
      r.call_count,
      r.unpriced_calls,
      r.cost.toFixed(6),
      measure === "cost" ? currency : r.currency ?? "",
    ]);
    const blob = new Blob([toCSV(header, body)], { type: "text/csv" });
    const a = document.createElement("a");
    a.href = URL.createObjectURL(blob);
    a.download = `usage-${tab}-${iso(range.from).slice(0, 10)}-${iso(range.to).slice(0, 10)}.csv`;
    a.click();
    URL.revokeObjectURL(a.href);
  };

  const chips: { label: string; clear: () => void }[] = [];
  if (isAdmin && tenant) chips.push({ label: `tenant: ${tenant}`, clear: () => setParam({ tenant: "" }) });
  for (const d of FILTER_DIMS) {
    for (const v of filters[d] ?? []) {
      chips.push({
        label: `${d}: ${d === "user" ? (names.get(v) ?? v) : v}`,
        clear: () => {
          const rest = (filters[d] ?? []).filter((x) => x !== v);
          setParam({ [d]: rest.length ? rest.join(",") : null });
        },
      });
    }
  }

  const search = sp.toString();
  const tabLink = (id: UsageTab) => `/usage/${id}${search ? "?" + search : ""}`;
  const hourOK = hourAllowed(range, detailSince);
  const fmt = (v: number) => formatMeasure(v, measure, currency);

  return (
    <div className="usage-view usage-dashboard">
      <div className="usage-header">
        <h1>usage</h1>
        <p className="usage-sub">
          What every model call cost, by {noun}. Periods are UTC; click a slice, a legend entry or a row to narrow every
          tab.
        </p>
      </div>

      <nav className="usage-tabs" aria-label="usage reports">
        {TABS.filter((t) => !t.adminOnly || isAdmin).map((t) => (
          <Link key={t.id} to={tabLink(t.id)} className={"usage-tab" + (t.id === tab ? " is-active" : "")} aria-current={t.id === tab ? "page" : undefined}>
            {t.label}
          </Link>
        ))}
      </nav>

      <div className="usage-controls usage-filterbar">
        <label className="usage-field">
          period
          <select value={preset} onChange={(e) => setParam({ period: e.target.value })}>
            {PRESETS.map((p) => (
              <option key={p.id} value={p.id}>
                {p.label}
              </option>
            ))}
          </select>
        </label>
        {preset === "custom" && (
          <>
            <label className="usage-field">
              from (UTC)
              <input
                type="datetime-local"
                value={customFrom ? customFrom.slice(0, 16) : iso(range.from).slice(0, 16)}
                onChange={(e) => setParam({ from: e.target.value ? e.target.value + ":00Z" : null })}
              />
            </label>
            <label className="usage-field">
              to (UTC)
              <input
                type="datetime-local"
                value={customTo ? customTo.slice(0, 16) : iso(range.to).slice(0, 16)}
                onChange={(e) => setParam({ to: e.target.value ? e.target.value + ":00Z" : null })}
              />
            </label>
          </>
        )}
        <label className="usage-field">
          step
          <select value={stepChoice} onChange={(e) => setParam({ step: e.target.value === "auto" ? null : e.target.value })}>
            <option value="auto">auto ({step})</option>
            <option value="hour" disabled={!hourOK} title={hourOK ? undefined : "hours reach only the per-call detail"}>
              hour
            </option>
            <option value="day">day</option>
            <option value="week">week</option>
            <option value="month">month</option>
          </select>
        </label>
        <label className="usage-field">
          measure
          <select value={measure} onChange={(e) => setParam({ measure: e.target.value === "cost" ? null : e.target.value })}>
            {MEASURES.map((m) => (
              <option key={m.id} value={m.id}>
                {m.label}
              </option>
            ))}
          </select>
        </label>
        {measure === "cost" && allCurrencies.length > 1 && (
          <label className="usage-field">
            currency
            <select value={currency} onChange={(e) => setParam({ currency: e.target.value })}>
              {allCurrencies.map((c) => (
                <option key={c} value={c}>
                  {c}
                </option>
              ))}
            </select>
          </label>
        )}
        {tab === "tiers" && (
          <label className="usage-field">
            tier of
            <select value={tierDim} onChange={(e) => setParam({ tierdim: e.target.value === "tier" ? null : e.target.value })}>
              <option value="tier">the model</option>
              <option value="user_tier">the user</option>
            </select>
          </label>
        )}
        <label className="usage-field">
          chart
          <select value={mode} onChange={(e) => setParam({ chart: e.target.value })}>
            <option value="lines">lines</option>
            <option value="stacked">stacked bars</option>
          </select>
        </label>
        <label className="usage-check">
          <input type="checkbox" checked={compare} onChange={(e) => setParam({ compare: e.target.checked ? "1" : null })} />
          compare with the previous period
        </label>
        <div className="usage-filterbar-actions">
          <button type="button" className="ghost-btn" onClick={() => setTick((t) => t + 1)} disabled={loading}>
            {loading ? "loading…" : "refresh"}
          </button>
          <button type="button" className="ghost-btn" onClick={exportCSV} disabled={tableRows.length === 0}>
            export CSV
          </button>
        </div>
        {chips.length > 0 && (
          <div className="usage-chips">
            {chips.map((c) => (
              <span key={c.label} className="usage-chip">
                {c.label}
                <button type="button" aria-label={`remove ${c.label}`} onClick={c.clear}>
                  ×
                </button>
              </span>
            ))}
            {chips.length > 1 && (
              <button
                type="button"
                className="usage-chip-clear"
                onClick={() => setParam(Object.fromEntries([["tenant", ""], ...FILTER_DIMS.map((d) => [d, null])]))}
              >
                clear all
              </button>
            )}
          </div>
        )}
      </div>

      {err && <div className="err">{err}</div>}

      {data && (
        <div className={"usage-body" + (loading ? " is-loading" : "")}>
          <div className="usage-summary usage-figures">
            <Figure label={`total ${MEASURES.find((m) => m.id === measure)?.label}`} value={fmt(total)} delta={delta} />
            <Figure label="calls" value={calls.toLocaleString()} />
            {measure === "cost" && <Figure label="operator-paid" value={fmt(operatorPaid)} />}
            {measure === "cost" && <Figure label="tenant-paid" value={fmt(tenantPaid)} />}
            {(tab === "overview" || tab === "background") && allTotal > 0 && (
              <Figure label="background share" value={`${Math.round((bgTotal / allTotal) * 100)}%`} />
            )}
            {unpriced > 0 && <Figure label="unpriced calls" value={unpriced.toLocaleString()} warn />}
          </div>

          {tab === "overview" && data.overview ? (
            <Overview
              data={data.overview}
              seriesRows={cur(data.series)}
              measure={measure}
              currency={currency}
              step={step}
              mode={mode}
              previous={previous}
              isAdmin={isAdmin}
              names={names}
              palette={palette}
              cur={cur}
              slotsFor={slotsFor}
              onPick={pickRow}
            />
          ) : (
            <div className="usage-charts">
              <Donut
                title={`share by ${noun}`}
                slices={tabSlices}
                slots={tabSlots}
                palette={palette}
                measure={measure}
                currency={currency}
                onPick={pickSlice(spec)}
              />
              <TimeChart
                title={`${MEASURES.find((m) => m.id === measure)?.label} per ${step}, by ${noun}`}
                data={tabSeries}
                slots={tabSlots}
                palette={palette}
                measure={measure}
                currency={currency}
                step={step}
                mode={mode}
                previous={previous}
                onPick={(k) => {
                  const r = (data.main ?? []).find((x) => spec.key(x) === k);
                  pickRow(spec, r, k === OTHER_KEY);
                }}
              />
            </div>
          )}

          <div className="usage-table-wrap" ref={tableRef}>
            <table className="usage-table">
              <thead>
                <tr>
                  <th>{tab === "overview" ? "provider" : noun}</th>
                  <th className="num">input</th>
                  <th className="num">output</th>
                  <th className="num">cache</th>
                  {hasAudio && <th className="num">audio</th>}
                  {hasChars && <th className="num">characters</th>}
                  <th className="num">calls</th>
                  <th className="num">cost</th>
                  <th className="num">share</th>
                </tr>
              </thead>
              <tbody>
                {tableRows.map((r) => (
                  <tr key={spec.key(r)} className="usage-row-pick" onClick={() => pickRow(spec, r)} tabIndex={0}
                    onKeyDown={(e) => {
                      if (e.key === "Enter") pickRow(spec, r);
                    }}>
                    <td>
                      <span className="usage-swatch" style={{ background: colorOf(palette, tabSlots, spec.key(r)) }} />
                      {spec.label(r, names)}
                    </td>
                    <td className="num">{formatMeasure(r.input_tokens, "tokens")}</td>
                    <td className="num">{formatMeasure(r.output_tokens, "tokens")}</td>
                    <td className="num">{formatMeasure(r.cache_creation_tokens + r.cache_read_tokens, "tokens")}</td>
                    {hasAudio && <td className="num">{(r.audio_seconds ?? 0) > 0 ? formatMeasure((r.audio_seconds ?? 0) / 60, "audio") : "—"}</td>}
                    {hasChars && <td className="num">{(r.input_chars ?? 0) > 0 ? formatMeasure(r.input_chars ?? 0, "chars") : "—"}</td>}
                    <td className="num">
                      {r.call_count.toLocaleString()}
                      {r.unpriced_calls > 0 && <span className="usage-unpriced"> ({r.unpriced_calls} unpriced)</span>}
                    </td>
                    <td className="num">{formatMeasure(r.cost, "cost", measure === "cost" ? currency : r.currency)}</td>
                    <td className="num">{tableTotal > 0 ? `${((measureOf(r, measure) / tableTotal) * 100).toFixed(1)}%` : "—"}</td>
                  </tr>
                ))}
              </tbody>
            </table>
            {tableRows.length === 0 && <div className="empty">no usage in this window.</div>}
          </div>
          {tab === "users" && (
            <p className="usage-note">A background call (a summary, a rerank, an embedding) is listed under the user it was made for.</p>
          )}
          {data.main.length > 0 && detailSince && (
            <p className="usage-note">Per-call detail starts {new Date(detailSince).toUTCString()}; earlier days come from the daily archive.</p>
          )}
        </div>
      )}
    </div>
  );
}

function Figure(props: { label: string; value: string; delta?: number; warn?: boolean }) {
  return (
    <div className={"usage-stat" + (props.warn ? " usage-warn" : "")}>
      <span className="usage-stat-label">{props.label}</span>
      <span className="usage-stat-value">{props.value}</span>
      {props.delta !== undefined && (
        <span className="usage-stat-delta">
          {props.delta >= 0 ? "▲" : "▼"} {Math.abs(props.delta * 100).toFixed(0)}% vs previous
        </span>
      )}
    </div>
  );
}

function Overview(props: {
  data: NonNullable<Loaded["overview"]>;
  seriesRows: Rows;
  measure: Measure;
  currency: string;
  step: Step;
  mode: "lines" | "stacked";
  previous?: Array<number | undefined>;
  isAdmin: boolean;
  names: Map<string, string>;
  palette: ReturnType<typeof usePalette>;
  cur: (r: Rows | undefined) => Rows;
  slotsFor: (dim: string, keys: string[]) => Map<string, number>;
  onPick: (s: DimSpec, r: UsageAggregate | undefined, other?: boolean) => void;
}) {
  const { data, measure, currency, palette, cur, slotsFor, onPick } = props;
  const provSeries = series(props.seriesRows, DIMS.provider, measure);
  const donut = (title: string, spec: DimSpec, rows: Rows, dim: string) => {
    const s = slices(cur(rows), spec, measure, props.names);
    const slots = slotsFor(dim, s.map((x) => x.key));
    return (
      <Donut
        key={dim}
        title={title}
        slices={s}
        slots={slots}
        palette={palette}
        measure={measure}
        currency={currency}
        size={140}
        onPick={(sl) => onPick(spec, sl.row, sl.other)}
      />
    );
  };
  const provSlots = slotsFor("overview:provider", [...provSeries.keys]);
  const topSpec = props.isAdmin ? DIMS.tenant : DIMS.user;
  const top = slices(cur(data.top), topSpec, measure, props.names).filter((s) => !s.other);
  return (
    <>
      <TimeChart
        title={`${measure === "cost" ? "cost" : measure} per ${props.step}, by provider`}
        data={provSeries}
        slots={provSlots}
        palette={palette}
        measure={measure}
        currency={currency}
        step={props.step}
        mode={props.mode}
        previous={props.previous}
        onPick={(k) => onPick(DIMS.provider, cur(data.provider).find((r) => DIMS.provider.key(r) === k), k === OTHER_KEY)}
      />
      <div className="usage-donut-grid">
        {donut("by model", DIMS.model, data.model, "overview:model")}
        {donut("by provider", DIMS.provider, data.provider, "overview:provider")}
        {donut("by model tier", DIMS.tier, data.tier, "overview:tier")}
        {donut("foreground and background", DIMS.class, data.class, "overview:class")}
      </div>
      {top.length > 0 && (
        <div className="usage-card">
          <div className="usage-card-title">largest {props.isAdmin ? "tenants" : "users"}</div>
          <ol className="usage-ranked">
            {top.map((s) => (
              <li key={s.key}>
                <button type="button" onClick={() => onPick(topSpec, s.row)}>
                  <span className="usage-ranked-label">{s.label}</span>
                  <span className="usage-ranked-bar" aria-hidden>
                    <span style={{ width: `${Math.max(2, s.share * 100)}%`, background: palette.series[0] }} />
                  </span>
                  <span className="usage-legend-value">{formatMeasure(s.value, measure, currency)}</span>
                </button>
              </li>
            ))}
          </ol>
        </div>
      )}
    </>
  );
}
