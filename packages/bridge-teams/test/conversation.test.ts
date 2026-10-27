import http from "node:http";
import net from "node:net";

import { afterEach, beforeEach, describe, expect, it } from "vitest";

import { ConversationRouter, keyFromSecret, MemoryStore, TokenBroker, VERB, type Logger } from "@loomcycle/bridge-core";

import { createTeamsBridge, type ConversationRef, type TeamsBridge } from "../src/bridge.js";
import { parseCardAction, renderCard } from "../src/cards.js";
import { loadConfig } from "../src/config.js";
import { conversationSubject, identify, NOT_AVAILABLE_TEXT, type ActivityIdentityFields } from "../src/identity.js";
import { FakeGate, FakeLoomcycle, FakeTeams, httpError, RecordingLogger, settle, silentLogger, waitFor, type StartedRun } from "./fakes.js";

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
  let store: MemoryStore<ConversationRef>;
  let now: number;
  let router: ConversationRouter<ConversationRef>;
  const TENANTS = new Set([TENANT]);

  /** What bridge.ts does with an inbound activity: identify, then route. */
  const inbound = {
    onMessage: (a: ActivityIdentityFields, ref: ConversationRef, text: string) => router.onMessage(identify(a, TENANTS), ref, text),
    onCardAction: (a: ActivityIdentityFields & { value?: unknown }, ref: ConversationRef) =>
      router.onCardAction(identify(a, TENANTS), ref, parseCardAction(a.value)),
  };

  function makeRouter(rolloverMs = 100_000) {
    const broker = new TokenBroker(lc.client(ADMIN_TOKEN), {
      agent: "helper",
      tokenName: "teams-bridge",
      ttlSeconds: 43200,
      ratePerMinute: 6,
      rateBurst: 10,
      accessMode: "isolated",
      now: () => now,
    });
    return new ConversationRouter<ConversationRef>(
      { idleMinutes: 60 },
      {
        platform: { name: "teams", label: "Teams" },
        capabilities: { streaming: "personal-only", maxMessageChars: 20000 },
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
    await inbound.onMessage(activity(user, name, conv, type), refFor(conv, type), text);
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
      inbound.onCardAction(
        { ...activity(user, name, "conv-g", "groupChat"), value: { action: { verb: VERB.answer, data: { runId: "run-1", interruptId: "q1", answer } } } },
        refFor("conv-g", "groupChat"),
      );

    expect(await click(BOB, "Bob", "2")).toEqual({ message: "Only Alice can answer this." });
    expect(lc.callsTo("resolveInterrupt")).toHaveLength(0);

    const res = await click(ALICE, "Alice", "3");
    expect(res && "card" in res && renderCard(res.card)).toMatchObject({ type: "AdaptiveCard" });
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
    const res = await inbound.onCardAction(
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
    await inbound.onMessage(activity(ALICE, "Alice", "conv-x", "personal", other), refFor("conv-x"), "hi");
    await inbound.onMessage(activity(ALICE, "Alice", "conv-x", "personal", other), refFor("conv-x"), "hello?");
    await settle();

    expect(lc.calls).toHaveLength(0);
    expect(teams.messages()).toEqual([NOT_AVAILABLE_TEXT]);
  });

  it("tells the user plainly when loomcycle rate-limits a run start", async () => {
    lc.startError = httpError(429);
    await inbound.onMessage(activity(ALICE, "Alice"), refFor(), "hi");
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

function freePort(): Promise<number> {
  return new Promise((resolve, reject) => {
    const s = net.createServer();
    s.once("error", reject);
    s.listen(0, "127.0.0.1", () => {
      const port = (s.address() as net.AddressInfo).port;
      s.close(() => resolve(port));
    });
  });
}

// The whole bridge as a loomcycle gate (RFC FA §3.7d): the Teams SDK app in
// its local (unauthenticated) mode, activities POSTed to /api/messages, a
// recording Bot Connector for replies, and a fake loomcycle gate surface.
describe("teams bridge as a loomcycle gate", () => {
  const OTHER_TENANT = "22222222-2222-2222-2222-222222222222";
  const SECRET = "s".repeat(40);
  let lc: FakeLoomcycle;
  let gate: FakeGate;
  let connector: http.Server;
  let connectorUrl: string;
  let bridge: TeamsBridge | undefined;
  let port: number;
  let seq = 0;

  beforeEach(async () => {
    lc = new FakeLoomcycle();
    gate = new FakeGate();
    connector = http.createServer((req, res) => {
      req.resume();
      req.on("end", () => {
        res.writeHead(201, { "Content-Type": "application/json" });
        res.end(JSON.stringify({ id: `act-${++seq}` }));
      });
    });
    await new Promise<void>((r) => connector.listen(0, "127.0.0.1", r));
    connectorUrl = `http://127.0.0.1:${(connector.address() as net.AddressInfo).port}/`;
  });

  afterEach(async () => {
    for (const run of lc.runs) run.events.end();
    await bridge?.stop();
    bridge = undefined;
    await new Promise((r) => connector.close(r));
  });

  async function start(opts: { asGate?: boolean; logger?: Logger } = {}) {
    port = await freePort();
    const auth = opts.asGate === false
      ? { LOOMCYCLE_TOKEN: ADMIN_TOKEN }
      : { BRIDGE_GATE: "teams", BRIDGE_GATE_AUDIENCE: "https://lc.example", BRIDGE_GATE_SECRET: SECRET };
    const cfg = loadConfig({
      LOOMCYCLE_BASE_URL: "http://loomcycle.test",
      ...auth,
      BRIDGE_AGENT: "helper",
      TEAMS_BRIDGE_ALLOWED_TENANTS: TENANT,
      BRIDGE_DEV: "1",
      PORT: String(port),
    });
    bridge = createTeamsBridge(cfg, {
      clients: lc.client,
      gateFetch: gate.fetch,
      gateKey: keyFromSecret(SECRET),
      logger: opts.logger ?? silentLogger,
      store: new MemoryStore(),
      stream: { stopPollMs: 5 },
    });
    await bridge.start();
  }

  const post = (activity: Record<string, unknown>) =>
    fetch(`http://127.0.0.1:${port}/api/messages`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        channelId: "msteams",
        serviceUrl: connectorUrl,
        timestamp: new Date().toISOString(),
        id: `in-${++seq}`,
        from: { id: "29:user", aadObjectId: ALICE, name: "Alice" },
        recipient: { id: "28:bot", name: "Bot" },
        conversation: { id: "a:personal", conversationType: "personal", tenantId: TENANT },
        channelData: { tenant: { id: TENANT } },
        ...activity,
      }),
    });

  const uninstall = (over: Record<string, unknown> = {}) => post({ type: "installationUpdate", action: "remove", ...over });

  /** message posts a message and lets the run it starts announce itself. */
  async function message(conversation: Record<string, unknown>, text = "hello") {
    const before = lc.runs.length;
    const res = post({ type: "message", text, conversation });
    await waitFor(() => lc.runs.length > before, "a run");
    announce(lc.runs.at(-1)!, `run-${lc.runs.length}`);
    expect((await res).status).toBe(200);
  }

  function announce(run: StartedRun, runId: string) {
    run.events.push({ type: "session", session_id: `sess-${runId}` });
    run.events.push({ type: "agent", run_id: runId, agent_id: `agent-${runId}`, session_id: `sess-${runId}`, parent_agent_id: null });
  }

  it("asserts a person and a shared chat with their tenant's group, and mints nothing", async () => {
    await start();
    await message({ id: "a:personal", conversationType: "personal", tenantId: TENANT });
    expect(gate.asserts).toHaveLength(1);
    expect(gate.asserts[0]!.gate).toBe("teams");
    expect(gate.asserts[0]!.payload).toMatchObject({
      iss: "teams",
      aud: "https://lc.example",
      sub: ALICE,
      name: "Alice",
      lc_use: "assert",
      groups: [`teams-tenant-${TENANT}`],
      token: { agents: ["helper"], overrides: ["interactive"] },
    });
    expect(lc.runs[0]!.token).toMatch(new RegExp(`^tok-teams:${ALICE}-`));
    // The bridge registers nobody and mints nothing: loomcycle does.
    expect(lc.createdUsers).toEqual([]);
    expect(lc.mints).toEqual([]);

    await message({ id: "19:g@thread.v2", conversationType: "groupChat", tenantId: TENANT });
    const g = gate.asserts.at(-1)!;
    expect(g.payload.sub).toBe(conversationSubject(TENANT, "19:g@thread.v2", "teams").slice("teams:".length));
    expect(g.payload.groups).toEqual([`teams-tenant-${TENANT}`]);
  });

  it("revokes a person who uninstalls the app from their own chat, and nothing else", async () => {
    await start();
    // Acknowledged only once loomcycle has the revoke.
    expect((await uninstall()).status).toBe(200);
    expect(gate.revokes).toHaveLength(1);
    expect(gate.revokes[0]).toMatchObject({ gate: "teams", payload: { iss: "teams", sub: ALICE, lc_use: "revoke" } });

    // An install, an uninstall from a group chat or a team, and one from a
    // tenant outside the fence are not a person leaving the bot.
    await post({ type: "installationUpdate", action: "add" });
    await uninstall({ conversation: { id: "19:g@thread.v2", conversationType: "groupChat", tenantId: TENANT } });
    await uninstall({ conversation: { id: "19:c@thread.tacv2", conversationType: "channel", tenantId: TENANT } });
    await uninstall({ conversation: { id: "a:x", conversationType: "personal", tenantId: OTHER_TENANT }, channelData: { tenant: { id: OTHER_TENANT } } });
    await settle();
    expect(gate.revokes).toHaveLength(1);
    expect(lc.calls).toEqual([]);
  });

  // Revocation must not fail open: a revoke loomcycle did not take is answered
  // with a 5xx, so the Bot Connector redelivers the uninstall.
  it("answers a failed revoke with a 5xx, logging only the status", async () => {
    const log = new RecordingLogger();
    gate.revokeStatus = 500;
    await start({ logger: log });
    expect((await uninstall()).status).toBe(500);
    expect(gate.revokes).toHaveLength(1);
    expect(log.lines.find((l) => l.msg === "teams revoke failed")).toEqual({ level: "warn", msg: "teams revoke failed", attrs: { status: 500 } });

    // The redelivery, once loomcycle takes it, is acknowledged.
    gate.revokeStatus = 204;
    expect((await uninstall()).status).toBe(200);
    expect(gate.revokes).toHaveLength(2);
  });

  it("with a loomcycle token, ignores an uninstall as it always has", async () => {
    await start({ asGate: false });
    expect((await uninstall()).status).toBe(200);
    await settle();
    expect(gate.revokes).toEqual([]);
    expect(lc.calls).toEqual([]);
  });

  it("with a loomcycle token, registers and mints for the subjects it always used", async () => {
    await start({ asGate: false });
    await message({ id: "a:personal", conversationType: "personal", tenantId: TENANT });
    expect(gate.asserts).toEqual([]);
    expect(lc.createdUsers.map((u) => u.subject)).toEqual([`teams-${ALICE}`]);
  });
});
