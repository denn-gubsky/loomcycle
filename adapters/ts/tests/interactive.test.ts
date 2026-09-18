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

// The gap a consumer reported at v1.82.0: the wire accepted per-run overrides on
// the steer path and on a dedicated retune route, and neither was reachable from
// this adapter — sendRunInput sent a bare {text} and retuneRun did not exist.
describe("retuning a parked run", () => {
  it("retuneRun POSTs the overrides to /retune, flat and without a turn", async () => {
    const { client, fetchMock } = makeClient([jsonResponse({ run_id: "r_abc", retuned: true })]);
    const out = await client.retuneRun("r_abc", { model: "claude-x", maxIterations: 40 });
    expect(out).toEqual({ run_id: "r_abc", retuned: true });

    const call = fetchMock.mock.calls[0]!;
    expect(String(call[0])).toContain("/v1/runs/r_abc/retune");
    const body = JSON.parse(call[1]!.body as string);
    expect(body).toEqual({ model: "claude-x", max_iterations: 40 });
    // No turn rides along: a retune must not put a message in the transcript.
    expect(body.text).toBeUndefined();
  });

  it("sendRunInput nests overrides beside the text, matching that endpoint's shape", async () => {
    const { client, fetchMock } = makeClient([jsonResponse({ run_id: "r_abc", delivered: true })]);
    await client.sendRunInput("r_abc", "carry on", { overrides: { model: "claude-x" } });

    const body = JSON.parse(fetchMock.mock.calls[0]![1]!.body as string);
    expect(body.text).toBe("carry on");
    // Nested here, flat on /retune — the two endpoints differ and the adapter
    // has to match each, which is why this asserts the SHAPE and not just the value.
    expect(body.overrides).toEqual({ model: "claude-x" });
  });

  it("sendRunInput still sends a bare body when no overrides are given", async () => {
    const { client, fetchMock } = makeClient([jsonResponse({ run_id: "r_abc", delivered: true })]);
    await client.sendRunInput("r_abc", "hello");
    const body = JSON.parse(fetchMock.mock.calls[0]![1]!.body as string);
    expect(body).toEqual({ text: "hello" });
  });

  it("the meaningful zeros survive — 0 and false are sent, not dropped as falsy", async () => {
    const { client, fetchMock } = makeClient([jsonResponse({ run_id: "r_abc", retuned: true })]);
    await client.retuneRun("r_abc", {
      retryAttempts: 0,
      injectToolGuide: false,
      unboundedIterations: false,
    });
    const body = JSON.parse(fetchMock.mock.calls[0]![1]!.body as string);
    expect(body.retry_attempts).toBe(0);
    expect(body.inject_tool_guide).toBe(false);
    expect(body.unbounded_iterations).toBe(false);
  });

  it("InteractiveSession.retune() drives it, and send() can carry overrides", async () => {
    const calls: Array<Record<string, unknown>> = [];
    const sess = new InteractiveSession((async function* () {})(), {
      sendRunInput: async (runId, text, opts) => {
        calls.push({ kind: "send", runId, text, overrides: opts?.overrides });
        return { delivered: true };
      },
      retuneRun: async (runId, overrides) => {
        calls.push({ kind: "retune", runId, overrides });
        return { retuned: true };
      },
      cancelAgent: async () => ({}),
    });
    sess.runId = "r_abc";

    expect(await sess.retune({ model: "claude-x" })).toBe(true);
    expect(await sess.send("go", { overrides: { tier: "middle" } })).toBe(true);

    expect(calls).toEqual([
      { kind: "retune", runId: "r_abc", overrides: { model: "claude-x" } },
      { kind: "send", runId: "r_abc", text: "go", overrides: { tier: "middle" } },
    ]);
  });
});

// The typed path a panel needs: what a run overrides, what it will actually
// use, and what a retune left it holding. The endpoints shipped server-side and
// the adapter exposed none of them, so a consumer had to drop to raw fetch.
describe("reading a run's configuration", () => {
  it("getRunConfig GETs /config and returns what the RUN overrides", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({
        run_id: "r_abc",
        agent: "chat",
        model: "claude-x",
        config: { routing: { model: "claude-x" }, resources: { max_iterations: 40 } },
      }),
    ]);
    const out = await client.getRunConfig("r_abc");

    const call = fetchMock.mock.calls[0]!;
    expect(String(call[0])).toContain("/v1/runs/r_abc/config");
    expect(call[1]?.method ?? "GET").toBe("GET");
    expect(out.config.routing?.model).toBe("claude-x");
    expect(out.config.resources?.max_iterations).toBe(40);
  });

  it("an un-retuned run comes back with an empty config, not an error", async () => {
    const { client } = makeClient([
      jsonResponse({ run_id: "r_abc", agent: "chat", model: "claude-x", config: {} }),
    ]);
    const out = await client.getRunConfig("r_abc");
    expect(out.config).toEqual({});
    expect(out.config.routing).toBeUndefined();
  });

  it("getEffectiveConfig returns each field with the layer that decided it", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({
        run_id: "r_abc",
        agent: "chat",
        fields: {
          max_iterations: { value: 16, source: "default" },
          tier: { value: "middle", source: "definition" },
          model: { value: "claude-x", source: "resolved" },
          retry_attempts: { value: 0, source: "user_tier" },
          max_tokens: { value: null, source: "resolved" },
        },
      }),
    ]);
    const out = await client.getEffectiveConfig("r_abc");

    expect(String(fetchMock.mock.calls[0]![0])).toContain("/v1/runs/r_abc/effective-config");
    // The source is the point: 16 alone cannot distinguish a deliberate setting
    // from a default nobody chose, and those call for opposite actions.
    expect(out.fields.max_iterations!.source).toBe("default");
    expect(out.fields.tier!.source).toBe("definition");
    expect(out.fields.model!.source).toBe("resolved");
    expect(out.fields.retry_attempts!.source).toBe("user_tier");
    // A resolved field with no value is an honest answer, not a missing one.
    expect(out.fields.max_tokens!.value).toBeNull();
  });

  it("retuneRun returns the MERGED config, which the caller cannot recompute", async () => {
    const { client } = makeClient([
      jsonResponse({
        run_id: "r_abc",
        retuned: true,
        // The server cleared `provider` because a model was named. Echoing the
        // request back would have shown a provider the run no longer has.
        config: { routing: { model: "claude-x" } },
      }),
    ]);
    const out = await client.retuneRun("r_abc", { model: "claude-x" });
    expect(out.retuned).toBe(true);
    expect(out.config.routing?.model).toBe("claude-x");
    expect(out.config.routing?.provider).toBeUndefined();
  });
});
