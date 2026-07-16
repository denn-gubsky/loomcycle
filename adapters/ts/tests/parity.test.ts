/**
 * Tests for the RFC Y fan-out + compaction client surface:
 *   - spawnRunBatch()  → POST /v1/runs:batch
 *   - compactRun()     → POST /v1/runs/{run_id}/compact
 *   - per-run sampling / compaction on runStreaming + continueSession bodies.
 */

import { describe, it, expect } from "vitest";
import { makeClient, jsonResponse, sseResponse } from "./helpers.js";

describe("spawnRunBatch", () => {
  it("POSTs /v1/runs:batch with per-spawn snake_case bodies + returns the envelope", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({
        spawned: 2,
        results: [
          { agent_id: "a0", run_id: "r0", session_id: "s0", status: "completed", final_text: "zero" },
          { agent_id: "a1", run_id: "r1", session_id: "s1", status: "failed", error: "boom" },
        ],
      }),
    ]);

    const res = await client.spawnRunBatch({
      mode: "join",
      timeoutMs: 5000,
      spawns: [
        {
          agent: "rev",
          segments: [{ role: "user", content: [{ type: "trusted-text", text: "a" }] }],
          compaction: { enabled: true, keepLastN: 8, autocompactAtPct: 75 },
        },
        {
          agent: "rev",
          segments: [{ role: "user", content: [{ type: "trusted-text", text: "b" }] }],
          sampling: { temperature: 0.2, topK: 40 },
        },
      ],
    });

    expect(res.spawned).toBe(2);
    expect(res.results[1]!.error).toBe("boom");

    const call = fetchMock.mock.calls[0]!;
    expect(call[0]).toBe("http://test-loomcycle:8787/v1/runs:batch");
    expect(call[1]!.method).toBe("POST");
    const body = JSON.parse(call[1]!.body as string);
    expect(body.mode).toBe("join");
    expect(body.timeout_ms).toBe(5000);
    expect(body.spawns).toHaveLength(2);
    // Per-spawn camelCase → snake_case mapping, including nested compaction/sampling.
    expect(body.spawns[0]).toEqual({
      agent: "rev",
      segments: [{ role: "user", content: [{ type: "trusted-text", text: "a" }] }],
      compaction: { enabled: true, keep_last_n: 8, autocompact_at_pct: 75 },
    });
    expect(body.spawns[1].sampling).toEqual({ temperature: 0.2, top_k: 40 });
  });

  it("surfaces a server 400 (over-cap) as a thrown error", async () => {
    const { client } = makeClient([
      jsonResponse({ error: "33 spawns exceeds the per-batch cap of 32" }, 400),
    ]);
    await expect(
      client.spawnRunBatch({ spawns: [{ agent: "x", segments: [] }] }),
    ).rejects.toThrow();
  });
});

describe("compactRun", () => {
  it("POSTs /v1/runs/{run_id}/compact and returns the result", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({
        run_id: "r_x",
        compacted: true,
        before_tokens: 900,
        after_tokens: 120,
        applied: "live",
      }),
    ]);

    const res = await client.compactRun("r_x", { reason: "freeing context" });
    expect(res.compacted).toBe(true);
    expect(res.applied).toBe("live");
    expect(res.after_tokens).toBe(120);

    const call = fetchMock.mock.calls[0]!;
    expect(call[0]).toBe("http://test-loomcycle:8787/v1/runs/r_x/compact");
    expect(call[1]!.method).toBe("POST");
    expect(JSON.parse(call[1]!.body as string)).toEqual({ reason: "freeing context" });
  });

  it("url-encodes the run_id", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({ run_id: "a/b", compacted: false, before_tokens: 0, after_tokens: 0, applied: "noop" }),
    ]);
    await client.compactRun("a/b");
    expect(fetchMock.mock.calls[0]![0]).toBe("http://test-loomcycle:8787/v1/runs/a%2Fb/compact");
  });
});

