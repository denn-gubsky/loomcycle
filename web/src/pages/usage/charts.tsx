import { useEffect, useMemo, useState } from "react";
import {
  Bar,
  CartesianGrid,
  ComposedChart,
  Line,
  Pie,
  PieChart,
  ResponsiveContainer,
  Tooltip,
  XAxis,
  YAxis,
} from "recharts";
import {
  OTHER_KEY,
  SERIES_SLOTS,
  compact,
  formatMeasure,
  type Measure,
  type SeriesData,
  type Slice,
  type Step,
} from "../../lib/usageReport";

// The charts read their colours from the theme tokens at render, resolved to
// concrete values: an SVG presentation attribute is the one place a var() is
// not guaranteed, and a resolved palette re-renders when the theme flips.

export interface Palette {
  series: string[]; // SERIES_SLOTS colours, in slot order
  other: string;
  grid: string;
  axis: string;
  surface: string;
  text: string;
  muted: string;
}

function readPalette(): Palette {
  const cs = getComputedStyle(document.documentElement);
  const v = (name: string, fallback: string) => cs.getPropertyValue(name).trim() || fallback;
  const series: string[] = [];
  for (let i = 1; i <= SERIES_SLOTS; i++) series.push(v(`--lc-series-${i}`, "#3987e5"));
  return {
    series,
    other: v("--lc-series-other", "#888888"),
    grid: v("--lc-chart-grid", "#2a2e3a"),
    axis: v("--lc-chart-axis", "#9aa0ad"),
    surface: v("--lc-surface", "#161922"),
    text: v("--lc-text", "#e6e6e6"),
    muted: v("--lc-text-muted", "#9aa0ad"),
  };
}

// usePalette follows the theme: the <html data-theme> toggle and, through it,
// the OS preference the app resolves at boot.
export function usePalette(): Palette {
  const [p, setP] = useState<Palette>(() => readPalette());
  useEffect(() => {
    const obs = new MutationObserver(() => setP(readPalette()));
    obs.observe(document.documentElement, { attributes: true, attributeFilter: ["data-theme", "class", "style"] });
    return () => obs.disconnect();
  }, []);
  return p;
}

// colorOf is a value's colour: its slot's, or the neutral for "other" and for a
// value no chart draws (a table row below the top), which must not borrow a
// charted value's colour.
export function colorOf(p: Palette, slots: Map<string, number>, key: string): string {
  const slot = key === OTHER_KEY ? undefined : slots.get(key);
  if (slot === undefined) return p.other;
  return p.series[slot % p.series.length] ?? p.other;
}

// formatPeriod writes a bucket start for its step, in UTC like the buckets.
export function formatPeriod(iso: string, step: Step, long = false): string {
  const d = new Date(iso);
  if (isNaN(d.getTime())) return iso;
  const o: Intl.DateTimeFormatOptions = { timeZone: "UTC" };
  switch (step) {
    case "hour":
      Object.assign(o, long ? { month: "short", day: "numeric", hour: "2-digit", minute: "2-digit" } : { hour: "2-digit", minute: "2-digit" });
      break;
    case "month":
      Object.assign(o, { month: "short", year: "numeric" });
      break;
    case "week":
      return (long ? "week of " : "") + d.toLocaleDateString(undefined, { ...o, month: "short", day: "numeric" });
    default:
      Object.assign(o, { month: "short", day: "numeric", ...(long ? { year: "numeric" } : {}) });
  }
  return step === "hour" && !long ? d.toLocaleTimeString(undefined, o) : d.toLocaleDateString(undefined, o);
}

// --- donut ---------------------------------------------------------------

