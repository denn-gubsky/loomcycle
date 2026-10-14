import { beforeEach, describe, expect, it } from "vitest";

import { VERB } from "../src/cards.js";
import { conversationSubject } from "../src/identity.js";
import type { ConversationRef } from "../src/ports.js";
import { ConversationRouter, NOT_AVAILABLE_TEXT } from "../src/router.js";
import { MemoryStore } from "../src/store.js";
import { TokenBroker } from "../src/tokens.js";
import { FakeLoomcycle, FakeTeams, httpError, settle, silentLogger, waitFor, type StartedRun } from "./fakes.js";

const TENANT = "11111111-1111-1111-1111-111111111111";
const ALICE = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa";
const BOB = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb";
const ADMIN_TOKEN = "admin-token";

type ConvType = "personal" | "groupChat" | "channel";

function activity(user: string, name: string, conv = "conv-1", type: ConvType = "personal", tenant = TENANT) {
  return {
    from: { aadObjectId: user, name },
    conversation: { id: conv, tenantId: tenant, conversationType: type },
  };
}

function refFor(conv = "conv-1", type: ConvType = "personal"): ConversationRef {
  return { conversation: { id: conv, conversationType: type, tenantId: TENANT }, serviceUrl: "https://smba.example/" };
}

describe("teams bridge conversation", () => {
  let lc: FakeLoomcycle;
  let teams: FakeTeams;
  let store: MemoryStore;
  let now: number;
  let router: ConversationRouter;

  function makeRouter(rolloverMs = 100_000) {
    const broker = new TokenBroker(lc.client(ADMIN_TOKEN), {
      agent: "helper",
      ttlSeconds: 43200,
      ratePerMinute: 6,
      rateBurst: 10,
      accessMode: "isolated",
      now: () => now,
    });
    return new ConversationRouter(
      { allowedTenants: new Set([TENANT]), idleMinutes: 60 },
      {
        agent: "helper",
        store,
        broker,
        clients: lc.client,
        port: teams,
        log: silentLogger,
        now: () => now,
        stream: { rolloverMs, stopPollMs: 5 },
        message: { typingEveryMs: 60_000 },
      },
    );
  }

  beforeEach(() => {
    lc = new FakeLoomcycle();
    teams = new FakeTeams();
    store = new MemoryStore();
    now = 1_000_000;
    lc.now = () => now;
    router = makeRouter();
  });

  /** say sends a message and, when it starts a run, announces the run like
   *  loomcycle does, so the router's onMessage can settle. */
  async function say(text: string, user = ALICE, name = "Alice", conv = "conv-1", type: ConvType = "personal") {
    const before = lc.runs.length;
    await router.onMessage(activity(user, name, conv, type), refFor(conv, type), text);
    await settle();
    if (lc.runs.length > before) announce(lc.runs[lc.runs.length - 1], `run-${lc.runs.length}`);
    await settle();
  }

  function announce(run: StartedRun, runId: string) {
    run.events.push({ type: "session", session_id: "sess-1" });
    run.events.push({ type: "agent", run_id: runId, agent_id: `agent-${runId}`, session_id: "sess-1", parent_agent_id: null });
  }

  it("starts an interactive run for the first message, as the user, with a restricted token", async () => {
    await say("hello there");

    expect(lc.runs).toHaveLength(1);
    const run = lc.runs[0];
    expect(run.kind).toBe("start");
    expect(run.opts).toMatchObject({ agent: "helper", interactive: true });
    expect(run.opts.segments).toEqual([{ role: "user", content: [{ type: "trusted-text", text: "hello there" }] }]);
    // The run uses the user's own token, never the bridge's.
    expect(run.token).toMatch(/^tok-teams-aaaaaaaa-/);
    expect(lc.createdUsers).toEqual([{ subject: `teams-${ALICE}`, display_name: "Alice", access_mode: "isolated" }]);
    expect(lc.mints).toHaveLength(1);
    expect(lc.mints[0].opts).toEqual({
      ttlSeconds: 43200,
      name: "teams-bridge",
      maxActive: 2,
      agents: ["helper"],
      overrides: ["interactive"],
      rate: { perMinute: 6, burst: 10 },
    });
    expect((await store.get("conv-1"))?.runId).toBe("run-1");
  });

  it("streams a 1:1 turn and closes the message when the run parks", async () => {
    await say("hi");
    const run = lc.runs[0];
    run.events.push({ type: "tool_call", tool_use: { id: "t1", name: "WebSearch", input: {} } });
    run.events.push({ type: "text", text: "Hello " });
    run.events.push({ type: "text", text: "Alice" });
    run.events.push({ type: "awaiting_input" });
    await waitFor(() => teams.streams[0]?.closed, "stream closed");

    const s = teams.streams[0];
    expect(s.updates).toEqual(["Using WebSearch…"]);
    expect(s.text).toBe("Hello Alice");
    expect(teams.streams).toHaveLength(1);
  });

  it("steers the live run with a second message instead of starting another", async () => {
    await say("first");
    await say("and also this");

    expect(lc.runs).toHaveLength(1);
    expect(lc.callsTo("sendRunInput").map((c) => c.args)).toEqual([["run-1", "and also this"]]);
  });

  it("continues the stored session in a new run once the old run has ended", async () => {
    await say("first");
    lc.runs[0].events.end();
    await settle();
    await say("again");

    expect(lc.runs).toHaveLength(2);
    expect(lc.runs[1].kind).toBe("continue");
    expect(lc.runs[1].opts).toMatchObject({ sessionId: "sess-1", interactive: true });
  });

  it("after a restart, ends the old run and continues its session rather than replaying it", async () => {
    await store.put({
      conversationId: "conv-1",
      scope: "personal",
      ref: refFor(),
      ownerSubject: `teams-${ALICE}`,
      ownerName: "Alice",
      runSubject: `teams-${ALICE}`,
      runName: "Alice",
      sessionId: "sess-old",
      runId: "run-old",
      agentId: "agent-old",
      lastActivityAt: now,
    });
    await say("are you still there?");

    expect(lc.callsTo("cancelAgent").map((c) => c.args[0])).toEqual(["agent-old"]);
    expect(lc.callsTo("sendRunInput")).toHaveLength(0);
    expect(lc.runs).toHaveLength(1);
    expect(lc.runs[0].kind).toBe("continue");
    expect(lc.runs[0].opts).toMatchObject({ sessionId: "sess-old" });
  });

  it("/new ends the run and starts the next conversation in a fresh session", async () => {
    await say("first");
    await say("/new");
    expect(lc.callsTo("cancelAgent").map((c) => c.args[0])).toEqual(["agent-run-1"]);
    expect((await store.get("conv-1"))?.sessionId).toBeUndefined();

    await say("fresh start");
    expect(lc.runs).toHaveLength(2);
    expect(lc.runs[1].kind).toBe("start");
  });

  it("/stop cancels only the current turn", async () => {
    await say("long task");
    await say("/stop");
    expect(lc.callsTo("cancelTurn").map((c) => c.args[0])).toEqual(["run-1"]);
    expect(lc.callsTo("cancelAgent")).toHaveLength(0);
  });

  it("maps the Teams Stop button to cancelling the turn", async () => {
    await say("write an essay");
    lc.runs[0].events.push({ type: "text", text: "Once upon" });
    await waitFor(() => teams.streams.length === 1, "stream");
    teams.streams[0].canceled = true;
    await waitFor(() => lc.callsTo("cancelTurn").length === 1, "cancelTurn");
    expect(lc.callsTo("cancelTurn")[0].args[0]).toBe("run-1");
  });

  it("rolls a long 1:1 answer over into a new message before the two-minute limit", async () => {
    router = makeRouter(1000);
    await say("long");
    const run = lc.runs[0];
    run.events.push({ type: "text", text: "part one. " });
    await waitFor(() => teams.streams.length === 1, "first stream");
    now += 1500;
    run.events.push({ type: "text", text: "part two." });
    await waitFor(() => teams.streams.length === 2, "second stream");

    expect(teams.streams[0].closed).toBe(true);
    expect(teams.streams[0].text).toBe("part one. ");
    expect(teams.streams[1].text).toBe("part two.");
  });

  it("asks a question with a card and only the owner's click answers it", async () => {
    await say("book a room", ALICE, "Alice", "conv-g", "groupChat");
    const run = lc.runs[0];
    run.events.push({
      type: "interruption_pending",
      interruption: { interrupt_id: "q1", kind: "question", question: "Which floor?", options: ["2", "3"], priority: "normal" },
    });
    await waitFor(() => teams.cards().length === 1, "question card");

    const click = (user: string, name: string, answer: string) =>
      router.onCardAction(
        { ...activity(user, name, "conv-g", "groupChat"), value: { action: { verb: VERB.answer, data: { runId: "run-1", interruptId: "q1", answer } } } },
        refFor("conv-g", "groupChat"),
      );

    expect(await click(BOB, "Bob", "2")).toEqual({ message: "Only Alice can answer this." });
    expect(lc.callsTo("resolveInterrupt")).toHaveLength(0);

    const res = await click(ALICE, "Alice", "3");
    expect(res).toMatchObject({ card: { type: "AdaptiveCard" } });
    expect(lc.callsTo("resolveInterrupt").map((c) => c.args)).toEqual([["run-1", "q1", { answer: "3", resolvedBy: `teams-${ALICE}` }]]);
  });

  it("takes the owner's plain reply as the answer to an open free-text question", async () => {
    await say("plan my trip");
    lc.runs[0].events.push({
      type: "interruption_pending",
      interruption: { interrupt_id: "q9", kind: "question", question: "Where to?", priority: "normal" },
    });
    await waitFor(() => teams.cards().length === 1, "question card");
    await say("Lisbon");

    expect(lc.callsTo("resolveInterrupt").map((c) => c.args)).toEqual([["run-1", "q9", { answer: "Lisbon", resolvedBy: `teams-${ALICE}` }]]);
    expect(lc.callsTo("sendRunInput")).toHaveLength(0);
  });

  it("approves a held answer from its review card", async () => {
    await say("draft the memo");
    lc.runs[0].events.push({ type: "awaiting_review", awaiting_review: { round: 1 } });
    await waitFor(() => teams.cards().length === 1, "review card");
    const res = await router.onCardAction(
      { ...activity(ALICE, "Alice"), value: { action: { verb: VERB.approve, data: { runId: "run-1" } } } },
      refFor(),
    );
    expect(res).toMatchObject({ card: {} });
    expect(lc.callsTo("reviewRun").map((c) => c.args.slice(0, 2))).toEqual([["run-1", "approve"]]);
  });

  it("in a group chat sends one message per turn and tells the agent who else spoke", async () => {
    await say("summarise the thread", ALICE, "Alice", "conv-g", "groupChat");
    const run = lc.runs[0];
    run.events.push({ type: "text", text: "Here is " });
    run.events.push({ type: "text", text: "the summary." });
    run.events.push({ type: "awaiting_input" });
    await waitFor(() => teams.messages().length === 1, "group reply");

    expect(teams.streams).toHaveLength(0);
    expect(teams.messages()).toEqual(["Here is the summary."]);

    await say("add the action items", BOB, "Bob", "conv-g", "groupChat");
    const steer = lc.callsTo("sendRunInput")[0];
    expect(steer.args[1]).toBe("[Bob] add the action items");
    expect(run.opts.segments[0].content[0]).toMatchObject({ text: "[Alice] summarise the thread" });
  });

  it("runs a shared chat as the conversation itself, never as one of its members", async () => {
    await say("hello", ALICE, "Alice", "conv-g", "groupChat");
    const subject = conversationSubject(TENANT, "conv-g");
    expect(subject).toMatch(/^teams-conv-[0-9a-f]{32}$/);
    // Registered and minted for the conversation; Alice is never registered.
    expect(lc.createdUsers.map((u) => u.subject)).toEqual([subject]);
    expect(lc.runs[0].token).toMatch(/^tok-teams-conv-/);

    await say("and me", BOB, "Bob", "conv-g", "groupChat");
    expect(lc.callsTo("sendRunInput")[0].token).toBe(lc.runs[0].token);
  });

  it("gives every thread of a channel the channel's one identity", () => {
    const a = conversationSubject(TENANT, "19:chan@thread.tacv2;messageid=111");
    const b = conversationSubject(TENANT, "19:chan@thread.tacv2;messageid=222");
    expect(a).toBe(b);
    expect(a).not.toBe(conversationSubject(TENANT, "19:other@thread.tacv2;messageid=111"));
  });

  it("does not let another member of a group throw the conversation away", async () => {
    await say("start", ALICE, "Alice", "conv-g", "groupChat");
    await say("/new", BOB, "Bob", "conv-g", "groupChat");
    expect(lc.callsTo("cancelAgent")).toHaveLength(0);
    expect(teams.messages().at(-1)).toContain("Only Alice");
  });

  it("refuses a tenant outside the allowlist once, without calling loomcycle", async () => {
    const other = "22222222-2222-2222-2222-222222222222";
    await router.onMessage(activity(ALICE, "Alice", "conv-x", "personal", other), refFor("conv-x"), "hi");
    await router.onMessage(activity(ALICE, "Alice", "conv-x", "personal", other), refFor("conv-x"), "hello?");
    await settle();

    expect(lc.calls).toHaveLength(0);
    expect(teams.messages()).toEqual([NOT_AVAILABLE_TEXT]);
  });

  it("tells the user plainly when loomcycle rate-limits a run start", async () => {
    lc.startError = httpError(429);
    await router.onMessage(activity(ALICE, "Alice"), refFor(), "hi");
    await waitFor(() => teams.messages().length === 1, "error message");
    expect(teams.messages()[0]).toContain("faster than the agent accepts");
  });

  it("ends a run an earlier process left parked once its conversation goes idle", async () => {
    await store.put({
      conversationId: "conv-old",
      scope: "personal",
      ref: refFor("conv-old"),
      ownerSubject: `teams-${ALICE}`,
      ownerName: "Alice",
      runSubject: `teams-${ALICE}`,
      runName: "Alice",
      sessionId: "sess-old",
      runId: "run-old",
      agentId: "agent-old",
      lastActivityAt: now - 2 * 3600_000,
    });
    await router.sweepIdle();

    expect(lc.callsTo("cancelAgent").map((c) => c.args[0])).toEqual(["agent-old"]);
    const rec = await store.get("conv-old");
    expect(rec?.agentId).toBeUndefined();
    expect(rec?.sessionId).toBe("sess-old");
  });

  it("ends an idle run but keeps its session", async () => {
    await say("hello");
    now += 61 * 60_000;
    await router.sweepIdle();

    expect(lc.callsTo("cancelAgent").map((c) => c.args[0])).toEqual(["agent-run-1"]);
    expect((await store.get("conv-1"))?.sessionId).toBe("sess-1");
    // An idle conversation leaves memory; the store brings it back.
    expect(router.get("conv-1")).toBeUndefined();
  });
});
