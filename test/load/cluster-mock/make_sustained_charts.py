#!/usr/bin/env python3
"""
make_sustained_charts.py — time-series charts for run-cluster-sustained.sh.

Reads $RESULTS_DIR/{process_samples.csv,waves.csv} and writes a PNG set
to $RESULTS_DIR/charts/ — the sustained-load analogue of
make_charts.py (which is burst-oriented).

Usage:
    python3 make_sustained_charts.py [RESULTS_DIR]

The 7-chart set:
  1. active_runs_timeseries     per-replica + sum, phase boundaries marked
  2. cpu_timeseries              per-replica loomcycle CPU% + system CPU%
  3. rss_timeseries              per-replica RSS — leak detector
  4. goroutines_timeseries       per-replica goroutine count
  5. wave_latency_over_time     per-wave p50/p95/p99 scatter+line
  6. wave_completion_over_time  per-wave completion % + silent regression
  7. wave_wall_over_time         per-wave wall time (kept-up indicator)
"""

from __future__ import annotations
import csv, sys
from pathlib import Path
from collections import defaultdict
from datetime import datetime, timezone
import matplotlib
matplotlib.use("Agg")
import matplotlib.pyplot as plt
import numpy as np

RESULTS_DIR = Path(sys.argv[1] if len(sys.argv) > 1 else ".").resolve()
CHARTS = RESULTS_DIR / "charts"
CHARTS.mkdir(parents=True, exist_ok=True)

SAMPLES_CSV = RESULTS_DIR / "process_samples.csv"
WAVES_CSV   = RESULTS_DIR / "waves.csv"

# ─── Read process_samples ─────────────────────────────────────────────
samples_by_replica = defaultdict(list)   # replica → list of dict(t, active, cpu, rss_mb, goros, sys_cpu)
if SAMPLES_CSV.exists():
    with SAMPLES_CSV.open() as fh:
        for row in csv.DictReader(fh):
            r = row["replica_id"] or "(none)"
            try:
                t = datetime.fromisoformat(row["sampled_at"].replace("Z", "+00:00"))
            except Exception:
                continue
            samples_by_replica[r].append(dict(
                t=t,
                active=int(row["active_runs"] or 0),
                queued=int(row["queued_runs"] or 0),
                cpu=float(row["loomcycle_cpu_pct_x100"] or 0) / 100.0,
                rss_mb=float(row["loomcycle_rss_bytes"] or 0) / 1024 / 1024,
                goros=int(row["loomcycle_num_goroutines"] or 0),
                sys_cpu=(float(row["system_cpu_pct_x100"]) / 100.0) if row.get("system_cpu_pct_x100") else None,
            ))
for r in samples_by_replica:
    samples_by_replica[r].sort(key=lambda s: s["t"])

# Test start = earliest sample timestamp
all_samples = sorted([s for L in samples_by_replica.values() for s in L], key=lambda s: s["t"])
T0 = all_samples[0]["t"] if all_samples else datetime.now(timezone.utc)

def elapsed_seconds(t): return (t - T0).total_seconds()

# ─── Read waves + reconstruct phase boundaries ────────────────────────
waves = []
phase_boundaries = []  # elapsed_s at which a NEW phase starts
last_phase = None
if WAVES_CSV.exists():
    with WAVES_CSV.open() as fh:
        for row in csv.DictReader(fh):
            w = dict(
                phase=int(row["phase_idx"]),
                scale=int(row["phase_scale"]),
                wave=int(row["wave_idx"]),
                t_offset_s=int(row["wave_start_epoch_offset_s"]),
                wall_s=int(row["wave_wall_s"]),
                circuits=int(row["circuits"]),
                completed=int(row["completed"]),
                failed=int(row["failed"]),
                silent=int(row["silent_regression"] or 0),
                p50=int(row["p50_ms"]) if row["p50_ms"] else None,
                p95=int(row["p95_ms"]) if row["p95_ms"] else None,
                p99=int(row["p99_ms"]) if row["p99_ms"] else None,
                max_ms=int(row["max_ms"]) if row["max_ms"] else None,
            )
            waves.append(w)
            if last_phase is None or w["phase"] != last_phase:
                phase_boundaries.append((w["t_offset_s"], w["scale"], w["phase"]))
                last_phase = w["phase"]
total_wall = max((w["t_offset_s"] + w["wall_s"] for w in waves), default=0)

REPLICA_COLORS = {
    "replica-1": "#1f77b4",
    "replica-2": "#ff7f0e",
    "replica-3": "#2ca02c",
    "replica-4": "#d62728",
    "replica-5": "#9467bd",
    "replica-6": "#8c564b",
    "(none)":    "#7f7f7f",
}

