import {
  contextLevel,
  formatCount,
  formatRate,
  type RunMetrics,
} from "../lib/runMetrics";

// RunMetricsBar is the live run pane's metrics strip: the provider/model that
// served the latest call (so a provider fallback shows), cumulative ▲ input /
// ▼ output, tokens per second, and the context gauge. Everything comes from
// the run's usage frames (lib/runMetrics); nothing renders until there is
// something to say.
export default function RunMetricsBar({
  metrics,
  running,
  knownWindow,
}: {
  metrics: RunMetrics;
  running: boolean;
  // The window a run of this agent starts with, when the caller knows it up
  // front (the agent library reports it) — so the gauge has a ceiling before
  // the first call reports one. The run's own report wins once it arrives.
  knownWindow?: number;
}) {
  const ctxMax = metrics.maxContextTokens || knownWindow || 0;
  const serving = [metrics.provider, metrics.model].filter(Boolean).join("/");
  if (metrics.calls === 0 && ctxMax <= 0) return null;
  return (
    <div className="run-metrics">
      {serving && (
        <span className="run-metrics-item run-metrics-model" title="Provider/model that served the latest call">
          {serving}
        </span>
      )}
      {metrics.calls > 0 && (
        <>
          <span
            className="run-metrics-item"
            title={`Input tokens, summed over ${metrics.calls} call(s)${
              metrics.cacheReadTokens ? ` — plus ${metrics.cacheReadTokens} read from the prompt cache` : ""
            }`}
          >
            ▲ {formatCount(metrics.inputTokens)}
          </span>
          <span className="run-metrics-item" title="Output tokens, summed over the run">
            ▼ {formatCount(metrics.outputTokens)}
          </span>
        </>
      )}
      <RunRate metrics={metrics} mode={running ? "latest" : "average"} />
      <ContextMeter used={metrics.contextTokens} max={ctxMax} />
    </div>
  );
}

// RunRate renders output tokens per second, measured from the server's own
// per-call timing. "latest" is the most recent call's rate (what the model is
// doing now — the live view); "average" is the run's, over every timed call,
// and says so. Nothing renders until a call has been timed.
export function RunRate({ metrics, mode }: { metrics: RunMetrics; mode: "latest" | "average" }) {
  const rate = mode === "latest" ? metrics.lastCallTokPerSec : metrics.avgTokPerSec;
  if (rate <= 0) return null;
  const avg = `${formatRate(metrics.avgTokPerSec)} tok/s over ${metrics.timedOutputTokens} output tokens in ${(
    metrics.generationMs / 1000
  ).toFixed(1)}s of generation`;
  return (
    <span
      className={mode === "latest" ? "run-metrics-item run-metrics-rate live" : "run-metrics-item run-metrics-rate"}
      title={
        mode === "latest"
          ? `Output tokens per second of the latest model call. Run average: ${avg}.`
          : `Average output tokens per second over the run: ${avg}.`
      }
    >
      {formatRate(rate)} tok/s{mode === "average" ? " avg" : ""}
    </span>
  );
}

// ContextMeter renders how much of the context window the latest call's
// prompt filled: a thin bar + "ctx used / window (pct%)", amber above 80% and
// red from 95%. Before any call has been measured it shows the window alone
// ("ctx — / 200k"); with no known window it shows only the size, with no bar —
// a window of 0 means unknown, not empty.
export function ContextMeter({ used, max }: { used: number; max: number }) {
  if (max <= 0) {
    if (used <= 0) return null;
    return (
      <span className="ctx-gauge" title={`${used} tokens in the latest prompt (window unknown)`}>
        ctx {formatCount(used)}
      </span>
    );
  }
  const pct = Math.min(100, Math.round((used / max) * 100));
  const level = contextLevel((used / max) * 100);
  return (
    <span
      className="ctx-gauge"
      title={
        used > 0
          ? `the latest prompt filled ${used} of a ${max}-token context window`
          : `${max}-token context window — nothing measured yet`
      }
    >
      <span className="ctx-bar">
        <span className="ctx-bar-fill" data-level={level} style={{ width: `${used > 0 ? pct : 0}%` }} />
      </span>
      {used > 0
        ? `ctx ${formatCount(used)} / ${formatCount(max)} (${pct}%)`
        : `ctx — / ${formatCount(max)}`}
    </span>
  );
}
