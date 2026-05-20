import { useEffect, useMemo, useState } from "react";
import {
  ProcessSample,
  SummaryBucket,
  getHealth,
  getMetricsSamples,
  getMetricsSummary,
} from "../api";
import ActivityToolbar, { type ActivityMode } from "../components/activity/ActivityToolbar";
import ChartCard from "../components/activity/ChartCard";
import LineChart, { type Series } from "../components/activity/LineChart";
import MetricsDisabledEmpty from "../components/activity/MetricsDisabledEmpty";

// ActivityMonitor — operator-facing live charts of CPU, memory,
// agent load, and (optionally) host metrics. Backed by the v0.8.11
// process-resource metrics sampler.
//
// Layout:
//   ┌─ ActivityToolbar (mode | pause | system | advanced) ─┐
//   │ MetricsDisabledEmpty  *or*  chart grid               │
//   └──────────────────────────────────────────────────────┘
//
// Three primary charts ship in this commit (memory+agents,
// CPU, queue depth). The 'advanced' toggle exposes diagnostic
// charts in the next commit; right now the toggle is wired but
// only the primaries render.

const LIVE_WINDOW_MS = 15 * 60 * 1000; // 15 min live window
const LIVE_POLL_MS = 5000;
const SUMMARY_POLL_MS = 30_000;

// localStorage keys — keep all activity preferences under one
// namespace prefix so they're easy to clear in DevTools.
const LS_MODE = "loomcycle.activity.mode";
const LS_AUTOREFRESH = "loomcycle.activity.autoRefresh";
const LS_SHOW_SYSTEM = "loomcycle.activity.showSystem";
const LS_SHOW_ADVANCED = "loomcycle.activity.showAdvanced";

// Series colors — pulled from the existing palette + the v0.8.21
// awaited-state chip palette to stay consistent with the rest of
// the UI.
const C_MEMORY = "#5b9dff";     // --accent
const C_AGENTS = "#ffb766";     // await-channel orange
const C_QUEUED = "#5b9dff";     // accent (top of stack)
const C_ACTIVE = "#f0a040";     // --running (bottom of stack)
const C_CPU_PROCESS = "#5b9dff";
const C_CPU_SYSTEM = "#9aa0ad"; // --fg-soft, dashed via opacity

function readLSBool(key: string, fallback: boolean): boolean {
  const v = localStorage.getItem(key);
  if (v === "1") return true;
  if (v === "0") return false;
  return fallback;
}

function readLSMode(fallback: ActivityMode): ActivityMode {
  const v = localStorage.getItem(LS_MODE);
  if (v === "live" || v === "1h" || v === "24h" || v === "7d") return v;
  return fallback;
}

