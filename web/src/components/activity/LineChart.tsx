import { useEffect, useMemo, useRef, useState } from "react";

// LineChart — hand-rolled SVG time series chart used by the
// Activity Monitor tab. Zero dependencies. Supports:
//   - multiple series (line OR stacked-area, mixed in one chart)
//   - dual-axis (yAxis: "left" | "right" per series)
//   - automatic gap detection (a path breaks if two adjacent
//     points are > gapThresholdMs apart, so idle-gated sampling
//     doesn't draw a straight line across hours of no data)
//   - hover tooltip snapped to the nearest X point
//   - responsive resize via ResizeObserver (the SVG uses a fixed
//     viewBox matched to measured width, so labels stay crisp)
//
// Why hand-rolled instead of recharts/uplot: loomcycle keeps its
// web/ deps minimal (3 runtime packages today). The chart shape we
// need is uniform across the 6 Activity-Monitor cards; a 200-LOC
// component beats a 80KB dep we'd have to theme. If we ever add a
// chart type that doesn't fit this shape (e.g. a heatmap), the
// trade-off shifts.

export interface SeriesPoint {
  t: number; // milliseconds since epoch
  y: number; // value at t; in raw units (caller pre-scales if needed)
}

export interface Series {
  label: string;
  color: string;
  points: SeriesPoint[];
  // "left" (default) or "right". Two-axis charts use one of each.
  yAxis?: "left" | "right";
  // true → render as filled area (stacks on previous fill series).
  // false/undefined → line only.
  fill?: boolean;
}

export interface LineChartProps {
  series: Series[];
  // Override the auto-computed domain (e.g. CPU% always 0..100).
  yLeftDomain?: [number, number];
  yRightDomain?: [number, number];
  // Formatters for axis-tick labels and tooltip values.
  yLeftFormat?: (v: number) => string;
  yRightFormat?: (v: number) => string;
  // X-tick label formatter (default: HH:MM:SS for live, M/D HH:MM
  // for longer windows — caller picks via the formatter passed in).
  xFormat?: (t: number) => string;
  // Visual height of the chart in pixels.
  height?: number;
  // Gap threshold in ms — adjacent points farther apart than this
  // produce a path break (no straight line bridging the gap).
  // Default 30s matches the sampler's worst-case idle gap.
  gapThresholdMs?: number;
}

const PAD_LEFT = 56;
const PAD_RIGHT = 56;
const PAD_TOP = 8;
const PAD_BOTTOM = 24;

