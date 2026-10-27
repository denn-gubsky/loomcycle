/**
 * usageReport() → GET /v1/_usage: grouping by purpose reaches the query string,
 * and a row's purpose and audio_seconds come back typed. The type half is
 * guarded by `npm run typecheck:tests` (vitest strips types).
 */

import { describe, it, expect } from "vitest";
import { makeClient, jsonResponse } from "./helpers.js";
import type { UsageAggregate, UsageDimension } from "../src/index.js";

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
