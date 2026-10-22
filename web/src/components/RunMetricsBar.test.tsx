import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";
import RunMetricsBar, { ContextMeter, RunRate } from "./RunMetricsBar";
import { emptyRunMetrics, type RunMetrics } from "../lib/runMetrics";

const twoCalls: RunMetrics = {
  ...emptyRunMetrics,
  inputTokens: 3700,
  outputTokens: 550,
  cacheReadTokens: 800,
  contextTokens: 28000,
  maxContextTokens: 32768,
  provider: "local",
  model: "local-model",
  calls: 2,
  timedOutputTokens: 550,
  generationMs: 8000,
  lastCallTokPerSec: 80,
  avgTokPerSec: 68.75,
};

describe("ContextMeter", () => {
  it("draws the used share of the window and flags it above 80%", () => {
    const html = renderToStaticMarkup(createElement(ContextMeter, { used: 28000, max: 32768 }));
    expect(html).toContain("ctx 28k / 33k (85%)");
    expect(html).toContain('data-level="warn"');
    expect(html).toContain("width:85");
  });

  it("shows the window before anything has been measured, with an empty bar", () => {
    const html = renderToStaticMarkup(createElement(ContextMeter, { used: 0, max: 200000 }));
    expect(html).toContain("ctx — / 200k");
    expect(html).toContain("width:0%");
  });

  it("shows only the size, and no bar, when the window is unknown", () => {
    const html = renderToStaticMarkup(createElement(ContextMeter, { used: 1500, max: 0 }));
    expect(html).toContain("ctx 1.5k");
    expect(html).not.toContain("ctx-bar");
  });

  it("renders nothing when neither is known", () => {
    expect(renderToStaticMarkup(createElement(ContextMeter, { used: 0, max: 0 }))).toBe("");
  });
});

describe("RunRate", () => {
  it("labels the run average as one", () => {
    const html = renderToStaticMarkup(createElement(RunRate, { metrics: twoCalls, mode: "average" }));
    expect(html).toContain("68.8 tok/s avg");
  });

  it("shows the latest call's rate while live", () => {
    const html = renderToStaticMarkup(createElement(RunRate, { metrics: twoCalls, mode: "latest" }));
    expect(html).toContain("80.0 tok/s");
    expect(html).not.toContain("avg");
  });

  it("renders nothing without a timed call", () => {
    expect(renderToStaticMarkup(createElement(RunRate, { metrics: emptyRunMetrics, mode: "average" }))).toBe("");
  });
});

describe("RunMetricsBar", () => {
  it("shows the serving provider/model, the totals, the rate and the context", () => {
    const html = renderToStaticMarkup(createElement(RunMetricsBar, { metrics: twoCalls, running: true }));
    expect(html).toContain("local/local-model");
    expect(html).toContain("▲ 3.7k");
    expect(html).toContain("▼ 550");
    expect(html).toContain("80.0 tok/s");
    expect(html).toContain("ctx 28k / 33k");
  });

  it("falls back to a window known before the first call", () => {
    const html = renderToStaticMarkup(
      createElement(RunMetricsBar, { metrics: emptyRunMetrics, running: true, knownWindow: 200000 }),
    );
    expect(html).toContain("ctx — / 200k");
    expect(html).not.toContain("▲");
  });

  it("prefers the window the run reported over the one known beforehand", () => {
    const html = renderToStaticMarkup(
      createElement(RunMetricsBar, { metrics: twoCalls, running: false, knownWindow: 200000 }),
    );
    expect(html).toContain("/ 33k");
    expect(html).toContain("68.8 tok/s avg");
  });

  it("renders nothing for a run with no usage and no known window", () => {
    expect(
      renderToStaticMarkup(createElement(RunMetricsBar, { metrics: emptyRunMetrics, running: true })),
    ).toBe("");
  });
});
