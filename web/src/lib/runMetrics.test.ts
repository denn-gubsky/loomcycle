import { readFileSync } from "node:fs";
import { describe, expect, it } from "vitest";
import type { EventPayload, TranscriptEvent } from "../api";
import {
  callGenerationMs,
  contextLevel,
  contextPercent,
  emptyRunMetrics,
  foldUsage,
  formatCount,
  metricsFromEvents,
  tokensPerSecond,
} from "./runMetrics";

// Produced by internal/providers/usage_event_wire_fixture_test.go from the real
// providers.Usage struct: a cloud call with prompt caching, then a local call
// that reports its decode phase and waited in a queue.
const frames: EventPayload[] = JSON.parse(
  readFileSync(new URL("./__fixtures__/usage-events.json", import.meta.url), "utf8"),
);

function row(ev: EventPayload, run_id = "", seq = 0): TranscriptEvent {
  return { seq, run_id, ts_ns: 0, type: ev.type, event: ev };
}

describe("run metrics from usage frames", () => {
  const [cloud, local] = frames;

  it("accumulates input and output but takes the context from the latest call", () => {
    const m = metricsFromEvents([row(cloud), row(local)]);
    expect(m.inputTokens).toBe(1200 + 2500);
    expect(m.outputTokens).toBe(150 + 400);
    expect(m.cacheReadTokens).toBe(800);
    expect(m.calls).toBe(2);
    // The latest call's prompt only — not a running sum, and no output.
    expect(m.contextTokens).toBe(2500);
    expect(m.maxContextTokens).toBe(32768);
  });

  it("counts a call's whole prompt as context: input plus cache read plus cache write", () => {
    // The runtime's own footprint (the one its distillation gate acts on) is
    // input + cache_read + cache_creation; the gauge must read the same number.
    const m = foldUsage(emptyRunMetrics, cloud.usage!);
    expect(m.contextTokens).toBe(1200 + 800 + 300);
  });

  it("reports the provider and model that served the latest call", () => {
    const m = metricsFromEvents([row(cloud), row(local)]);
    expect(m.provider).toBe("local");
    expect(m.model).toBe("local-model");
  });

  it("keeps the last known window when a later call reports none", () => {
    const m = foldUsage(foldUsage(emptyRunMetrics, cloud.usage!), { input_tokens: 10, output_tokens: 1 });
    expect(m.maxContextTokens).toBe(200000);
  });

  it("measures generation time from the decode phase when the server reports one", () => {
    expect(callGenerationMs(local.usage!.timing)).toBe(5000);
  });

  it("falls back to the call's time less its queueing, never to the time after the first token", () => {
    // A call that only emits a tool call has its first event at the very end,
    // so duration − ttft would be a sliver and the rate would explode.
    expect(callGenerationMs(cloud.usage!.timing)).toBe(3000);
    expect(callGenerationMs({ duration_ms: 4000, ttft_ms: 3990, queue_ms: 1000 })).toBe(3000);
    expect(callGenerationMs(undefined)).toBe(0);
    expect(callGenerationMs({ queue_ms: 5 })).toBe(0);
  });

  it("gives the latest call's rate and the run's average over timed calls", () => {
    const m = metricsFromEvents([row(cloud), row(local)]);
    expect(m.lastCallTokPerSec).toBeCloseTo(400 / 5, 6);
    expect(m.avgTokPerSec).toBeCloseTo((150 + 400) / ((3000 + 5000) / 1000), 6);
  });

  it("leaves an untimed call out of the throughput rather than counting it as instant", () => {
    const untimed: EventPayload = { type: "usage", usage: { input_tokens: 5, output_tokens: 999 } };
    const m = metricsFromEvents([row(cloud), row(untimed)]);
    expect(m.outputTokens).toBe(150 + 999);
    expect(m.avgTokPerSec).toBeCloseTo(150 / 3, 6);
    expect(m.lastCallTokPerSec).toBeCloseTo(150 / 3, 6);
  });

  it("reads only the named run's usage out of a session transcript", () => {
    const m = metricsFromEvents([row(cloud, "run_a", 1), row(local, "run_b", 2)], "run_a");
    expect(m.calls).toBe(1);
    expect(m.contextTokens).toBe(2300);
    expect(m.model).toBe("cloud-model");
  });

  it("ignores frames that are not usage", () => {
    const m = metricsFromEvents([row({ type: "text", text: "hi" }), row({ type: "done", usage: cloud.usage })]);
    expect(m.calls).toBe(0);
  });
});

describe("tokensPerSecond", () => {
  it("is zero for an empty or negative window", () => {
    expect(tokensPerSecond(100, 0)).toBe(0);
    expect(tokensPerSecond(100, -5)).toBe(0);
    expect(tokensPerSecond(100, 2000)).toBe(50);
  });
});

