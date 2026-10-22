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
  if (metrics.calls === 0 && ctxMax <= 0 && metrics.streamingTokPerSec <= 0) return null;
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
      <RunRate metrics={metrics} mode={running ? "live" : "average"} />
      <ContextMeter used={metrics.contextTokens} max={ctxMax} />
    </div>
  );
}

// RunRate renders output tokens per second. "live" is the live pane while the
// run is going: a call still streaming shows a provisional "~N tok/s" estimate
// from its streamed text (lib/runMetrics streamingEstimate), and otherwise the
// latest call's server-timed rate, which replaces the estimate the moment that
// call's usage frame arrives. "latest" is the latest timed call only; "average"
// is the run's, over every timed call, and says so.
export function RunRate({ metrics, mode }: { metrics: RunMetrics; mode: "live" | "latest" | "average" }) {
  const avg = `${formatRate(metrics.avgTokPerSec)} tok/s over ${metrics.timedOutputTokens} output tokens in ${(
    metrics.generationMs / 1000
  ).toFixed(1)}s of generation`;
  if (mode === "live" && metrics.streamingTokPerSec > 0) {
    return (
      <span
        className="run-metrics-item run-metrics-rate live provisional"
        title={`An estimate for the call still streaming (its text at about 4 characters a token, since the call started); the measured rate replaces it when the call finishes.${
          metrics.timedOutputTokens > 0 ? ` Run average so far: ${avg}.` : ""
        }`}
      >
        ~{Math.round(metrics.streamingTokPerSec)} tok/s
      </span>
    );
  }
  const rate = mode === "average" ? metrics.avgTokPerSec : metrics.lastCallTokPerSec;
  if (rate <= 0) return null;
  return (
    <span
      className={mode === "average" ? "run-metrics-item run-metrics-rate" : "run-metrics-item run-metrics-rate live"}
      title={
        mode === "average"
          ? `Average output tokens per second over the run: ${avg}.`
          : `Output tokens per second of the latest model call. Run average: ${avg}.`
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
