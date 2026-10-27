// The whole path: a webhook POST → the bridge → a fake loomcycle → the reply
// the bridge sends to a fake Bot API.

import net from "node:net";

import { gateSharedSubject, keyFromSecret, MemoryStore, type Bridge } from "@loomcycle/bridge-core";
import { afterEach, beforeEach, describe, expect, it } from "vitest";

import { createTelegramBridge } from "../src/bridge.js";
import { loadConfig } from "../src/config.js";
import { NOT_ALLOWED_NOTICE, type TelegramRef, type TgUpdate } from "../src/identity.js";
import { FakeGate, FakeLoomcycle, FakeTelegram, settle, silentLogger, waitFor, type StartedRun } from "./fakes.js";

const SECRET = "hook_secret-1";

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

const alice = { id: 42, is_bot: false, first_name: "Alice" };
const bob = { id: 43, is_bot: false, first_name: "Bob" };
const privateChat = (u = alice) => ({ id: u.id, type: "private" });
const group = { id: -100500, type: "supergroup", title: "Team" };

describe("telegram bridge", () => {
  let lc: FakeLoomcycle;
  let tg: FakeTelegram;
  let gate: FakeGate;
  let bridge: Bridge<TelegramRef>;
  let base: string;
  let nextUpdate = 1;

  async function start(env: Record<string, string> = {}) {
    const port = await freePort();
    base = `http://127.0.0.1:${port}`;
    const cfg = loadConfig({
      LOOMCYCLE_BASE_URL: "http://loomcycle.test",
      BRIDGE_GATE: "telegram",
      BRIDGE_GATE_AUDIENCE: "https://lc.example",
      BRIDGE_GATE_SECRET: "s".repeat(40),
      BRIDGE_AGENT: "helper",
      TELEGRAM_BOT_TOKEN: "123:abc",
      TELEGRAM_BRIDGE_WEBHOOK_SECRET: SECRET,
      TELEGRAM_BRIDGE_ALLOWED_USERS: "42",
      TELEGRAM_BRIDGE_ALLOWED_CHATS: "-100500",
      PORT: String(port),
      ...env,
    });
    bridge = createTelegramBridge(cfg, {
      clients: lc.client,
      fetch: tg.fetch,
      gateFetch: gate.fetch,
      gateKey: keyFromSecret("s".repeat(40)),
      sleep: async () => {},
      logger: silentLogger,
      store: new MemoryStore(),
      timing: { minIntervalMs: 5 },
      message: { typingEveryMs: 60_000 },
      stream: { stopPollMs: 5 },
    });
    await bridge.start();
  }

  beforeEach(() => {
    lc = new FakeLoomcycle();
    tg = new FakeTelegram();
    gate = new FakeGate();
  });

  afterEach(async () => {
    await bridge?.stop();
  });

  /** post delivers an update as Telegram would; secret null sends no header. */
  function post(update: object, secret: string | null = SECRET) {
    return fetch(`${base}/telegram/webhook`, {
      method: "POST",
      headers: { "content-type": "application/json", ...(secret !== null ? { "x-telegram-bot-api-secret-token": secret } : {}) },
      body: JSON.stringify(update),
    });
  }

  function message(text: string, from = alice, chat: object = privateChat(from), extra: object = {}): TgUpdate {
    return { update_id: nextUpdate++, message: { message_id: nextUpdate, chat, from, text, ...extra } } as TgUpdate;
  }

  /** announce plays a run's first events, as loomcycle streams them. */
  function announce(run: StartedRun, runId = `run-${lc.runs.length}`) {
    run.events.push({ type: "session", session_id: "sess-1" });
    run.events.push({ type: "agent", run_id: runId, agent_id: `agent-${runId}`, session_id: "sess-1", parent_agent_id: null });
  }

  async function sayAndStart(update: TgUpdate): Promise<StartedRun> {
    const before = lc.runs.length;
    const res = await post(update);
    expect(res.status).toBe(200);
    await waitFor(() => lc.runs.length > before, "run started");
    const run = lc.runs[lc.runs.length - 1]!;
    announce(run);
    await settle();
    return run;
  }

  it("refuses a request without the right secret before reading it", async () => {
    await start();
    for (const secret of [null, "wrong", `${SECRET}x`, SECRET.slice(1), ""]) {
      const res = await post(message("hello"), secret);
      expect(res.status).toBe(401);
      expect(await res.text()).toBe("");
    }
    // The secret twice: Node joins the two into one value, which never matches.
    const twice = new Headers({ "content-type": "application/json" });
    twice.append("x-telegram-bot-api-secret-token", SECRET);
    twice.append("x-telegram-bot-api-secret-token", SECRET);
    const res = await fetch(`${base}/telegram/webhook`, { method: "POST", headers: twice, body: JSON.stringify(message("hello")) });
    expect(res.status).toBe(401);
    await settle();
    expect(lc.calls).toHaveLength(0);
    expect(tg.calls.map((c) => c.method)).toEqual(["getMe"]);
  });

  it("answers a private message: a run as the user, then the reply in the chat", async () => {
    await start();
    const run = await sayAndStart(message("hello there"));
    expect(run.token).toMatch(/^tok-telegram:42-/);
    expect(run.opts).toMatchObject({ agent: "helper", interactive: true });
    expect(run.opts.segments).toEqual([{ role: "user", content: [{ type: "trusted-text", text: "hello there" }] }]);
    // The bridge is a loomcycle gate: it registers nobody and mints
    // nothing; it asserts who is talking and loomcycle issues the token.
    expect(lc.createdUsers).toEqual([]);
    expect(lc.mints).toEqual([]);
    expect(gate.asserts).toHaveLength(1);
    expect(gate.asserts[0]!.gate).toBe("telegram");
    expect(gate.asserts[0]!.payload).toMatchObject({ iss: "telegram", aud: "https://lc.example", sub: "42", name: "Alice", lc_use: "assert",
      token: { agents: ["helper"], overrides: ["interactive"] } });
    // No trusted chats: a person is asserted in no chat group.
    expect(gate.asserts[0]!.payload.groups).toEqual([]);

    run.events.push({ type: "text", text: "Hi **Alice**" });
    run.events.push({ type: "awaiting_input" });
    await waitFor(() => tg.callsTo("sendMessage").length === 1, "reply");
    expect(tg.callsTo("sendMessage")[0]!.params).toEqual({ chat_id: 42, link_preview_options: { is_disabled: true }, text: "Hi <b>Alice</b>", parse_mode: "HTML" });
    expect(tg.callsTo("sendChatAction")[0]!.params).toEqual({ chat_id: 42, action: "typing" });
  });

  it("handles a redelivered update once", async () => {
    await start();
    const u = message("hello");
    await sayAndStart(u);
    expect((await post(u)).status).toBe(200);
    await settle();
    expect(lc.runs).toHaveLength(1);
    expect(lc.callsTo("sendRunInput")).toHaveLength(0);
  });

  it("drops a bot's message before anything touches loomcycle", async () => {
    await start({ TELEGRAM_BRIDGE_ALLOW_ANYONE: "1" });
    const other = { id: 77, is_bot: true, first_name: "OtherBot" };
    expect((await post(message("ping", other, group, { entities: [{ type: "mention", offset: 0, length: 8 }] }))).status).toBe(200);
    expect((await post(message("ping", { ...tg.bot, is_bot: true }, privateChat(tg.bot)))).status).toBe(200);
    await settle();
    expect(lc.calls).toHaveLength(0);
    expect(tg.callsTo("sendMessage")).toHaveLength(0);
  });

  it("answers a non-text message in a private chat once per message, and stays silent in a group", async () => {
    await start();
    const media = (from: object, chat: object, content: object): TgUpdate =>
      ({ update_id: nextUpdate++, message: { message_id: nextUpdate, chat, from, ...content } }) as TgUpdate;
    await post(media(alice, privateChat(), { photo: [{ file_id: "p1", width: 1, height: 1 }], caption: "look" }));
    await post(media(alice, privateChat(), { voice: { file_id: "v1", duration: 1 } }));
    await post(media(alice, privateChat(), { sticker: { file_id: "s1" } }));
    await waitFor(() => tg.callsTo("sendMessage").length === 3, "three replies");
    for (const c of tg.callsTo("sendMessage")) {
      expect(c.params).toEqual({ chat_id: 42, link_preview_options: { is_disabled: true }, text: "I can only read text for now.", parse_mode: "HTML" });
    }
    // A group, even mentioning the bot in a caption, and a bot: nothing.
    await post(media(alice, group, { document: { file_id: "d1" }, caption: "@LoomBot", caption_entities: [{ type: "mention", offset: 0, length: 8 }] }));
    await post(media({ id: 77, is_bot: true, first_name: "OtherBot" }, privateChat({ id: 77, is_bot: true, first_name: "OtherBot" }), { video: { file_id: "x" } }));
    await settle();
    expect(tg.callsTo("sendMessage")).toHaveLength(3);
    expect(lc.calls).toHaveLength(0);
  });

  it("a stranger's non-text message gets only the usual refusal, once", async () => {
    await start();
    const stranger = { id: 66, is_bot: false, first_name: "Stranger" };
    const photo = (): TgUpdate =>
      ({ update_id: nextUpdate++, message: { message_id: nextUpdate, chat: privateChat(stranger), from: stranger, photo: [{ file_id: "p" }] } }) as TgUpdate;
    await post(photo());
    await post(photo());
    await waitFor(() => tg.callsTo("sendMessage").length === 1, "notice");
    await settle();
    expect(tg.texts()).toEqual([NOT_ALLOWED_NOTICE]);
    expect(lc.calls).toHaveLength(0);
  });

  it("on the upgrade service message, tells an allowed group its new id once, and never reaches loomcycle", async () => {
    await start({ TELEGRAM_BRIDGE_ALLOWED_CHATS: "-500,-100500" });
    const upgraded = (chatId: number, newId: number): TgUpdate =>
      ({ update_id: nextUpdate++, message: { message_id: nextUpdate, chat: { id: chatId, type: "group" }, from: alice, migrate_to_chat_id: newId } }) as TgUpdate;
    await post(upgraded(-500, -1009999));
    await waitFor(() => tg.callsTo("sendMessage").length === 1, "upgrade notice");
    expect(tg.callsTo("sendMessage")[0]!.params).toEqual({
      chat_id: -1009999,
      link_preview_options: { is_disabled: true },
      text: "This group was upgraded, so its id changed to -1009999. Ask the operator to allow it.",
      parse_mode: "HTML",
    });
    // Again for the same group, and for a group nobody allowed: nothing.
    await post(upgraded(-500, -1009999));
    await post(upgraded(-777, -1008888));
    await settle();
    expect(tg.callsTo("sendMessage")).toHaveLength(1);
    expect(lc.calls).toHaveLength(0);
  });

  it("tells a user outside the allow-list once, and never reaches loomcycle", async () => {
    await start();
    const stranger = { id: 66, is_bot: false, first_name: "Stranger" };
    await post(message("hi", stranger));
    await post(message("hi again", stranger));
    await waitFor(() => tg.callsTo("sendMessage").length === 1, "notice");
    await settle();
    expect(tg.callsTo("sendMessage")).toHaveLength(1);
    expect(tg.callsTo("sendMessage")[0]!.params.chat_id).toBe(66);
    expect(lc.calls).toHaveLength(0);
  });

  it("with GROUP_REQUIRE_USERS, a group member off the user allow-list is refused, for messages and buttons", async () => {
    await start({ TELEGRAM_BRIDGE_GROUP_REQUIRE_USERS: "1" });
    const stranger = { id: 66, is_bot: false, first_name: "Stranger" };
    const mention = { entities: [{ type: "mention", offset: 0, length: 8 }] };
    await post(message("@LoomBot hi", stranger, group, mention));
    await post(message("@LoomBot hi again", stranger, group, mention));
    await waitFor(() => tg.callsTo("sendMessage").length === 1, "notice");
    await post({ update_id: nextUpdate++, callback_query: { id: "cq-s", from: stranger, message: { message_id: 100, chat: group }, data: "anything" } });
    await waitFor(() => tg.callsTo("answerCallbackQuery").length === 1, "press answered");
    await settle();
    expect(tg.callsTo("sendMessage")).toHaveLength(1);
    expect(tg.callsTo("sendMessage")[0]!.params.chat_id).toBe(-100500);
    expect(tg.callsTo("answerCallbackQuery")[0]!.params.text).toBe(tg.callsTo("sendMessage")[0]!.params.text);
    expect(lc.calls).toHaveLength(0);
    // A listed user in the same group still reaches the agent.
    await sayAndStart(message("@LoomBot hello", alice, group, mention));
  });

  it("in a group, runs as the group and sends the speaker's words as untrusted", async () => {
    await start();
    const mention = { entities: [{ type: "mention", offset: 0, length: 8 }] };
    const run = await sayAndStart(message("@LoomBot summarise this thread", alice, group, mention));
    expect(run.token).toContain(gateSharedSubject("telegram", "-100500"));
    expect(run.opts.segments).toEqual([{ role: "user", content: [{ type: "untrusted-block", kind: "user_input", text: "[Alice] summarise this thread" }] }]);
    // Chatter not addressed to the bot is not read at all.
    await post(message("unrelated chatter", bob, group));
    await settle();
    expect(lc.runs).toHaveLength(1);
    expect(lc.callsTo("sendRunInput")).toHaveLength(0);
  });

  it("a trusted group's speakers are trusted, and a topic's reply goes into the topic", async () => {
    await start({ TELEGRAM_BRIDGE_TRUSTED_CHATS: "-100500" });
    const inTopic = { is_topic_message: true, message_thread_id: 7, entities: [{ type: "mention", offset: 0, length: 8 }] };
    const run = await sayAndStart(message("@LoomBot hello", alice, { ...group, is_forum: true }, inTopic));
    expect(run.opts.segments).toEqual([{ role: "user", content: [{ type: "trusted-text", text: "[Alice] hello" }] }]);
    run.events.push({ type: "text", text: "hi all" });
    run.events.push({ type: "awaiting_input" });
    await waitFor(() => tg.callsTo("sendMessage").length === 1, "reply");
    expect(tg.callsTo("sendMessage")[0]!.params).toMatchObject({ chat_id: -100500, message_thread_id: 7, text: "hi all" });
  });

  it("takes a command addressed to the bot by name in a group", async () => {
    await start();
    await post(message("/help@LoomBot", alice, group));
    await waitFor(() => tg.callsTo("sendMessage").length === 1, "help");
    expect(String(tg.callsTo("sendMessage")[0]!.params.text)).toContain("<b>/new</b>");
    await post(message("/help@OtherBot", alice, group));
    await settle();
    expect(tg.callsTo("sendMessage")).toHaveLength(1);
    expect(lc.runs).toHaveLength(0);
  });

  it("asks a question with buttons, and a press answers it and closes the card", async () => {
    await start();
    const run = await sayAndStart(message("pick a file"));
    run.events.push({ type: "interruption_pending", interruption: { interrupt_id: "int-1", kind: "question", priority: "normal", question: "Which file?", options: ["a.txt", "b.txt"] } });
    await waitFor(() => tg.callsTo("sendMessage").length === 1, "card");
    const card = tg.callsTo("sendMessage")[0]!.params;
    const rows = (card.reply_markup as { inline_keyboard: Array<Array<{ text: string; callback_data: string }>> }).inline_keyboard;
    expect(rows.map((r) => r[0]!.text)).toEqual(["a.txt", "b.txt", "Decline"]);
    const pick = rows[1]![0]!.callback_data;

    // Someone else's press, forged data and the owner's press.
    await post({ update_id: nextUpdate++, callback_query: { id: "cq-1", from: { id: 42, first_name: "Alice" }, message: { message_id: 100, chat: privateChat() }, data: "forged00" } });
    await post({ update_id: nextUpdate++, callback_query: { id: "cq-2", from: { id: 42, first_name: "Alice" }, message: { message_id: 100, chat: privateChat() }, data: pick } });
    await waitFor(() => tg.callsTo("answerCallbackQuery").length === 2, "callbacks answered");
    await waitFor(() => tg.callsTo("editMessageText").length === 2, "cards closed");

    expect(lc.callsTo("resolveInterrupt").map((c) => c.args)).toEqual([["run-1", "int-1", { answer: "b.txt", resolvedBy: "telegram:42" }]]);
    const edits = tg.callsTo("editMessageText").map((c) => c.params);
    expect(edits).toContainEqual({ chat_id: 42, message_id: 100, link_preview_options: { is_disabled: true }, text: "Answered: b.txt", parse_mode: "HTML" });
    for (const e of edits) expect(e.reply_markup).toBeUndefined();
  });

  it("a free-text question is answered by the owner's reply", async () => {
    await start();
    const run = await sayAndStart(message("ask me"));
    run.events.push({ type: "interruption_pending", interruption: { interrupt_id: "int-2", kind: "question", priority: "normal", question: "Your name?" } });
    await waitFor(() => tg.callsTo("sendMessage").length === 2, "card and prompt");
    const [card, prompt] = tg.callsTo("sendMessage").map((c) => c.params.reply_markup);
    expect(card).toEqual({ inline_keyboard: [[{ text: "Decline", callback_data: expect.any(String) }]] });
    expect(prompt).toEqual({ force_reply: true });

    await post(message("Alice Smith"));
    await waitFor(() => lc.callsTo("resolveInterrupt").length === 1, "answer");
    expect(lc.callsTo("resolveInterrupt")[0]!.args).toEqual(["run-1", "int-2", { answer: "Alice Smith", resolvedBy: "telegram:42" }]);
  });

  it("in an untrusted group, a free-text question offers only Decline and no reply prompt", async () => {
    const mention = { entities: [{ type: "mention", offset: 0, length: 8 }] };
    const ask = { interrupt_id: "int-3", kind: "question", priority: "normal", question: "Your name?" };
    type Markup = { inline_keyboard: Array<Array<{ text: string }>>; force_reply?: boolean };

    await start();
    let run = await sayAndStart(message("@LoomBot ask me", alice, group, mention));
    run.events.push({ type: "interruption_pending", interruption: ask });
    await waitFor(() => tg.callsTo("sendMessage").length === 1, "untrusted card");
    let card = tg.callsTo("sendMessage")[0]!.params;
    expect(String(card.text)).toContain("Only the buttons can answer here.");
    expect((card.reply_markup as Markup).force_reply).toBeUndefined();
    expect((card.reply_markup as Markup).inline_keyboard.map((r) => r[0]!.text)).toEqual(["Decline"]);
    await settle();
    expect(tg.callsTo("sendMessage")).toHaveLength(1);
  });

  it("in a trusted group, a free-text question prompts its owner for a reply, and a reply to the card or the prompt answers", async () => {
    await start({ TELEGRAM_BRIDGE_TRUSTED_CHATS: "-100500" });
    const mention = { entities: [{ type: "mention", offset: 0, length: 8 }] };
    const ask = (id: string) => ({ interrupt_id: id, kind: "question", priority: "normal", question: "Your name?" });
    const first = message("@LoomBot ask me", alice, group, mention);
    const run = await sayAndStart(first);
    // Someone else speaks after the owner: the prompt still aims at the owner.
    await post(message("@LoomBot me too", bob, group, mention));
    await waitFor(() => lc.callsTo("sendRunInput").length === 1, "bob's input");

    run.events.push({ type: "interruption_pending", interruption: ask("int-4") });
    await waitFor(() => tg.callsTo("sendMessage").length === 2, "card and prompt");
    const [card, prompt] = tg.callsTo("sendMessage").map((c) => c.params);
    expect(String(card!.text)).not.toContain("Only the buttons can answer here.");
    expect(card!.reply_markup).toEqual({ inline_keyboard: [[{ text: "Decline", callback_data: expect.any(String) }]] });
    expect(prompt).toEqual({
      chat_id: -100500,
      link_preview_options: { is_disabled: true },
      reply_markup: { force_reply: true, selective: true },
      reply_parameters: { message_id: first.message!.message_id, allow_sending_without_reply: true },
      text: "Reply to this message with your answer.",
      parse_mode: "HTML",
    });

    // A reply to the prompt (the bot's message) answers.
    const toBot = { reply_to_message: { from: tg.bot } };
    await post(message("Alice Smith", alice, group, toBot));
    await waitFor(() => lc.callsTo("resolveInterrupt").length === 1, "answer via the prompt");
    expect(lc.callsTo("resolveInterrupt")[0]!.args).toEqual(["run-1", "int-4", { answer: "Alice Smith", resolvedBy: "telegram:42" }]);

    // So does a reply to the card, which is the bot's message too.
    run.events.push({ type: "interruption_pending", interruption: ask("int-5") });
    await waitFor(() => tg.callsTo("sendMessage").length === 4, "second card and prompt");
    await post(message("Smith", alice, group, toBot));
    await waitFor(() => lc.callsTo("resolveInterrupt").length === 2, "answer via the card");
    expect(lc.callsTo("resolveInterrupt")[1]!.args).toEqual(["run-1", "int-5", { answer: "Smith", resolvedBy: "telegram:42" }]);
  });

  it("a press by someone other than the card's owner is refused in the callback answer", async () => {
    await start({ TELEGRAM_BRIDGE_TRUSTED_CHATS: "-100500" });
    const mention = { entities: [{ type: "mention", offset: 0, length: 8 }] };
    const run = await sayAndStart(message("@LoomBot approve flow", alice, group, mention));
    run.events.push({ type: "awaiting_review", awaiting_review: { round: 1 } });
    await waitFor(() => tg.callsTo("sendMessage").length === 1, "review card");
    const rows = (tg.callsTo("sendMessage")[0]!.params.reply_markup as { inline_keyboard: Array<Array<{ callback_data: string }>> }).inline_keyboard;
    await post({ update_id: nextUpdate++, callback_query: { id: "cq-b", from: bob, message: { message_id: 100, chat: group }, data: rows[0]![0]!.callback_data } });
    await waitFor(() => tg.callsTo("answerCallbackQuery").length === 1, "answered");
    expect(tg.callsTo("answerCallbackQuery")[0]!.params).toEqual({ callback_query_id: "cq-b", text: "Only Alice can answer this." });
    expect(lc.callsTo("reviewRun")).toHaveLength(0);
    expect(tg.callsTo("editMessageText")).toHaveLength(0);
  });

  it("streams a private answer into one edited message when streaming is on", async () => {
    await start({ TELEGRAM_BRIDGE_STREAMING: "1" });
    const run = await sayAndStart(message("write"));
    run.events.push({ type: "text", text: "Once upon " });
    await waitFor(() => tg.callsTo("sendMessage").length === 1, "first send");
    run.events.push({ type: "text", text: "a time" });
    run.events.push({ type: "awaiting_input" });
    await waitFor(() => tg.callsTo("editMessageText").some((c) => c.params.text === "Once upon a time"), "final edit");
    expect(tg.callsTo("sendMessage")).toHaveLength(1);
  });

  // RFC FA §3.7d: the bridge tells loomcycle the groups it knows — a group's
  // own user is in its chat group; a person is in the groups of the trusted
  // chats Telegram says they belong to, and their admins groups.
  it("asserts a group's chat group and a person's trusted-chat memberships", async () => {
    tg.reply.getChatMember = (p) =>
      ({ ok: true, result: { status: p.chat_id === "-100500" ? "administrator" : p.chat_id === "-100600" ? "member" : "left" } }) as object;
    await start({ TELEGRAM_BRIDGE_TRUSTED_CHATS: "-100500,-100600,-100700", TELEGRAM_BRIDGE_ALLOWED_CHATS: "-100500" });
    await sayAndStart(message("hello"));
    expect(gate.asserts.at(-1)!.payload.groups).toEqual(["tg-chat--100500", "tg-chat--100500-admins", "tg-chat--100600"]);
    await sayAndStart(message("@LoomBot hi", alice, group, { entities: [{ type: "mention", offset: 0, length: 8 }] }));
    const g = gate.asserts.at(-1)!;
    expect(g.payload.sub).toBe(gateSharedSubject("telegram", "-100500").slice("telegram:".length));
    expect(g.payload.groups).toEqual(["tg-chat--100500"]);
  });

  it("asserts no groups for a person when a membership lookup fails", async () => {
    tg.reply.getChatMember = () => ({ ok: false, error_code: 400, description: "chat not found" }) as object;
    await start({ TELEGRAM_BRIDGE_TRUSTED_CHATS: "-100500" });
    await sayAndStart(message("hello"));
    expect("groups" in gate.asserts.at(-1)!.payload).toBe(false);
  });

  // A person who blocks the bot is gone as far as the gate knows: loomcycle
  // is told (a signed revoke) and disables them.
  it("revokes a person who blocks the bot in their private chat", async () => {
    await start();
    await post({ update_id: nextUpdate++, my_chat_member: { chat: privateChat(), from: alice, new_chat_member: { status: "kicked" } } });
    await waitFor(() => gate.revokes.length === 1, "revoke");
    expect(gate.revokes[0]).toMatchObject({ gate: "telegram", payload: { sub: "42", lc_use: "revoke" } });
    // A group's membership change is not a person leaving the bot.
    await post({ update_id: nextUpdate++, my_chat_member: { chat: group, from: alice, new_chat_member: { status: "kicked" } } });
    await settle();
    expect(gate.revokes).toHaveLength(1);
  });

  it("sets the webhook with its secret and the updates it handles", async () => {
    await start({ TELEGRAM_BRIDGE_SET_WEBHOOK: "1", TELEGRAM_BRIDGE_PUBLIC_URL: "https://bot.example.com" });
    expect(tg.callsTo("setWebhook")[0]!.params).toEqual({
      url: "https://bot.example.com/telegram/webhook",
      secret_token: SECRET,
      allowed_updates: ["message", "callback_query", "my_chat_member"],
    });
  });
});