export default function LineChart({
  series,
  yLeftDomain,
  yRightDomain,
  yLeftFormat = formatNumber,
  yRightFormat = formatNumber,
  xFormat = formatTimeHMS,
  height = 180,
  gapThresholdMs = 30_000,
}: LineChartProps) {
  const wrapRef = useRef<HTMLDivElement | null>(null);
  const [width, setWidth] = useState(600);
  const [hover, setHover] = useState<{ x: number; t: number } | null>(null);

  // ResizeObserver gives us the actual rendered width. The SVG
  // viewBox is set to match so paths render 1:1 with the container.
  useEffect(() => {
    if (!wrapRef.current) return;
    const el = wrapRef.current;
    const ro = new ResizeObserver(() => {
      setWidth(el.clientWidth || 600);
    });
    ro.observe(el);
    setWidth(el.clientWidth || 600);
    return () => ro.disconnect();
  }, []);

  // Domains: X from full point set; Y from each axis's series.
  const xDomain = useMemo(() => computeXDomain(series), [series]);
  const computedLeft = useMemo(() => computeYDomain(series, "left"), [series]);
  const computedRight = useMemo(() => computeYDomain(series, "right"), [series]);
  const leftDom = yLeftDomain ?? computedLeft;
  const rightDom = yRightDomain ?? computedRight;

  const plotW = Math.max(20, width - PAD_LEFT - PAD_RIGHT);
  const plotH = Math.max(20, height - PAD_TOP - PAD_BOTTOM);

  const xScale = (t: number) =>
    PAD_LEFT + ((t - xDomain[0]) / Math.max(1, xDomain[1] - xDomain[0])) * plotW;
  const yScaleLeft = (v: number) =>
    PAD_TOP + plotH - ((v - leftDom[0]) / Math.max(1e-9, leftDom[1] - leftDom[0])) * plotH;
  const yScaleRight = (v: number) =>
    PAD_TOP + plotH - ((v - rightDom[0]) / Math.max(1e-9, rightDom[1] - rightDom[0])) * plotH;

  // For stacked areas: the running baseline. Each fill series adds
  // on top of the prior fill series' values at the SAME index. We
  // assume all stacked series share an X grid (the Queue Depth
  // chart's active+queued series are computed from the same sample
  // list, so this holds in practice). Non-stacked series don't
  // touch this state.
  const stackedBaseline = useMemo(() => buildStackedBaseline(series), [series]);

  const xTicks = useMemo(() => makeXTicks(xDomain, plotW), [xDomain, plotW]);
  const yLeftTicks = useMemo(() => makeYTicks(leftDom), [leftDom]);
  const yRightTicks = useMemo(() => makeYTicks(rightDom), [rightDom]);

  const hasAnyPoints = series.some((s) => s.points.length > 0);

  // onMouseMove: find the closest X point across all series.
  const onMove = (e: React.MouseEvent<SVGSVGElement>) => {
    if (!hasAnyPoints) return;
    const rect = e.currentTarget.getBoundingClientRect();
    const px = e.clientX - rect.left;
    if (px < PAD_LEFT || px > PAD_LEFT + plotW) {
      setHover(null);
      return;
    }
    // Use the union of all series' X points and pick the closest.
    let bestT = xDomain[0];
    let bestDist = Infinity;
    for (const s of series) {
      for (const p of s.points) {
        const d = Math.abs(xScale(p.t) - px);
        if (d < bestDist) {
          bestDist = d;
          bestT = p.t;
        }
      }
    }
    setHover({ x: xScale(bestT), t: bestT });
  };

  const hoverValuesByT = (t: number) =>
    series.map((s) => {
      const p = s.points.find((pp) => pp.t === t);
      return { label: s.label, color: s.color, value: p?.y, yAxis: s.yAxis ?? "left" };
    });

  return (
    <div className="line-chart-wrap" ref={wrapRef} style={{ position: "relative" }}>
      <svg
        className="line-chart"
        viewBox={`0 0 ${width} ${height}`}
        preserveAspectRatio="none"
        onMouseMove={onMove}
        onMouseLeave={() => setHover(null)}
      >
        {/* horizontal grid lines on the LEFT axis ticks */}
        {yLeftTicks.map((t, i) => (
          <line
            key={`gh-${i}`}
            x1={PAD_LEFT}
            x2={PAD_LEFT + plotW}
            y1={yScaleLeft(t)}
            y2={yScaleLeft(t)}
            stroke="#2a2e3a"
            strokeOpacity={0.5}
          />
        ))}

        {/* axis lines */}
        <line x1={PAD_LEFT} x2={PAD_LEFT} y1={PAD_TOP} y2={PAD_TOP + plotH} stroke="#2a2e3a" />
        <line
          x1={PAD_LEFT + plotW}
          x2={PAD_LEFT + plotW}
          y1={PAD_TOP}
          y2={PAD_TOP + plotH}
          stroke="#2a2e3a"
        />
        <line
          x1={PAD_LEFT}
          x2={PAD_LEFT + plotW}
          y1={PAD_TOP + plotH}
          y2={PAD_TOP + plotH}
          stroke="#2a2e3a"
        />

        {/* axis tick labels */}
        {yLeftTicks.map((t, i) => (
          <text
            key={`yl-${i}`}
            x={PAD_LEFT - 6}
            y={yScaleLeft(t) + 3}
            textAnchor="end"
            fontSize="10"
            fill="#9aa0ad"
            fontFamily="ui-monospace, monospace"
          >
            {yLeftFormat(t)}
          </text>
        ))}
        {rightDom[0] !== rightDom[1] &&
          yRightTicks.map((t, i) => (
            <text
              key={`yr-${i}`}
              x={PAD_LEFT + plotW + 6}
              y={yScaleRight(t) + 3}
              textAnchor="start"
              fontSize="10"
              fill="#9aa0ad"
              fontFamily="ui-monospace, monospace"
            >
              {yRightFormat(t)}
            </text>
          ))}
        {xTicks.map((t, i) => (
          <text
            key={`x-${i}`}
            x={xScale(t)}
            y={PAD_TOP + plotH + 14}
            textAnchor="middle"
            fontSize="10"
            fill="#9aa0ad"
            fontFamily="ui-monospace, monospace"
          >
            {xFormat(t)}
          </text>
        ))}

        {/* series — areas drawn first (under lines), lines on top */}
        {series.map((s, si) => {
          if (!s.fill) return null;
          const baseline = stackedBaseline[si];
          const path = buildAreaPath(s.points, baseline, xScale, yScaleLeft, gapThresholdMs);
          return (
            <path
              key={`area-${si}`}
              d={path}
              fill={s.color}
              fillOpacity={0.25}
              stroke={s.color}
              strokeWidth={1}
            />
          );
        })}
        {series.map((s, si) => {
          if (s.fill) return null;
          const yScale = (s.yAxis ?? "left") === "right" ? yScaleRight : yScaleLeft;
          const path = buildLinePath(s.points, xScale, yScale, gapThresholdMs);
          return (
            <path
              key={`line-${si}`}
              d={path}
              fill="none"
              stroke={s.color}
              strokeWidth={1.5}
            />
          );
        })}

        {/* hover crosshair */}
        {hover && (
          <line
            x1={hover.x}
            x2={hover.x}
            y1={PAD_TOP}
            y2={PAD_TOP + plotH}
            stroke="#9aa0ad"
            strokeOpacity={0.4}
            strokeDasharray="3 3"
          />
        )}
      </svg>

      {/* tooltip */}
      {hover && (
        <div
          className="line-chart-tooltip"
          style={{
            position: "absolute",
            left:
              hover.x + 140 > width
                ? `${hover.x - 140}px`
                : `${hover.x + 8}px`,
            top: `${PAD_TOP + 4}px`,
            pointerEvents: "none",
          }}
        >
          <div className="tt-time">{xFormat(hover.t)}</div>
          {hoverValuesByT(hover.t).map((v) => (
            <div key={v.label} className="tt-row">
              <span className="tt-swatch" style={{ background: v.color }} />
              <span className="tt-label">{v.label}</span>
              <span className="tt-value">
                {v.value === undefined
                  ? "—"
                  : v.yAxis === "right"
                    ? yRightFormat(v.value)
                    : yLeftFormat(v.value)}
              </span>
            </div>
          ))}
        </div>
      )}

      {!hasAnyPoints && (
        <div className="line-chart-empty">awaiting first sample…</div>
      )}
    </div>
  );
}