def draw_phase_boundaries(ax):
    """Vertical lines + scale labels for each phase boundary."""
    if not phase_boundaries: return
    y_top = ax.get_ylim()[1]
    for offset_s, scale, phase in phase_boundaries:
        ax.axvline(offset_s, ls="--", color="black", alpha=0.35, lw=1)
        ax.text(offset_s + 5, y_top * 0.95,
                f"phase {phase}\nx{scale}",
                fontsize=8, va="top", ha="left",
                bbox=dict(facecolor="white", edgecolor="gray", boxstyle="round,pad=0.2", alpha=0.85))

def rolling_mean(values, window):
    """Simple centered rolling mean with the same length as input."""
    if not values: return values
    out = []
    half = max(1, window // 2)
    for i in range(len(values)):
        a = max(0, i - half); b = min(len(values), i + half + 1)
        out.append(sum(values[a:b]) / (b - a))
    return out

def base_ts_chart(metric, ylabel, title, out_name, *, sum_line=False, rolling_window=15):
    """Time-series chart: raw per-replica lines (light) + rolling-mean
    overlay (strong). The rolling mean tells the envelope/trend; the
    raw spaghetti shows per-second variance. With 46 waves over 15 min,
    raw alone is unreadable; this layering keeps both."""
    fig, ax = plt.subplots(figsize=(14, 5.5))
    for r, rows in sorted(samples_by_replica.items()):
        if not rows: continue
        x = [elapsed_seconds(s["t"]) for s in rows]
        y = [s[metric] for s in rows]
        color = REPLICA_COLORS.get(r, "#444")
        # Raw per-sample line, lightened.
        ax.plot(x, y, color=color, alpha=0.25, lw=0.8)
        # Rolling-mean overlay, the actual readable signal.
        ax.plot(x, rolling_mean(y, rolling_window), color=color, alpha=1.0, lw=2.0,
                label=f"{r} ({rolling_window}-sample avg)")
    if sum_line and len(samples_by_replica) > 1:
        all_t = sorted({s["t"] for L in samples_by_replica.values() for s in L})
        sum_vals = []
        for t in all_t:
            tot = 0
            for r, rows in samples_by_replica.items():
                prev = None
                for s in rows:
                    if s["t"] <= t: prev = s
                    else: break
                if prev: tot += prev[metric]
            sum_vals.append(tot)
        xs = [elapsed_seconds(t) for t in all_t]
        ax.plot(xs, rolling_mean(sum_vals, rolling_window),
                color="black", ls="--", lw=1.6, label="cluster sum (rolling)")
    ax.set_xlabel("elapsed time (seconds)")
    ax.set_ylabel(ylabel)
    ax.set_title(title)
    ax.grid(axis="both", alpha=0.3)
    ax.legend(loc="upper right", fontsize=8, framealpha=0.9)
    if total_wall > 0:
        ax.set_xlim(0, total_wall + 5)
    draw_phase_boundaries(ax)
    fig.tight_layout()
    fig.savefig(CHARTS / out_name, dpi=130)
    plt.close(fig)

# ─── Chart 1-4: per-replica time series ───────────────────────────────
base_ts_chart("active", "active runs",
              "Active runs per replica over time", "active_runs_timeseries.png",
              sum_line=True)
base_ts_chart("cpu",    "loomcycle CPU %",
              "Per-replica loomcycle CPU % over time", "cpu_timeseries.png")
base_ts_chart("rss_mb", "RSS (MB)",
              "Per-replica RSS over time — heap-growth / leak indicator", "rss_timeseries.png")
base_ts_chart("goros",  "goroutines",
              "Per-replica goroutines over time", "goroutines_timeseries.png")

# ─── Chart 5: wave latency over time ──────────────────────────────────
def chart_wave_latency():
    if not waves: return
    fig, ax = plt.subplots(figsize=(14, 5))
    xs = [w["t_offset_s"] for w in waves]
    for metric, color, label in [("p50","#1f77b4","p50"),("p95","#ff7f0e","p95"),("p99","#d62728","p99")]:
        ys = [(w[metric] or 0)/1000 for w in waves]
        ax.plot(xs, ys, color=color, label=label, alpha=0.85, lw=1.4, marker="o", markersize=3)
    ax.set_xlabel("elapsed time (seconds)")
    ax.set_ylabel("per-circuit latency (seconds)")
    ax.set_title("Per-wave latency over the sustained run")
    ax.grid(axis="both", alpha=0.3)
    ax.legend(loc="upper right")
    if total_wall > 0:
        ax.set_xlim(0, total_wall + 5)
    draw_phase_boundaries(ax)
    fig.tight_layout()
    fig.savefig(CHARTS / "wave_latency_over_time.png", dpi=130)
    plt.close(fig)
chart_wave_latency()

# ─── Chart 6: wave completion + silent regressions ────────────────────
def chart_wave_completion():
    if not waves: return
    fig, ax1 = plt.subplots(figsize=(14, 5))
    xs = [w["t_offset_s"] for w in waves]
    pct = [100.0 * w["completed"] / w["circuits"] if w["circuits"] else 0 for w in waves]
    silent = [w["silent"] for w in waves]
    ax1.plot(xs, pct, color="#2ca02c", lw=1.6, marker="o", markersize=3, label="completion %")
    ax1.set_ylim(0, 105)
    ax1.set_xlabel("elapsed time (seconds)")
    ax1.set_ylabel("circuit completion (%)", color="#2ca02c")
    ax1.tick_params(axis="y", labelcolor="#2ca02c")
    ax1.grid(axis="both", alpha=0.3)

    ax2 = ax1.twinx()
    ax2.bar(xs, silent, width=2.0, color="#d62728", alpha=0.5, label="silent regressions")
    ax2.set_ylabel("silent regressions per wave", color="#d62728")
    ax2.tick_params(axis="y", labelcolor="#d62728")
    ax2.set_ylim(0, max(max(silent, default=0)*1.3, 5))

    ax1.set_title("Per-wave completion % + silent regressions over the sustained run")
    if total_wall > 0:
        ax1.set_xlim(0, total_wall + 5)
    draw_phase_boundaries(ax1)
    fig.tight_layout()
    fig.savefig(CHARTS / "wave_completion_over_time.png", dpi=130)
    plt.close(fig)
chart_wave_completion()

# ─── Chart 7: wave wall time ──────────────────────────────────────────
def chart_wave_wall():
    if not waves: return
    fig, ax = plt.subplots(figsize=(14, 5))
    xs = [w["t_offset_s"] for w in waves]
    walls = [w["wall_s"] for w in waves]
    scales = sorted({w["scale"] for w in waves})
    scale_color = {s: c for s,c in zip(scales, ["#1f77b4","#ff7f0e","#2ca02c","#d62728"])}
    for w in waves:
        ax.plot(w["t_offset_s"], w["wall_s"], "o",
                color=scale_color[w["scale"]], markersize=4, alpha=0.8)
    # Mean line
    if walls:
        ax.axhline(np.mean(walls), ls=":", color="gray", alpha=0.6,
                   label=f"mean wall = {np.mean(walls):.1f}s")
    ax.set_xlabel("elapsed time (seconds)")
    ax.set_ylabel("wave wall time (seconds)")
    ax.set_title("Wave wall time over the run — stable = cluster keeping up")
    ax.grid(axis="both", alpha=0.3)
    # Legend with scale → color
    from matplotlib.lines import Line2D
    legend_h = [Line2D([],[], marker="o", color=c, ls="", label=f"x{s}") for s,c in scale_color.items()]
    legend_h.append(Line2D([],[], ls=":", color="gray", label=f"mean = {np.mean(walls):.1f}s"))
    ax.legend(handles=legend_h, loc="upper right")
    if total_wall > 0:
        ax.set_xlim(0, total_wall + 5)
    draw_phase_boundaries(ax)
    fig.tight_layout()
    fig.savefig(CHARTS / "wave_wall_over_time.png", dpi=130)
    plt.close(fig)
chart_wave_wall()

# ─── Summary print ────────────────────────────────────────────────────
print(f"→ wrote {len(list(CHARTS.glob('*.png')))} PNGs to {CHARTS}")
print(f"→ {len(waves)} waves; total wall ~{total_wall}s; {len(samples_by_replica)} replicas")
if waves:
    by_phase = defaultdict(list)
    for w in waves: by_phase[w["phase"]].append(w)
    for p, ws in sorted(by_phase.items()):
        completed = sum(w["completed"] for w in ws)
        total     = sum(w["circuits"]  for w in ws)
        silent    = sum(w["silent"]    for w in ws)
        scale = ws[0]["scale"]
        p99s = [w["p99"] for w in ws if w["p99"]]
        p99_max = max(p99s) if p99s else 0
        print(f"  phase {p} (x{scale}, {len(ws):3d} waves): "
              f"{completed:6d}/{total:6d} circuits, silent={silent:3d}, p99_max={p99_max}ms")
