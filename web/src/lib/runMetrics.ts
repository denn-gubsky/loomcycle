// runMetrics — token accounting for a run's metrics bar (the live terminal and
// the run page header): cumulative input/output, the context the latest call
// filled, the window it filled it against, and output tokens per second.
//
// Pure functions over usage frames, which the server emits once per model call
// (live on the SSE stream, and persisted in the transcript), so the live pane
// and a finished run's page compute the same numbers from the same data.

import type { CallTiming, EventPayload, TranscriptEvent } from "../api";

export type UsageFrame = NonNullable<EventPayload["usage"]>;

export interface RunMetrics {
  /** Cumulative uncached prompt tokens (▲) — input_tokens summed over calls. */
  inputTokens: number;
  /** Cumulative generated tokens (▼). */
  outputTokens: number;
  /** Cumulative prompt tokens served from the provider's cache. */
  cacheReadTokens: number;
  /** The latest call's prompt footprint — see foldUsage. */
  contextTokens: number;
  /** The latest reported context window; 0 = unknown. */
  maxContextTokens: number;
  /** Provider and model that served the latest call ("" until one reports). */
  provider: string;
  model: string;
  /** Model calls folded in. */
  calls: number;
  /** Output tokens and generation time over the calls that carry timing. */
  timedOutputTokens: number;
  generationMs: number;
  /** Output rate of the latest timed call, and the average over timed calls. */
  lastCallTokPerSec: number;
  avgTokPerSec: number;
  /** A PROVISIONAL rate for the call still streaming — see streamingEstimate.
   *  0 when no call is in flight, it has streamed nothing yet, or its window
   *  is too short to mean anything. */
  streamingTokPerSec: number;
}

export const emptyRunMetrics: RunMetrics = {
  inputTokens: 0,
  outputTokens: 0,
  cacheReadTokens: 0,
  contextTokens: 0,
  maxContextTokens: 0,
  provider: "",
  model: "",
  calls: 0,
  timedOutputTokens: 0,
  generationMs: 0,
  lastCallTokPerSec: 0,
  avgTokPerSec: 0,
  streamingTokPerSec: 0,
};

/** Fold one call's usage into the run's totals.
 *
 *  contextTokens is the LATEST call's prompt (it already carries every prior
 *  turn, so the newest prompt is the footprint "right now" — and it drops after
 *  a compaction, as it should): input + cache_read + cache_creation. That is
 *  the exact sum the runtime keeps as the footprint its distillation gate acts
 *  on (recordTurn in internal/loop), so the gauge reads the number the runtime
 *  believes. cache_creation counts because a prefix being written to the cache
 *  is still prompt the model read; it is reported apart from input_tokens.
 *  Output is never added: it is the answer, not context the prompt consumed. */
export function foldUsage(m: RunMetrics, u: UsageFrame): RunMetrics {
  const input = u.input_tokens ?? 0;
  const output = u.output_tokens ?? 0;
  const cacheRead = u.cache_read_input_tokens ?? 0;
  const cacheWrite = u.cache_creation_input_tokens ?? 0;
  const genMs = callGenerationMs(u.timing);
  const timed = genMs > 0;
  const timedOutputTokens = m.timedOutputTokens + (timed ? output : 0);
  const generationMs = m.generationMs + (timed ? genMs : 0);
  return {
    inputTokens: m.inputTokens + input,
    outputTokens: m.outputTokens + output,
    cacheReadTokens: m.cacheReadTokens + cacheRead,
    contextTokens: input + cacheRead + cacheWrite,
    maxContextTokens: u.max_context_tokens || m.maxContextTokens,
    provider: u.provider || m.provider,
    model: u.model || m.model,
    calls: m.calls + 1,
    timedOutputTokens,
    generationMs,
    lastCallTokPerSec: timed ? tokensPerSecond(output, genMs) : m.lastCallTokPerSec,
    avgTokPerSec: tokensPerSecond(timedOutputTokens, generationMs),
    // The call this usage closes is no longer streaming; its timed figure
    // above replaces whatever was estimated for it.
    streamingTokPerSec: 0,
  };
}

/** The metrics of the usage frames in a transcript. With runId, only that
 *  run's rows count — a session transcript holds every run of the session.
 *  Live rows carry no run id, so a live pane passes none. */
export function metricsFromEvents(events: readonly TranscriptEvent[], runId?: string): RunMetrics {
  let m = emptyRunMetrics;
  const rows = runId === undefined ? events : events.filter((e) => e.run_id === runId);
  for (const e of rows) {
    const ev = e.event;
    if (ev?.type === "usage" && ev.usage) m = foldUsage(m, ev.usage);
  }
  return { ...m, streamingTokPerSec: streamingEstimate(rows) };
}