// --- helpers --------------------------------------------------------

function computeXDomain(series: Series[]): [number, number] {
  let lo = Infinity;
  let hi = -Infinity;
  for (const s of series) {
    for (const p of s.points) {
      if (p.t < lo) lo = p.t;
      if (p.t > hi) hi = p.t;
    }
  }
  if (!Number.isFinite(lo) || !Number.isFinite(hi)) {
    const now = Date.now();
    return [now - 60_000, now];
  }
  if (lo === hi) return [lo - 60_000, hi];
  return [lo, hi];
}

function computeYDomain(series: Series[], axis: "left" | "right"): [number, number] {
  let lo = Infinity;
  let hi = -Infinity;
  // Stacked-area handling: sum same-X values on left axis when
  // multiple fill series exist, so the y-domain encloses the stack.
  const stackedAt: Map<number, number> = new Map();
  for (const s of series) {
    if ((s.yAxis ?? "left") !== axis) continue;
    for (const p of s.points) {
      if (s.fill && axis === "left") {
        stackedAt.set(p.t, (stackedAt.get(p.t) ?? 0) + p.y);
      } else {
        if (p.y < lo) lo = p.y;
        if (p.y > hi) hi = p.y;
      }
    }
  }
  for (const v of stackedAt.values()) {
    if (v < lo) lo = v;
    if (v > hi) hi = v;
  }
  if (!Number.isFinite(lo) || !Number.isFinite(hi)) return [0, 1];
  if (lo === hi) return [Math.min(0, lo), hi + 1];
  // Pad the top by 8% so the line doesn't kiss the axis.
  const span = hi - lo;
  return [Math.min(0, lo), hi + span * 0.08];
}

