import { describe, expect, it } from "vitest";
import { LoomcycleError } from "../src/errors.js";
import { errorResponse, jsonResponse, makeClient } from "./helpers.js";

// RFC S client twins — awaitChannels (fan-in) / broadcastChannels
// (fan-out). The adapter is a thin POST wrapper, so the tests pin the
// path + method + body shape + that the result parses, plus the
// atomic-refusal error surfacing.

describe("awaitChannels", () => {
  it("POSTs to /v1/_channels/_await with the fan-in body", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({
        satisfied: true,
        timed_out: false,
        mode: "at_least",
        fired: ["a", "b"],
        total_messages: 2,
        results: {
          a: { messages: [{ id: "m1", value: { x: 1 }, published_at: "t" }], next_cursor: "m1" },
          b: { messages: [{ id: "m2", value: { y: 1 }, published_at: "t" }], next_cursor: "m2" },
        },
      }),
    ]);

    const res = await client.awaitChannels({
      channels: ["a", "b", "c"],
      scope: "global",
      mode: "at_least",
      n: 2,
      waitMs: 30000,
    });

    expect(res.satisfied).toBe(true);
    expect(res.total_messages).toBe(2);
    expect(res.results.a!.next_cursor).toBe("m1");

    const [url, init] = fetchMock.mock.calls[0]!;
    expect(url).toBe("http://test-loomcycle:8787/v1/_channels/_await");
    expect(init.method).toBe("POST");
    expect(JSON.parse(init.body as string)).toEqual({
      channels: ["a", "b", "c"],
      scope: "global",
      mode: "at_least",
      n: 2,
      wait_ms: 30000,
    });
  });

  it("maps userId to scope_id for user-scoped fan-in", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({ satisfied: false, timed_out: true, mode: "any", fired: [], total_messages: 0, results: {} }),
    ]);
    await client.awaitChannels({ channels: ["a"], scope: "user", userId: "alice" });
    const [, init] = fetchMock.mock.calls[0]!;
    expect(JSON.parse(init.body as string)).toEqual({
      channels: ["a"],
      scope: "user",
      scope_id: "alice",
    });
  });
});

describe("broadcastChannels", () => {
  it("POSTs to /v1/_channels/_broadcast with the fan-out body", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({
        published: 3,
        failed: 0,
        results: [
          { channel: "a", msg_id: "m1", created_at: "t" },
          { channel: "b", msg_id: "m2", created_at: "t" },
          { channel: "c", msg_id: "m3", created_at: "t" },
        ],
      }),
    ]);

    const res = await client.broadcastChannels({
      channels: ["a", "b", "c"],
      scope: "global",
      payload: { go: 1 },
    });

    expect(res.published).toBe(3);
    expect(res.failed).toBe(0);
    expect(res.results[0]!.channel).toBe("a");

    const [url, init] = fetchMock.mock.calls[0]!;
    expect(url).toBe("http://test-loomcycle:8787/v1/_channels/_broadcast");
    expect(init.method).toBe("POST");
    expect(JSON.parse(init.body as string)).toEqual({
      channels: ["a", "b", "c"],
      scope: "global",
      payload: { go: 1 },
    });
  });

  it("surfaces an undeclared-channel refusal as a typed error", async () => {
    const { client } = makeClient([
      errorResponse(404, "channel_not_declared", `channel "ghost" is not declared`),
    ]);
    await expect(
      client.broadcastChannels({ channels: ["a", "ghost"], scope: "global", payload: { x: 1 } }),
    ).rejects.toBeInstanceOf(LoomcycleError);
  });
});