// Events that come before a model call starts. No event marks the start of a
// call itself — `started` is sent once per run, before the first — so the call
// is taken to start at the LAST of these seen before it streams anything: the
// run starting, an operator message (the client's own echo, or a steer), or a
// tool result (the round that the next call answers). Queueing for a slot and
// prefill therefore count, as they do in loomboard's estimate. A row of this
// kind arriving after the call has begun streaming — a steer is drained only at
// the next boundary — does not restart it.
const CALL_START_EVENTS = new Set(["started", "user_echo", "steer", "tool_result"]);

// Shorter windows than this give no estimate. A re-attach replays a call's
// deltas in one burst, all stamped on arrival, and a rate over that sliver of
// time would read as thousands of tokens a second.
export const MIN_ESTIMATE_WINDOW_MS = 500;

/** A provisional output rate for the call still in flight, before its usage
 *  frame brings the server-timed figure: streamed characters / 4 (the same
 *  chars-per-token rule loomcycle's own estimates use) over the time since the
 *  call started, as of the latest delta. Rows carry their arrival time.
 *
 *  Thinking deltas count: a reasoning model's thinking is billed as output and
 *  is most of what it generates on a hard turn, so leaving it out would read as
 *  a stall. A tool call does not: it arrives whole at the end of the call, and
 *  counting it would spike the rate at the last moment. If no start event was
 *  seen, the call is taken to start at its first delta.
 *
 *  0 when nothing is streaming: a `usage` frame closes the call. */
export function streamingEstimate(rows: readonly TranscriptEvent[]): number {
  let startNs: number | null = null;
  let lastNs = 0;
  let chars = 0;
  for (const e of rows) {
    const type = e.event?.type ?? e.type;
    if (type === "usage") {
      startNs = null;
      lastNs = 0;
      chars = 0;
      continue;
    }
    if (CALL_START_EVENTS.has(type)) {
      if (chars === 0) startNs = e.ts_ns;
      continue;
    }
    if ((type === "text" || type === "thinking") && e.event?.text) {
      if (startNs === null) startNs = e.ts_ns;
      chars += e.event.text.length;
      lastNs = e.ts_ns;
    }
  }
  if (chars === 0 || startNs === null) return 0;
  const elapsedMs = (lastNs - startNs) / 1_000_000;
  if (elapsedMs < MIN_ESTIMATE_WINDOW_MS) return 0;
  return tokensPerSecond(chars / 4, elapsedMs);
}

/** How long one call spent generating, in ms, from the server's own timing;
 *  0 = not measured. The decode phase when the server reports it (Ollama).
 *  Otherwise the call's duration less its queueing — prefill included, so it
 *  under-reads a little, but never time-after-first-token: a call that only
 *  emits a tool call has its first event at the very end, and that sliver of a
 *  window would make the rate explode. */
export function callGenerationMs(t: CallTiming | undefined): number {
  if (!t) return 0;
  if ((t.decode_ms ?? 0) > 0) return t.decode_ms!;
  const ms = (t.duration_ms ?? 0) - (t.queue_ms ?? 0);
  return ms > 0 ? ms : 0;
}

/** Throughput in tokens/second; an empty or negative window yields 0. */
export function tokensPerSecond(outputTokens: number, elapsedMs: number): number {
  if (elapsedMs <= 0) return 0;
  return (outputTokens / elapsedMs) * 1000;
}

/** Context used as 0..100, or null when no window is known (nothing to draw). */
export function contextPercent(m: Pick<RunMetrics, "contextTokens" | "maxContextTokens">): number | null {
  if (!m.maxContextTokens) return null;
  return Math.min(100, (m.contextTokens / m.maxContextTokens) * 100);
}

/** The gauge's colour band: warn above 80%, full from 95%. */
export function contextLevel(pct: number): "ok" | "warn" | "full" {
  if (pct >= 95) return "full";
  if (pct > 80) return "warn";
  return "ok";
}

/** Compact a token count: 942, 1.2k, 48k, 1.3M. */
export function formatCount(n: number): string {
  if (n < 1000) return String(n);
  if (n < 1_000_000) return (n / 1000).toFixed(n < 10_000 ? 1 : 0) + "k";
  return (n / 1_000_000).toFixed(1) + "M";
}

/** A tokens-per-second figure for display: one decimal below 100, whole above. */
export function formatRate(r: number): string {
  return r < 100 ? r.toFixed(1) : String(Math.round(r));
}
