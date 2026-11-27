// A question the agent asked survives a bridge restart (RFC HG §4.3): it is
// saved in the conversation record, and the first message or card press of
// the next process checks it against the run.

import { mkdtemp, rm, writeFile } from "node:fs/promises";
import os from "node:os";
import path from "node:path";

import type { AgentEvent } from "@loomcycle/client";
import { afterEach, beforeEach, describe, expect, it } from "vitest";

import type { CanonicalCard } from "../src/cards.js";
import { afterQuestion, isStoredQuestion } from "../src/conversation.js";
import { type Identified } from "../src/identity.js";
import { ConversationRouter } from "../src/router.js";
import { FileStore, MemoryStore, type ConversationRecord, type ConversationStore } from "../src/store.js";
import { TokenBroker } from "../src/tokens.js";
import { EventChannel, FakeLoomcycle, FakePlatform, settle, silentLogger, waitFor, type FakeRef } from "./fakes.js";

const alice = (): Identified => ({
  ok: true,
  speaker: { subject: "test-alice", name: "Alice", trust: "trusted" },
  scope: "personal",
  conversationId: "conv-1",
  runAs: { subject: "test-alice", name: "Alice" },
});
const ref: FakeRef = { conversation: "conv-1" };

describe("a question that survives a bridge restart (RFC HG)", () => {
  let lc: FakeLoomcycle;
  let platform: FakePlatform;
  let dir: string;
  let file: string;

  /** process is one bridge process: a router over a store of its own, as a
   *  restart builds them anew. */
  function bridgeProcess(store: ConversationStore<FakeRef>) {
    const broker = new TokenBroker(lc.client("admin"), {
      agent: "helper",
      tokenName: "test-bridge",
      ttlSeconds: 43200,
      ratePerMinute: 6,
      rateBurst: 10,
      accessMode: "isolated",
    });
    return new ConversationRouter<FakeRef>(
      { idleMinutes: 60 },
      {
        platform: { name: "test", label: "Test" },
        capabilities: { streaming: "never", maxMessageChars: 20000 },
        agent: "helper",
        store,
        broker,
        clients: lc.client,
        port: platform,
        log: silentLogger,
        message: { typingEveryMs: 60_000 },
      },
    );
  }

  beforeEach(async () => {
    lc = new FakeLoomcycle();
    platform = new FakePlatform();
    dir = await mkdtemp(path.join(os.tmpdir(), "bridge-question-"));
    file = path.join(dir, "conversations.json");
  });
  afterEach(async () => {
    await rm(dir, { recursive: true, force: true });
  });

  /** ask starts a run in a first process and has it ask a question. */
  async function ask(store: ConversationStore<FakeRef>, options = ["Lisbon", "Porto", "Faro"], extra: Record<string, unknown> = {}) {
    const first = bridgeProcess(store);
    await first.onMessage(alice(), ref, "plan my trip");
    await settle();
    const run = lc.runs[0]!;
    run.events.push({ type: "session", session_id: "sess-1" });
    run.events.push({ type: "agent", run_id: "run-1", agent_id: "agent-1", session_id: "sess-1", parent_agent_id: null });
    await settle();
    run.events.push({
      type: "interruption_pending",
      interruption: { interrupt_id: "q1", kind: "question", question: "Where to?", options, priority: "normal", ...extra },
    });
    await waitFor(() => platform.cards().length === 1, "question card");
    await settle();
    const card = platform.cards()[0] as Extract<CanonicalCard, { kind: "question" }>;
    return { first, card };
  }

  /** restart ends the first process the way a shutdown does, and starts a
   *  second over a fresh store reading the same file. */
  async function restart(first: ConversationRouter<FakeRef>) {
    await first.stop();
    return bridgeProcess(new FileStore<FakeRef>(file));
  }

  /** replay feeds a re-attach what loomcycle's replay holds, then the run's
   *  answer after the question. */
  function replayThenAnswer(events: EventChannel<AgentEvent>) {
    events.push({ type: "agent", run_id: "run-1", agent_id: "agent-1", session_id: "sess-1", parent_agent_id: null });
    events.push({ type: "text", text: "Earlier turn, already shown." });
    events.push({ type: "awaiting_input" });
    events.push({ type: "interruption_pending", interruption: { interrupt_id: "q1", kind: "question", question: "Where to?", priority: "normal" } });
    events.push({ type: "text", text: "Booked Porto." });
    events.push({ type: "awaiting_input" });
  }

  it("saves the open question in the record, buttons included, and nothing secret", async () => {
    const store = new FileStore<FakeRef>(file);
    const { card } = await ask(store);
    const q = (await new FileStore<FakeRef>(file).get("conv-1"))?.question;
    expect(q).toMatchObject({ runId: "run-1", interruptId: "q1", options: ["Lisbon", "Porto", "Faro"], allowOther: false });
    expect(Object.keys(q!.buttons).sort()).toEqual(card.buttons.map((b) => b.id).sort());
    expect(JSON.stringify(q)).not.toMatch(/tok-/);
  });

  it("restores a still-pending question: a typed number answers it and the run's answer is shown", async () => {
    const { first } = await ask(new FileStore<FakeRef>(file));
    lc.pendingInterrupts["run-1"] = ["q1"];
    const second = await restart(first);

    await second.onMessage(alice(), ref, "2");
    await settle();

    expect(lc.callsTo("listRunInterrupts").map((c) => c.args)).toEqual([["run-1", { status: "pending" }]]);
    expect(lc.callsTo("resolveInterrupt").map((c) => c.args)).toEqual([["run-1", "q1", { answer: "Porto", resolvedBy: "test-alice" }]]);
    // The session is not continued in a new run: the old one is still ours.
    expect(lc.runs).toHaveLength(1);
    expect(lc.callsTo("cancelAgent")).toHaveLength(0);

    expect(lc.reattaches.map((r) => r.runId)).toEqual(["run-1"]);
    replayThenAnswer(lc.reattaches[0]!.events);
    await waitFor(() => platform.messages().includes("Booked Porto."), "the answer after the question");
    expect(platform.messages()).not.toContain("Earlier turn, already shown.");
    expect(platform.cards()).toHaveLength(1);
    expect((await new FileStore<FakeRef>(file).get("conv-1"))?.question).toBeUndefined();
  });

  it("restores a still-pending question's buttons for a press after the restart", async () => {
    const { first, card } = await ask(new FileStore<FakeRef>(file));
    lc.pendingInterrupts["run-1"] = ["q1"];
    const second = await restart(first);

    const faro = card.buttons.find((b) => b.label === "Faro")!;
    expect(await second.onCardAction(alice(), ref, { buttonId: faro.id })).toEqual({ card: { kind: "result", message: "Answered: Faro" } });
    expect(lc.callsTo("resolveInterrupt").map((c) => c.args)).toEqual([["run-1", "q1", { answer: "Faro", resolvedBy: "test-alice" }]]);
  });

  it("forgets a question the run no longer waits on, and the reply goes on as an ordinary message", async () => {
    const { first } = await ask(new FileStore<FakeRef>(file));
    const second = await restart(first);

    await second.onMessage(alice(), ref, "2");
    await settle();

    expect(lc.callsTo("resolveInterrupt")).toHaveLength(0);
    expect(lc.reattaches).toHaveLength(0);
    // As before questions were saved: the left run is ended and the session
    // continued with the message.
    expect(lc.callsTo("cancelAgent").map((c) => c.args[0])).toEqual(["agent-1"]);
    expect(lc.runs).toHaveLength(2);
    expect(lc.runs[1]!.kind).toBe("continue");
    expect(lc.runs[1]!.opts.segments).toEqual([{ role: "user", content: [{ type: "trusted-text", text: "2" }] }]);
    expect((await new FileStore<FakeRef>(file).get("conv-1"))?.question).toBeUndefined();
  });

  it("forgets a saved question a press cannot reach any more", async () => {
    const { first, card } = await ask(new FileStore<FakeRef>(file));
    const second = await restart(first);
    expect(await second.onCardAction(alice(), ref, { buttonId: card.buttons[0]!.id })).toEqual({ card: { kind: "result", message: "This is no longer open." } });
    expect(lc.callsTo("resolveInterrupt")).toHaveLength(0);
    expect((await new FileStore<FakeRef>(file).get("conv-1"))?.question).toBeUndefined();
  });

  it("does not restore with a client that has no restart calls", async () => {
    const { first } = await ask(new FileStore<FakeRef>(file));
    lc.pendingInterrupts["run-1"] = ["q1"];
    lc.restartCalls = false;
    const second = await restart(first);
    await second.onMessage(alice(), ref, "2");
    await settle();
    expect(lc.callsTo("resolveInterrupt")).toHaveLength(0);
    expect(lc.runs).toHaveLength(2);
  });

  it("clears the saved question once it is answered, declined, or the run ends", async () => {
    const store = new FileStore<FakeRef>(file);
    const { first, card } = await ask(store);
    const saved = async () => (await new FileStore<FakeRef>(file).get("conv-1"))?.question;
    expect(await saved()).toBeDefined();
    await first.onCardAction(alice(), ref, { buttonId: card.buttons.find((b) => b.label === "Decline")!.id });
    expect(await saved()).toBeUndefined();

    lc.runs[0]!.events.push({
      type: "interruption_pending",
      interruption: { interrupt_id: "q2", kind: "question", question: "When?", options: ["now"], priority: "normal" },
    });
    await waitFor(() => platform.cards().length === 2, "second question");
    await settle();
    expect((await saved())?.interruptId).toBe("q2");
    lc.runs[0]!.events.end();
    await settle();
    await settle();
    expect(await saved()).toBeUndefined();
  });

  it("keeps nothing across a restart with the memory store", async () => {
    const { first } = await ask(new MemoryStore<FakeRef>());
    lc.pendingInterrupts["run-1"] = ["q1"];
    await first.stop();
    const second = bridgeProcess(new MemoryStore<FakeRef>());
    await second.onMessage(alice(), ref, "2");
    await settle();
    expect(lc.callsTo("listRunInterrupts")).toHaveLength(0);
    expect(lc.callsTo("resolveInterrupt")).toHaveLength(0);
  });

  it("loads a record written before questions were saved", async () => {
    const old: ConversationRecord<FakeRef> = {
      conversationId: "conv-1",
      scope: "personal",
      ref,
      ownerSubject: "test-alice",
      ownerName: "Alice",
      runSubject: "test-alice",
      runName: "Alice",
      sessionId: "sess-1",
      lastActivityAt: Date.now(),
    };
    await writeFile(file, JSON.stringify([old]));
    const router = bridgeProcess(new FileStore<FakeRef>(file));
    await router.onMessage(alice(), ref, "hello again");
    await settle();
    expect(lc.callsTo("listRunInterrupts")).toHaveLength(0);
    expect(lc.runs).toHaveLength(1);
    expect(lc.runs[0]!.kind).toBe("continue");
  });

  it("refuses a saved question whose buttons were edited to answer something not offered", () => {
    const q = {
      runId: "run-1",
      interruptId: "q1",
      options: ["yes"],
      allowOther: false,
      buttons: { b1: { verb: "lc.answer", runId: "run-1", interruptId: "q1", answer: "yes" } },
    };
    expect(isStoredQuestion(q)).toBe(true);
    expect(isStoredQuestion({ ...q, buttons: { b1: { ...q.buttons.b1, answer: "wire the money" } } })).toBe(false);
    expect(isStoredQuestion({ ...q, buttons: { b1: { ...q.buttons.b1, verb: "lc.approve" } } })).toBe(false);
    expect(isStoredQuestion({ ...q, buttons: { b1: { ...q.buttons.b1, runId: "run-2" } } })).toBe(false);
    expect(isStoredQuestion({ ...q, options: "yes" })).toBe(false);
    expect(isStoredQuestion(undefined)).toBe(false);
  });

  it("afterQuestion drops the replay up to the question and passes the run's own frames", async () => {
    const events = new EventChannel<AgentEvent>();
    replayThenAnswer(events);
    events.end();
    const seen: string[] = [];
    for await (const ev of afterQuestion(events, "q1")) seen.push(ev.type === "text" ? `text:${ev.text}` : ev.type);
    expect(seen).toEqual(["agent", "text:Booked Porto.", "awaiting_input"]);
  });
});
