// adapters/ts/tests/interactive.test.ts — RFC AI interactive agentic sessions:
// the interactive flag, sendRunInput, streamRunByID re-attach, and the
// high-level InteractiveSession driver.

import { describe, expect, it } from "vitest";

import { jsonResponse, makeClient, sseResponse } from "./helpers.js";
import { InteractiveSession } from "../src/index.js";

describe("runStreaming interactive flag", () => {
  it("sends interactive:true in the POST body", async () => {
    const { client, fetchMock } = makeClient([
      sseResponse(['event: awaiting_input\ndata: {"type":"awaiting_input","awaiting_input":{"since_turn":1}}\n\n']),
    ]);
    const events = [];
    for await (const ev of client.runStreaming({ agent: "chat", segments: [], interactive: true })) {
      events.push(ev);
    }
    const body = JSON.parse(fetchMock.mock.calls[0]![1]!.body as string);
    expect(body.interactive).toBe(true);
    // the awaiting_input payload round-trips
    expect(events[0]!.type).toBe("awaiting_input");
    expect(events[0]!.awaiting_input?.since_turn).toBe(1);
  });
});

describe("sendRunInput", () => {
  it("POSTs the steer text to /v1/runs/{id}/input and returns delivered", async () => {
    const { client, fetchMock } = makeClient([jsonResponse({ run_id: "r_abc", delivered: true })]);
    const out = await client.sendRunInput("r_abc", "focus on the failing test");
    expect(out).toEqual({ run_id: "r_abc", delivered: true });
    const call = fetchMock.mock.calls[0]!;
    expect(call[0]).toBe("http://test-loomcycle:8787/v1/runs/r_abc/input");
    expect(call[1]!.method).toBe("POST");
    expect(JSON.parse(call[1]!.body as string)).toEqual({ text: "focus on the failing test" });
  });
});

describe("streamRunByID re-attach", () => {
  it("GETs /v1/runs/{id}/stream?from_seq=N and replays operator turns as steer events", async () => {
    const { client, fetchMock } = makeClient([
      sseResponse([
        'event: agent\ndata: {"agent_id":"a1","run_id":"r_abc","session_id":"s1"}\n\n',
        'event: steer\ndata: {"type":"steer","user_input":{"text":"earlier turn","source":"replay"}}\n\n',
        'event: text\ndata: {"type":"text","text":"resuming"}\n\n',
      ]),
    ]);
    const events = [];
    for await (const ev of client.streamRunByID("r_abc", { fromSeq: 12 })) {
      events.push(ev);
    }
    const call = fetchMock.mock.calls[0]!;
    expect(call[0]).toBe("http://test-loomcycle:8787/v1/runs/r_abc/stream?from_seq=12");
    expect(call[1]!.method).toBe("GET");
    // The replayed operator turn surfaces as a steer event with source=replay.
    const steer = events.find((e) => e.type === "steer");
    expect(steer?.user_input?.text).toBe("earlier turn");
    expect(steer?.user_input?.source).toBe("replay");
  });
});

describe("InteractiveSession", () => {
  it("tracks run IDs + awaiting state from the stream and steers via send()", async () => {
    const { client, fetchMock } = makeClient([
      // 1) the interactive run stream: agent frame → awaiting_input
      sseResponse([
        'event: agent\ndata: {"agent_id":"a1","run_id":"r_abc","session_id":"s1"}\n\n',
        'event: awaiting_input\ndata: {"type":"awaiting_input"}\n\n',
      ]),
      // 2) the sendRunInput POST
      jsonResponse({ run_id: "r_abc", delivered: true }),
    ]);

    const session = client.interactiveSession({ agent: "chat", segments: [] });
    expect(session).toBeInstanceOf(InteractiveSession);

    let parked = false;
    for await (const ev of session.events()) {
      if (ev.type === "awaiting_input") {
        parked = true;
        break; // consumed the agent frame already → run_id known
      }
    }
    expect(parked).toBe(true);
    expect(session.runId).toBe("r_abc");
    expect(session.agentId).toBe("a1");
    expect(session.sessionId).toBe("s1");
    expect(session.awaitingInput).toBe(true);

    const delivered = await session.send("keep going");
    expect(delivered).toBe(true);
    expect(session.awaitingInput).toBe(false);

    // the run stream POSTed interactive:true; the steer hit /input
    expect(JSON.parse(fetchMock.mock.calls[0]![1]!.body as string).interactive).toBe(true);
    expect(fetchMock.mock.calls[1]![0]).toBe("http://test-loomcycle:8787/v1/runs/r_abc/input");
  });

  it("send() throws before the run_id is known", async () => {
    const { client } = makeClient([sseResponse([])]);
    const session = client.interactiveSession({ agent: "chat", segments: [] });
    await expect(session.send("too early")).rejects.toThrow(/run_id not known/);
  });

  it("attachInteractiveSession knows the run_id up front", async () => {
    const { client } = makeClient([jsonResponse({ run_id: "r_xyz", delivered: true })]);
    const session = client.attachInteractiveSession("r_xyz");
    expect(session.runId).toBe("r_xyz");
    // send works immediately without consuming events
    await expect(session.send("hi")).resolves.toBe(true);
  });
});