export function Donut(props: {
  title: string;
  slices: Slice[];
  slots: Map<string, number>;
  palette: Palette;
  measure: Measure;
  currency?: string;
  size?: number;
  onPick?: (s: Slice) => void;
}) {
  const { slices, palette, measure, currency, onPick } = props;
  const size = props.size ?? 180;
  const total = slices.reduce((s, x) => s + x.value, 0);
  const data = slices.map((s) => ({ ...s, fill: colorOf(palette, props.slots, s.key) }));
  const summary =
    `${props.title}: ` + slices.map((s) => `${s.label} ${Math.round(s.share * 100)}%`).join(", ");
  if (slices.length === 0) {
    return (
      <div className="usage-card usage-donut">
        <div className="usage-card-title">{props.title}</div>
        <div className="usage-empty-chart">nothing in this window</div>
      </div>
    );
  }
  return (
    <div className="usage-card usage-donut">
      <div className="usage-card-title">{props.title}</div>
      <div className="usage-donut-body">
        <div className="usage-donut-chart" style={{ width: size, height: size }} role="img" aria-label={summary}>
          <PieChart width={size} height={size}>
            <Pie
              data={data}
              dataKey="value"
              nameKey="label"
              innerRadius={size * 0.32}
              outerRadius={size * 0.48}
              stroke={palette.surface}
              strokeWidth={2}
              startAngle={90}
              endAngle={-270}
              isAnimationActive={false}
              onClick={(entry: unknown) => {
                // Recharts hands the sector; the slice it was drawn from is its payload.
                const s = (entry as { payload?: Slice }).payload;
                if (s && onPick) onPick(s);
              }}
              style={{ cursor: onPick ? "pointer" : "default" }}
            />
            <Tooltip
              content={({ active, payload }) => {
                const s = active ? (payload?.[0]?.payload as Slice | undefined) : undefined;
                if (!s) return null;
                return (
                  <div className="usage-tooltip">
                    <div className="usage-tooltip-row">
                      <strong>{formatMeasure(s.value, measure, currency)}</strong>
                      <span>{Math.round(s.share * 100)}%</span>
                    </div>
                    <div className="usage-tooltip-label">{s.label}</div>
                  </div>
                );
              }}
            />
          </PieChart>
          <div className="usage-donut-center" aria-hidden>
            <span className="usage-donut-total">{measure === "cost" ? formatMeasure(total, measure, currency) : compact(total)}</span>
          </div>
        </div>
        <ul className="usage-legend">
          {slices.map((s) => (
            <li key={s.key}>
              <button
                type="button"
                className="usage-legend-item"
                onClick={() => onPick?.(s)}
                disabled={!onPick}
                title={s.other ? "show the rest in the table" : `filter on ${s.label}`}
              >
                <span className="usage-swatch" style={{ background: colorOf(palette, props.slots, s.key) }} />
                <span className="usage-legend-label">{s.label}</span>
                <span className="usage-legend-value">{formatMeasure(s.value, measure, currency)}</span>
                <span className="usage-legend-share">{Math.round(s.share * 100)}%</span>
              </button>
            </li>
          ))}
        </ul>
      </div>
    </div>
  );
}

// --- over time -------------------------------------------------------------