export default function ActivityMonitor() {
  // Preference state — initialised from localStorage so a reload
  // lands the operator on the same view.
  const [mode, setMode] = useState<ActivityMode>(() => readLSMode("live"));
  const [autoRefresh, setAutoRefresh] = useState<boolean>(() => readLSBool(LS_AUTOREFRESH, true));
  const [showSystem, setShowSystem] = useState<boolean>(() => readLSBool(LS_SHOW_SYSTEM, false));
  const [showAdvanced, setShowAdvanced] = useState<boolean>(() => readLSBool(LS_SHOW_ADVANCED, false));

  // Data state.
  const [samples, setSamples] = useState<ProcessSample[]>([]);
  const [summary, setSummary] = useState<SummaryBucket[]>([]);
  const [disabled, setDisabled] = useState<string | null>(null);
  const [loading, setLoading] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const [lastUpdated, setLastUpdated] = useState<number | null>(null);

  // On mount, probe /healthz so we can render the disabled empty
  // state BEFORE making a metrics request that would 503. This is
  // also why the helper /healthz now ships metrics_enabled.
  useEffect(() => {
    let cancelled = false;
    getHealth()
      .then((h) => {
        if (cancelled) return;
        if (h.metrics_enabled === false) {
          setDisabled("set LOOMCYCLE_METRICS_ENABLED=1 and restart loomcycle");
        }
      })
      .catch(() => {
        // Health probe failing is a different problem; the metrics
        // fetch below will surface its own error.
      });
    return () => {
      cancelled = true;
    };
  }, []);

  // Persist preferences on change.
  useEffect(() => localStorage.setItem(LS_MODE, mode), [mode]);
  useEffect(() => localStorage.setItem(LS_AUTOREFRESH, autoRefresh ? "1" : "0"), [autoRefresh]);
  useEffect(() => localStorage.setItem(LS_SHOW_SYSTEM, showSystem ? "1" : "0"), [showSystem]);
  useEffect(() => localStorage.setItem(LS_SHOW_ADVANCED, showAdvanced ? "1" : "0"), [showAdvanced]);

  // Polling lifecycle. One useEffect keyed on [mode, autoRefresh].
  // On mode change we drop the prior data so the chart doesn't
  // briefly render samples against the wrong X domain.
  useEffect(() => {
    setSamples([]);
    setSummary([]);
    setErr(null);
    if (!autoRefresh) return;
    if (disabled) return; // already known disabled — skip polling

    let cancelled = false;
    const tick = async () => {
      setLoading(true);
      try {
        if (mode === "live") {
          const since = new Date(Date.now() - LIVE_WINDOW_MS).toISOString();
          const r = await getMetricsSamples({ since, limit: 200 });
          if (cancelled) return;
          if (r.disabled) {
            setDisabled(r.enableHint);
          } else {
            setSamples(r.data.samples);
            setLastUpdated(Date.now());
          }
        } else {
          const r = await getMetricsSummary(mode);
          if (cancelled) return;
          if (r.disabled) {
            setDisabled(r.enableHint);
          } else {
            setSummary(r.data.buckets);
            setLastUpdated(Date.now());
          }
        }
        setErr(null);
      } catch (e) {
        if (!cancelled) setErr(e instanceof Error ? e.message : String(e));
      } finally {
        if (!cancelled) setLoading(false);
      }
    };
    tick();
    const id = setInterval(tick, mode === "live" ? LIVE_POLL_MS : SUMMARY_POLL_MS);
    return () => {
      cancelled = true;
      clearInterval(id);
    };
  }, [mode, autoRefresh, disabled]);

  // Detect whether the server is shipping system_* fields. The
  // sampler may be enabled but COLLECT_SYSTEM may not be — the
  // toggle is rendered disabled when this is false.
  const systemAvailable = useMemo(() => {
    if (mode === "live") {
      return samples.some((s) => s.system_cpu_pct_x100 != null);
    }
    // Summary buckets don't carry system_* separately; we'd need a
    // probe sample. For window modes, fall back to the live samples
    // we accumulated (none, on first window-mode load). Treat as
    // unavailable until the user toggles to live and back —
    // pragmatic; the toggle's tooltip explains.
    return false;
  }, [mode, samples]);

  const statusText = useMemo(() => {
    if (disabled) return undefined;
    if (loading && lastUpdated === null) return "loading…";
    if (!autoRefresh) return "paused";
    if (lastUpdated == null) return undefined;
    const ageS = Math.round((Date.now() - lastUpdated) / 1000);
    return `updated ${ageS}s ago`;
  }, [disabled, loading, lastUpdated, autoRefresh]);

  return (
    <div className="activity-view">
      <ActivityToolbar
        mode={mode}
        onModeChange={setMode}
        autoRefresh={autoRefresh}
        onAutoRefreshChange={setAutoRefresh}
        systemAvailable={systemAvailable}
        showSystem={showSystem}
        onShowSystemChange={setShowSystem}
        showAdvanced={showAdvanced}
        onShowAdvancedChange={setShowAdvanced}
        statusText={statusText}
      />

      {err && !disabled && <div className="err">{err}</div>}

      {disabled ? (
        <MetricsDisabledEmpty enableHint={disabled} />
      ) : (
        <div className="activity-grid">
          <MemoryVsAgentsCard samples={samples} buckets={summary} mode={mode} />
          <CPUCard samples={samples} buckets={summary} mode={mode} showSystem={showSystem && systemAvailable} />
          <QueueDepthCard samples={samples} buckets={summary} mode={mode} />
        </div>
      )}
    </div>
  );
}

// --- Chart cards ----------------------------------------------------

interface CardProps {
  samples: ProcessSample[];
  buckets: SummaryBucket[];
  mode: ActivityMode;
}

function MemoryVsAgentsCard({ samples, buckets, mode }: CardProps) {
  const memSeries: Series[] = [];
  const agentSeries: Series[] = [];
  if (mode === "live") {
    memSeries.push({
      label: "memory",
      color: C_MEMORY,
      points: samples.map((s) => ({ t: tsMs(s.sampled_at), y: bytesToMB(s.loomcycle_rss_bytes) })),
    });
    agentSeries.push({
      label: "active agents",
      color: C_AGENTS,
      yAxis: "right",
      points: samples.map((s) => ({ t: tsMs(s.sampled_at), y: s.active_runs })),
    });
  } else {
    memSeries.push({
      label: "memory (mean)",
      color: C_MEMORY,
      points: buckets.map((b) => ({ t: tsMs(b.at), y: bytesToMB(b.mean_rss_bytes) })),
    });
    agentSeries.push({
      label: "active agents (max)",
      color: C_AGENTS,
      yAxis: "right",
      points: buckets.map((b) => ({ t: tsMs(b.at), y: b.active_runs_max })),
    });
  }
  const current = latestPoint(memSeries[0]?.points)?.y;
  const currentAgents = latestPoint(agentSeries[0]?.points)?.y;
  return (
    <ChartCard
      title="memory vs running agents"
      currentValue={
        current != null && currentAgents != null
          ? `${formatMB(current)} · ${currentAgents} run${currentAgents === 1 ? "" : "s"}`
          : undefined
      }
      legend={[
        { label: "memory (MB, left axis)", color: C_MEMORY },
        { label: "active agents (count, right axis)", color: C_AGENTS },
      ]}
    >
      <LineChart
        series={[...memSeries, ...agentSeries]}
        yLeftFormat={(v) => `${v.toFixed(0)} MB`}
        yRightFormat={(v) => v.toFixed(0)}
        xFormat={xFormatForMode(mode)}
      />
    </ChartCard>
  );
}