describe("contextPercent and contextLevel", () => {
  it("is null without a window and capped at 100", () => {
    expect(contextPercent({ ...emptyRunMetrics, contextTokens: 10 })).toBeNull();
    expect(contextPercent({ ...emptyRunMetrics, contextTokens: 50, maxContextTokens: 200 })).toBe(25);
    expect(contextPercent({ ...emptyRunMetrics, contextTokens: 500, maxContextTokens: 200 })).toBe(100);
  });

  it("warns above 80% and alarms from 95%", () => {
    expect(contextLevel(80)).toBe("ok");
    expect(contextLevel(80.5)).toBe("warn");
    expect(contextLevel(95)).toBe("full");
  });
});

describe("formatCount", () => {
  it("compacts counts the way the loomboard HUD does", () => {
    expect(formatCount(942)).toBe("942");
    expect(formatCount(1234)).toBe("1.2k");
    expect(formatCount(48000)).toBe("48k");
    expect(formatCount(1_300_000)).toBe("1.3M");
  });
});

describe("the streaming estimate of a call in flight", () => {
  // Rows at explicit times (ms) so the window is exact.
  const at = (ms: number, ev: EventPayload, seq = 0): TranscriptEvent => ({
    seq,
    run_id: "",
    ts_ns: ms * 1_000_000,
    type: ev.type,
    event: ev,
  });
  const text = (ms: number, s: string) => at(ms, { type: "text", text: s });
  const chars = (n: number) => "x".repeat(n);

  it("rises as text streams, measured from the call's start rather than its first character", () => {
    // The call starts at 0; prefill until 1000; 400 chars (≈100 tokens) by 2000.
    const early = metricsFromEvents([at(0, { type: "started" }), text(1000, chars(200))]);
    const later = metricsFromEvents([at(0, { type: "started" }), text(1000, chars(200)), text(2000, chars(200))]);
    expect(early.streamingTokPerSec).toBeCloseTo(50 / 1, 6);
    expect(later.streamingTokPerSec).toBeCloseTo(100 / 2, 6);
    const faster = metricsFromEvents([at(0, { type: "started" }), text(1000, chars(200)), text(2000, chars(600))]);
    expect(faster.streamingTokPerSec).toBeGreaterThan(later.streamingTokPerSec);
  });

  it("is replaced by the call's own timed figure when its usage arrives", () => {
    const [cloud] = frames;
    const m = metricsFromEvents([at(0, { type: "started" }), text(1000, chars(4000)), at(3000, cloud)]);
    expect(m.streamingTokPerSec).toBe(0);
    expect(m.lastCallTokPerSec).toBeCloseTo(150 / 3, 6);
  });

  it("starts over for the next call, from the tool results that began it", () => {
    const [cloud] = frames;
    const m = metricsFromEvents([
      at(0, { type: "started" }),
      text(1000, chars(4000)),
      at(3000, cloud),
      at(5000, { type: "tool_result", text: "ok", tool_use: { id: "t1", name: "Read", input: {} } }),
      text(6000, chars(400)),
    ]);
    // 100 tokens over the 1 s since the tool result — nothing from the first call.
    expect(m.streamingTokPerSec).toBeCloseTo(100, 6);
  });

  it("counts streamed thinking as output, and not a whole tool call", () => {
    const m = metricsFromEvents([
      at(0, { type: "started" }),
      at(1000, { type: "thinking", text: chars(400) }),
      at(1500, { type: "tool_call", tool_use: { id: "t", name: "Read", input: { path: chars(4000) } } }),
      text(2000, chars(400)),
    ]);
    expect(m.streamingTokPerSec).toBeCloseTo(200 / 2, 6);
  });

  it("is zero for a call that has streamed no text", () => {
    const m = metricsFromEvents([
      at(0, { type: "started" }),
      at(1500, { type: "tool_call", tool_use: { id: "t", name: "Read", input: {} } }),
    ]);
    expect(m.streamingTokPerSec).toBe(0);
  });

  it("does not restart the call on a steer that lands mid-stream", () => {
    const m = metricsFromEvents([
      at(0, { type: "started" }),
      text(1000, chars(200)),
      at(1500, { type: "steer", user_input: { text: "also check X" } }),
      text(2000, chars(200)),
    ]);
    expect(m.streamingTokPerSec).toBeCloseTo(100 / 2, 6);
  });

  it("says nothing over a window too short to mean anything (a replay lands at once)", () => {
    const m = metricsFromEvents([at(0, { type: "started" }), text(5, chars(4000))]);
    expect(m.streamingTokPerSec).toBe(0);
  });
});