describe("cancelTurn (RFC BH)", () => {
  it("POSTs /v1/runs/{run_id}/cancel with the reason + returns the result", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({ run_id: "run-1", stopped: true, parked: true }),
    ]);

    const res = await client.cancelTurn("run-1", { reason: "too slow" });
    expect(res.stopped).toBe(true);
    expect(res.parked).toBe(true);
    expect(res.run_id).toBe("run-1");

    const call = fetchMock.mock.calls[0]!;
    expect(call[0]).toBe("http://test-loomcycle:8787/v1/runs/run-1/cancel");
    expect(call[1]!.method).toBe("POST");
    expect(JSON.parse(call[1]!.body as string)).toEqual({ reason: "too slow" });
  });

  it("omits the reason (empty body) when not given + url-encodes the run_id", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({ run_id: "a/b", stopped: true, parked: true }),
    ]);
    await client.cancelTurn("a/b");
    const call = fetchMock.mock.calls[0]!;
    expect(call[0]).toBe("http://test-loomcycle:8787/v1/runs/a%2Fb/cancel");
    expect(JSON.parse(call[1]!.body as string)).toEqual({});
  });
});

describe("resolveInterrupt / cancelInterrupt (RFC BH decline)", () => {
  it("resolveInterrupt answer path sends kind + answer + resolved_by, no disposition", async () => {
    const { client, fetchMock } = makeClient([jsonResponse({ status: "resolved" })]);
    await client.resolveInterrupt("run-1", "intr-1", { answer: "Yes" });
    const call = fetchMock.mock.calls[0]!;
    expect(call[0]).toBe(
      "http://test-loomcycle:8787/v1/runs/run-1/interrupts/intr-1/resolve",
    );
    expect(JSON.parse(call[1]!.body as string)).toEqual({
      kind: "question",
      resolved_by: "client",
      answer: "Yes",
    });
  });

  it("resolveInterrupt threads an explicit disposition", async () => {
    const { client, fetchMock } = makeClient([jsonResponse({ status: "declined" })]);
    await client.resolveInterrupt("run-1", "intr-1", { disposition: "declined" });
    const body = JSON.parse(fetchMock.mock.calls[0]![1]!.body as string);
    expect(body.disposition).toBe("declined");
    expect("answer" in body).toBe(false);
  });

  it("cancelInterrupt POSTs a decline with NO answer", async () => {
    const { client, fetchMock } = makeClient([jsonResponse({ status: "declined" })]);
    await client.cancelInterrupt("run-1", "intr-1");
    const call = fetchMock.mock.calls[0]!;
    expect(call[0]).toBe(
      "http://test-loomcycle:8787/v1/runs/run-1/interrupts/intr-1/resolve",
    );
    expect(call[1]!.method).toBe("POST");
    const body = JSON.parse(call[1]!.body as string);
    expect(body.disposition).toBe("declined");
    expect("answer" in body).toBe(false);
  });
});

describe("per-run sampling + compaction on the run body", () => {
  it("runStreaming maps sampling + compaction into the snake_case body", async () => {
    const { client, fetchMock } = makeClient([
      sseResponse(['event: done\ndata: {"type":"done","stop_reason":"end_turn"}\n\n']),
    ]);
    for await (const _ of client.runStreaming({
      agent: "qa",
      segments: [{ role: "user", content: [{ type: "trusted-text", text: "hi" }] }],
      sampling: { temperature: 0, topP: 0.9 },
      compaction: { enabled: true, targetPercentage: 20 },
    })) {
      void _;
    }
    const body = JSON.parse(fetchMock.mock.calls[0]![1]!.body as string);
    // temperature 0 must survive (deterministic, not dropped as falsy).
    expect(body.sampling).toEqual({ temperature: 0, top_p: 0.9 });
    expect(body.compaction).toEqual({ enabled: true, target_percentage: 20 });
  });

  it("continueSession maps sampling + compaction too", async () => {
    const { client, fetchMock } = makeClient([
      sseResponse(['event: done\ndata: {"type":"done","stop_reason":"end_turn"}\n\n']),
    ]);
    for await (const _ of client.continueSession({
      sessionId: "s1",
      segments: [{ role: "user", content: [{ type: "trusted-text", text: "more" }] }],
      compaction: { keepFirst: false, model: "haiku" },
    })) {
      void _;
    }
    const body = JSON.parse(fetchMock.mock.calls[0]![1]!.body as string);
    expect(body.compaction).toEqual({ keep_first: false, model: "haiku" });
  });
});
