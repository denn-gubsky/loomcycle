/**
 * usageReport() → GET /v1/_usage: grouping by purpose reaches the query string,
 * and a row's purpose and audio_seconds come back typed. The type half is
 * guarded by `npm run typecheck:tests` (vitest strips types).
 */

import { describe, it, expect } from "vitest";
import { makeClient, jsonResponse } from "./helpers.js";
import type { UsageAggregate, UsageBucket, UsageDimension } from "../src/index.js";

describe("usageReport", () => {
  it("groups by purpose and returns purpose + audio_seconds", async () => {
    const rows: UsageAggregate[] = [
      { purpose: "ocr", input_tokens: 300, output_tokens: 0, cache_creation_tokens: 0, cache_read_tokens: 0,
        cost: 0.3, currency: "USD", call_count: 1, unpriced_calls: 0 },
      { purpose: "stt", input_tokens: 0, output_tokens: 0, cache_creation_tokens: 0, cache_read_tokens: 0,
        cost: 0.5, currency: "USD", call_count: 1, unpriced_calls: 0, audio_seconds: 75 },
    ];
    const { client, fetchMock } = makeClient([jsonResponse({ group_by: ["tenant", "purpose"], rows })]);
    const groupBy: UsageDimension[] = ["tenant", "purpose"];

    const res = await client.usageReport({ groupBy });

    const url = new URL(fetchMock.mock.calls[0]![0] as string);
    expect(url.pathname).toBe("/v1/_usage");
    expect(url.searchParams.get("group_by")).toBe("tenant,purpose");
    const stt = res.rows.find((r) => r.purpose === "stt");
    expect(stt?.audio_seconds).toBe(75);
    expect(res.rows.find((r) => r.purpose === "ocr")?.audio_seconds).toBeUndefined();
  });
});

describe("usageReport chart parameters", () => {
  it("sends bucket, top, top_by and filters, and reads period, other and detail_since", async () => {
    const rows: UsageAggregate[] = [
      { model: "big", period: "2026-10-01T00:00:00Z", input_tokens: 9, output_tokens: 0, cache_creation_tokens: 0,
        cache_read_tokens: 0, cost: 9, currency: "USD", call_count: 1, unpriced_calls: 0 },
      { other: true, period: "2026-10-01T00:00:00Z", input_tokens: 4, output_tokens: 0, cache_creation_tokens: 0,
        cache_read_tokens: 0, cost: 4, currency: "USD", call_count: 2, unpriced_calls: 0, input_chars: 40 },
    ];
    const { client, fetchMock } = makeClient([jsonResponse({
      group_by: ["model"], bucket: "day", rows, detail_since: "2026-09-28T00:00:00Z",
    })]);
    const bucket: UsageBucket = "day";

    const res = await client.usageReport({
      groupBy: ["model"], bucket, top: 1, topBy: "tokens",
      filters: { class: ["background"], tier: ["high", "middle"], user: [] },
    });

    const url = new URL(fetchMock.mock.calls[0]![0] as string);
    expect(url.searchParams.get("bucket")).toBe("day");
    expect(url.searchParams.get("top")).toBe("1");
    expect(url.searchParams.get("top_by")).toBe("tokens");
    expect(url.searchParams.get("class")).toBe("background");
    expect(url.searchParams.get("tier")).toBe("high,middle");
    expect(url.searchParams.has("user")).toBe(false);
    expect(res.detail_since).toBe("2026-09-28T00:00:00Z");
    expect(res.rows[1]?.other).toBe(true);
    expect(res.rows[1]?.input_chars).toBe(40);
  });
});
