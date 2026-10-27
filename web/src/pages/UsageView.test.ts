import { describe, expect, it } from "vitest";
import type { UsageAggregate } from "../api";
import { DIMENSIONS, dimValue, fmtAudioMinutes, totalAudioSeconds } from "./UsageView";

const row = (over: Partial<UsageAggregate>): UsageAggregate => ({
  input_tokens: 0,
  output_tokens: 0,
  cache_creation_tokens: 0,
  cache_read_tokens: 0,
  cost: 0,
  call_count: 1,
  unpriced_calls: 0,
  ...over,
});

describe("UsageView purpose", () => {
  it("offers purpose as a grouping, last in the server's canonical order", () => {
    expect(DIMENSIONS.map((d) => d.key)).toEqual(["tenant", "user", "provider", "model", "source", "purpose"]);
  });

  it("shows a row's purpose, and a dash when the report is not grouped by it", () => {
    expect(dimValue(row({ purpose: "ocr" }), "purpose")).toBe("ocr");
    expect(dimValue(row({}), "purpose")).toBe("—");
  });

  it("sums audio only from speech-to-text rows and renders it in minutes", () => {
    const rows = [row({ purpose: "chat" }), row({ purpose: "stt", audio_seconds: 60 }), row({ purpose: "stt", audio_seconds: 30 })];
    expect(totalAudioSeconds(rows)).toBe(90);
    expect(fmtAudioMinutes(90)).toBe("1.5 min");
    expect(totalAudioSeconds([row({ purpose: "ocr" })])).toBe(0);
  });
});
