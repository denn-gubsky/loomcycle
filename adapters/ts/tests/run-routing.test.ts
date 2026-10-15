import { describe, expect, it } from "vitest";
import { jsonResponse, makeClient, sseResponse } from "./helpers.js";

// A run's own `routing:` block. It is sent under the same keys the option
// uses (queue_aware / switch_after_ms) on every path that starts a run, and an
// empty block is sent as `{}` — the run turning queue-aware routing off — never
// dropped as if it were unset.

const done = () => sseResponse(['event: done\ndata: {"type":"done"}\n\n']);
const bodyOf = (fetchMock: { mock: { calls: unknown[][] } }, i: number) =>
  JSON.parse((fetchMock.mock.calls[i]![1] as RequestInit).body as string);

describe("per-run routing", () => {
  it("runStreaming sends it when set, including an empty block, and omits it otherwise", async () => {
    const { client, fetchMock } = makeClient([done(), done(), done()]);
    for await (const _ of client.runStreaming({
      agent: "a",
      segments: [],
      routing: { queue_aware: true, switch_after_ms: 15000 },
    })) void _;
    expect(bodyOf(fetchMock, 0).routing).toEqual({ queue_aware: true, switch_after_ms: 15000 });

    for await (const _ of client.runStreaming({ agent: "a", segments: [], routing: {} })) void _;
    expect(bodyOf(fetchMock, 1).routing).toEqual({});

    for await (const _ of client.runStreaming({ agent: "a", segments: [] })) void _;
    expect("routing" in bodyOf(fetchMock, 2)).toBe(false);
  });

  it("continueSession sends it too", async () => {
    const { client, fetchMock } = makeClient([done()]);
    for await (const _ of client.continueSession({
      sessionId: "s1",
      segments: [],
      routing: { queue_aware: true },
    })) void _;
    expect(bodyOf(fetchMock, 0).routing).toEqual({ queue_aware: true });
  });

  it("each batch child and a configured run carry their own", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({ results: [] }),
      jsonResponse({ run_id: "r_1", session_id: "s_1", agent_id: "a_1", status: "configured" }),
    ]);
    await client.spawnRunBatch({
      spawns: [
        { agent: "a", segments: [], routing: { queue_aware: true } },
        { agent: "a", segments: [] },
      ],
    });
    const batch = bodyOf(fetchMock, 0);
    expect(batch.spawns[0].routing).toEqual({ queue_aware: true });
    expect("routing" in batch.spawns[1]).toBe(false);

    await client.createConfiguredRun({ agent: "a", segments: [], routing: { switch_after_ms: 5000 } });
    expect(bodyOf(fetchMock, 1).routing).toEqual({ switch_after_ms: 5000 });
  });

  it("a retune and a steered turn carry it, an empty block included", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({ run_id: "r_1", retuned: true }),
      jsonResponse({ run_id: "r_1", delivered: true }),
    ]);
    await client.retuneRun("r_1", { routing: { queue_aware: true, switch_after_ms: 15000 } });
    expect(bodyOf(fetchMock, 0).routing).toEqual({ queue_aware: true, switch_after_ms: 15000 });

    await client.sendRunInput("r_1", "go on", { overrides: { routing: {} } });
    expect(bodyOf(fetchMock, 1).overrides.routing).toEqual({});
  });
});