export function TimeChart(props: {
  title: string;
  data: SeriesData;
  slots: Map<string, number>;
  palette: Palette;
  measure: Measure;
  currency?: string;
  step: Step;
  mode: "lines" | "stacked";
  previous?: Array<number | undefined>;
  onPick?: (key: string) => void;
}) {
  const { data, palette, measure, currency, step, mode } = props;
  const [hidden, setHidden] = useState<Set<string>>(new Set());
  const points = useMemo(() => {
    if (!props.previous || mode !== "lines") return data.points;
    // The previous window's i-th period beside this one's; a period it lacks
    // leaves a gap rather than a false zero.
    return data.points.map((p, i) => {
      const v = props.previous?.[i];
      return v === undefined ? p : { ...p, __prev: v };
    });
  }, [data.points, props.previous, mode]);
  const shown = data.keys.filter((k) => !hidden.has(k));
  const toggle = (k: string) =>
    setHidden((h) => {
      const n = new Set(h);
      if (n.has(k)) n.delete(k);
      else n.add(k);
      return n;
    });
  const summary = `${props.title}: ${data.keys.map((k) => data.labels.get(k) ?? k).join(", ")}, ${data.points.length} periods`;
  if (data.points.length === 0) {
    return (
      <div className="usage-card usage-time">
        <div className="usage-card-title">{props.title}</div>
        <div className="usage-empty-chart">nothing in this window</div>
      </div>
    );
  }
  return (
    <div className="usage-card usage-time">
      <div className="usage-card-title">
        {props.title}
        <span className="usage-card-note">UTC</span>
      </div>
      <div className="usage-time-chart" role="img" aria-label={summary}>
        <ResponsiveContainer width="100%" height={260}>
          <ComposedChart data={points} margin={{ top: 8, right: 12, bottom: 0, left: 0 }}>
            <CartesianGrid vertical={false} stroke={palette.grid} strokeWidth={1} />
            <XAxis
              dataKey="period"
              tickFormatter={(v: string) => formatPeriod(v, step)}
              stroke={palette.axis}
              tick={{ fill: palette.muted, fontSize: 11 }}
              tickLine={false}
              axisLine={{ stroke: palette.grid }}
              minTickGap={24}
            />
            <YAxis
              tickFormatter={(v: number) => compact(v)}
              stroke={palette.axis}
              tick={{ fill: palette.muted, fontSize: 11 }}
              tickLine={false}
              axisLine={false}
              width={48}
            />
            <Tooltip
              cursor={mode === "lines" ? { stroke: palette.axis, strokeWidth: 1 } : { fill: palette.grid, opacity: 0.4 }}
              content={({ active, payload, label }) => {
                if (!active || !payload?.length) return null;
                const rows = payload
                  .filter((p) => p.dataKey !== "__prev")
                  .map((p) => ({ key: String(p.dataKey), value: Number(p.value ?? 0) }))
                  .sort((a, b) => b.value - a.value);
                const prev = payload.find((p) => p.dataKey === "__prev");
                return (
                  <div className="usage-tooltip">
                    <div className="usage-tooltip-label">{formatPeriod(String(label), step, true)}</div>
                    {rows.map((r) => (
                      <div key={r.key} className="usage-tooltip-row">
                        <span className="usage-line-key" style={{ background: colorOf(palette, props.slots, r.key) }} />
                        <strong>{formatMeasure(r.value, measure, currency)}</strong>
                        <span>{data.labels.get(r.key) ?? r.key}</span>
                      </div>
                    ))}
                    {prev && prev.value != null && (
                      <div className="usage-tooltip-row usage-tooltip-prev">
                        <span className="usage-line-key usage-line-key-dashed" />
                        <strong>{formatMeasure(Number(prev.value), measure, currency)}</strong>
                        <span>previous period</span>
                      </div>
                    )}
                  </div>
                );
              }}
            />
            {mode === "lines"
              ? shown.map((k) => (
                  <Line
                    key={k}
                    dataKey={k}
                    type="linear"
                    stroke={colorOf(palette, props.slots, k)}
                    strokeWidth={2}
                    strokeLinecap="round"
                    strokeLinejoin="round"
                    dot={false}
                    activeDot={{ r: 4, stroke: palette.surface, strokeWidth: 2 }}
                    isAnimationActive={false}
                  />
                ))
              : shown.map((k, i) => (
                  <Bar
                    key={k}
                    dataKey={k}
                    stackId="s"
                    fill={colorOf(palette, props.slots, k)}
                    stroke={palette.surface}
                    strokeWidth={1}
                    maxBarSize={24}
                    radius={i === shown.length - 1 ? [4, 4, 0, 0] : 0}
                    isAnimationActive={false}
                    onClick={() => k !== OTHER_KEY && props.onPick?.(k)}
                    style={{ cursor: props.onPick && k !== OTHER_KEY ? "pointer" : "default" }}
                  />
                ))}
            {mode === "lines" && props.previous && (
              <Line
                dataKey="__prev"
                type="linear"
                stroke={palette.axis}
                strokeWidth={2}
                strokeDasharray="4 4"
                dot={false}
                activeDot={false}
                isAnimationActive={false}
              />
            )}
          </ComposedChart>
        </ResponsiveContainer>
      </div>
      <ul className="usage-legend usage-legend-row">
        {data.keys.map((k) => (
          <li key={k}>
            <button
              type="button"
              className={"usage-legend-item" + (hidden.has(k) ? " is-hidden" : "")}
              onClick={() => toggle(k)}
              aria-pressed={!hidden.has(k)}
              title={hidden.has(k) ? "show this series" : "hide this series"}
            >
              {mode === "lines" ? (
                <span className="usage-line-key" style={{ background: colorOf(palette, props.slots, k) }} />
              ) : (
                <span className="usage-swatch" style={{ background: colorOf(palette, props.slots, k) }} />
              )}
              <span className="usage-legend-label">{data.labels.get(k) ?? k}</span>
            </button>
          </li>
        ))}
        {mode === "lines" && props.previous && (
          <li>
            <span className="usage-legend-item usage-legend-static">
              <span className="usage-line-key usage-line-key-dashed" />
              <span className="usage-legend-label">previous period (total)</span>
            </span>
          </li>
        )}
      </ul>
    </div>
  );
}