// buildStackedBaseline returns, for each series index, the cumulative
// baseline (sum of all PRIOR fill series' values at each X point).
// Non-fill series get an empty map. Used by buildAreaPath as the
// lower edge of the polygon.
function buildStackedBaseline(series: Series[]): Map<number, number>[] {
  const out: Map<number, number>[] = [];
  const running: Map<number, number> = new Map();
  for (const s of series) {
    if (!s.fill) {
      out.push(new Map());
      continue;
    }
    // Snapshot the current running totals as THIS series' baseline.
    out.push(new Map(running));
    for (const p of s.points) {
      running.set(p.t, (running.get(p.t) ?? 0) + p.y);
    }
  }
  return out;
}

function buildLinePath(
  points: SeriesPoint[],
  xScale: (t: number) => number,
  yScale: (v: number) => number,
  gapMs: number,
): string {
  if (points.length === 0) return "";
  const sorted = [...points].sort((a, b) => a.t - b.t);
  let d = "";
  let lastT = -Infinity;
  for (const p of sorted) {
    const cmd = lastT === -Infinity || p.t - lastT > gapMs ? "M" : "L";
    d += `${cmd}${xScale(p.t).toFixed(1)},${yScale(p.y).toFixed(1)} `;
    lastT = p.t;
  }
  return d.trim();
}

function buildAreaPath(
  points: SeriesPoint[],
  baseline: Map<number, number>,
  xScale: (t: number) => number,
  yScale: (v: number) => number,
  gapMs: number,
): string {
  if (points.length === 0) return "";
  const sorted = [...points].sort((a, b) => a.t - b.t);
  // Build the path in segments split by gaps; each segment is a
  // closed polygon (top edge → bottom edge reversed).
  let d = "";
  let segStart = 0;
  const closeSegment = (start: number, end: number) => {
    const top: string[] = [];
    const bottom: string[] = [];
    for (let i = start; i <= end; i++) {
      const p = sorted[i];
      const base = baseline.get(p.t) ?? 0;
      const yTop = yScale(base + p.y);
      const yBot = yScale(base);
      top.push(`${xScale(p.t).toFixed(1)},${yTop.toFixed(1)}`);
      bottom.push(`${xScale(p.t).toFixed(1)},${yBot.toFixed(1)}`);
    }
    bottom.reverse();
    d += `M${top[0]} L${top.slice(1).join(" L")} L${bottom.join(" L")} Z `;
  };
  for (let i = 1; i < sorted.length; i++) {
    if (sorted[i].t - sorted[i - 1].t > gapMs) {
      closeSegment(segStart, i - 1);
      segStart = i;
    }
  }
  closeSegment(segStart, sorted.length - 1);
  return d.trim();
}

function makeXTicks(domain: [number, number], plotW: number): number[] {
  const targetCount = Math.max(2, Math.min(6, Math.floor(plotW / 100)));
  const out: number[] = [];
  for (let i = 0; i < targetCount; i++) {
    out.push(domain[0] + ((domain[1] - domain[0]) * i) / (targetCount - 1));
  }
  return out;
}

function makeYTicks(domain: [number, number]): number[] {
  const ticks = 4;
  const out: number[] = [];
  for (let i = 0; i <= ticks; i++) {
    out.push(domain[0] + ((domain[1] - domain[0]) * i) / ticks);
  }
  return out;
}

function formatNumber(v: number): string {
  if (Math.abs(v) >= 1000) return v.toLocaleString(undefined, { maximumFractionDigits: 0 });
  if (Math.abs(v) >= 10) return v.toFixed(0);
  if (Math.abs(v) >= 1) return v.toFixed(1);
  return v.toFixed(2);
}

function formatTimeHMS(t: number): string {
  const d = new Date(t);
  const pad = (n: number) => String(n).padStart(2, "0");
  return `${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}`;
}