function CPUCard({
  samples,
  buckets,
  mode,
  showSystem,
}: CardProps & { showSystem: boolean }) {
  const series: Series[] = [];
  if (mode === "live") {
    series.push({
      label: "process CPU",
      color: C_CPU_PROCESS,
      points: samples.map((s) => ({ t: tsMs(s.sampled_at), y: s.loomcycle_cpu_pct_x100 / 100 })),
    });
    if (showSystem) {
      series.push({
        label: "system CPU",
        color: C_CPU_SYSTEM,
        points: samples
          .filter((s) => s.system_cpu_pct_x100 != null)
          .map((s) => ({ t: tsMs(s.sampled_at), y: (s.system_cpu_pct_x100 as number) / 100 })),
      });
    }
  } else {
    series.push({
      label: "process CPU (p95)",
      color: C_CPU_PROCESS,
      points: buckets.map((b) => ({ t: tsMs(b.at), y: b.p95_cpu_pct_x100 / 100 })),
    });
    // System CPU isn't bucketed by the summary endpoint, so the
    // overlay is live-only in window modes.
  }
  const cur = latestPoint(series[0]?.points)?.y;
  return (
    <ChartCard
      title="CPU load"
      currentValue={cur != null ? `${cur.toFixed(1)}%` : undefined}
      legend={[
        { label: "process CPU %", color: C_CPU_PROCESS },
        ...(showSystem ? [{ label: "system CPU %", color: C_CPU_SYSTEM }] : []),
      ]}
    >
      <LineChart
        series={series}
        yLeftDomain={[0, 100]}
        yLeftFormat={(v) => `${v.toFixed(0)}%`}
        xFormat={xFormatForMode(mode)}
      />
    </ChartCard>
  );
}

function QueueDepthCard({ samples, buckets, mode }: CardProps) {
  // Stacked area: active on the bottom, queued on top.
  // Summary endpoint doesn't carry queued_runs separately — only
  // active_runs_max — so the window-mode chart degrades to one
  // band (active only).
  const series: Series[] = [];
  if (mode === "live") {
    series.push({
      label: "active",
      color: C_ACTIVE,
      fill: true,
      points: samples.map((s) => ({ t: tsMs(s.sampled_at), y: s.active_runs })),
    });
    series.push({
      label: "queued",
      color: C_QUEUED,
      fill: true,
      points: samples.map((s) => ({ t: tsMs(s.sampled_at), y: s.queued_runs })),
    });
  } else {
    series.push({
      label: "active (max per bucket)",
      color: C_ACTIVE,
      fill: true,
      points: buckets.map((b) => ({ t: tsMs(b.at), y: b.active_runs_max })),
    });
  }
  const totalNow =
    (latestPoint(series[0]?.points)?.y ?? 0) + (latestPoint(series[1]?.points)?.y ?? 0);
  return (
    <ChartCard
      title="queue depth"
      currentValue={`${totalNow} total`}
      legend={
        mode === "live"
          ? [
              { label: "active runs", color: C_ACTIVE },
              { label: "queued runs", color: C_QUEUED },
            ]
          : [{ label: "active runs (max)", color: C_ACTIVE }]
      }
    >
      <LineChart
        series={series}
        yLeftFormat={(v) => v.toFixed(0)}
        xFormat={xFormatForMode(mode)}
      />
    </ChartCard>
  );
}

// --- formatting helpers --------------------------------------------

function tsMs(s: string): number {
  return Date.parse(s);
}

function bytesToMB(b: number): number {
  return b / (1024 * 1024);
}

function formatMB(v: number): string {
  if (v >= 1024) return `${(v / 1024).toFixed(2)} GB`;
  return `${v.toFixed(0)} MB`;
}

function latestPoint(points: { t: number; y: number }[] | undefined) {
  if (!points || points.length === 0) return undefined;
  return points.reduce((a, b) => (a.t > b.t ? a : b));
}

function xFormatForMode(mode: ActivityMode) {
  return (t: number) => {
    const d = new Date(t);
    const pad = (n: number) => String(n).padStart(2, "0");
    if (mode === "live" || mode === "1h") {
      return `${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}`;
    }
    if (mode === "24h") {
      return `${pad(d.getHours())}:${pad(d.getMinutes())}`;
    }
    // 7d
    return `${d.getMonth() + 1}/${d.getDate()} ${pad(d.getHours())}:${pad(d.getMinutes())}`;
  };
}
