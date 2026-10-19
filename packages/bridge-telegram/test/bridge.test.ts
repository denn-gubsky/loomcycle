// The whole path: a webhook POST → the bridge → a fake loomcycle → the reply
// the bridge sends to a fake Bot API.

import net from "node:net";

import { MemoryStore, sharedSubject, type Bridge } from "@loomcycle/bridge-core";
import { afterEach, beforeEach, describe, expect, it } from "vitest";

import { createTelegramBridge } from "../src/bridge.js";
import { loadConfig } from "../src/config.js";
import { NOT_ALLOWED_NOTICE, type TelegramRef, type TgUpdate } from "../src/identity.js";
import { FakeLoomcycle, FakeTelegram, settle, silentLogger, waitFor, type StartedRun } from "./fakes.js";

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
  let bridge: Bridge<TelegramRef>;
  let base: string;
  let nextUpdate = 1;

  async function start(env: Record<string, string> = {}) {
    const port = await freePort();
    base = `http://127.0.0.1:${port}`;
    const cfg = loadConfig({
      LOOMCYCLE_BASE_URL: "http://loomcycle.test",
      LOOMCYCLE_TOKEN: "tenant-token",
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
    expect(run.token).toMatch(/^tok-telegram-42-/);
    expect(run.opts).toMatchObject({ agent: "helper", interactive: true });
    expect(run.opts.segments).toEqual([{ role: "user", content: [{ type: "trusted-text", text: "hello there" }] }]);
    expect(lc.createdUsers).toEqual([{ subject: "telegram-42", display_name: "Alice", access_mode: "isolated" }]);
    expect(lc.mints[0]!.opts.name).toBe("telegram-bridge");

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
    expect(run.token).toContain(sharedSubject("telegram", "-100500"));
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

    expect(lc.callsTo("resolveInterrupt").map((c) => c.args)).toEqual([["run-1", "int-1", { answer: "b.txt", resolvedBy: "telegram-42" }]]);
    const edits = tg.callsTo("editMessageText").map((c) => c.params);
    expect(edits).toContainEqual({ chat_id: 42, message_id: 100, link_preview_options: { is_disabled: true }, text: "Answered: b.txt", parse_mode: "HTML" });
    for (const e of edits) expect(e.reply_markup).toBeUndefined();
  });

  it("a free-text question is answered by the owner's reply", async () => {
    await start();
    const run = await sayAndStart(message("ask me"));
    run.events.push({ type: "interruption_pending", interruption: { interrupt_id: "int-2", kind: "question", priority: "normal", question: "Your name?" } });
    await waitFor(() => tg.callsTo("sendMessage").length === 1, "card");
    const markup = tg.callsTo("sendMessage")[0]!.params.reply_markup as { inline_keyboard: unknown[][]; force_reply?: boolean };
    expect(markup.inline_keyboard).toHaveLength(1);
    expect(markup.force_reply).toBe(true);

    await post(message("Alice Smith"));
    await waitFor(() => lc.callsTo("resolveInterrupt").length === 1, "answer");
    expect(lc.callsTo("resolveInterrupt")[0]!.args).toEqual(["run-1", "int-2", { answer: "Alice Smith", resolvedBy: "telegram-42" }]);
  });

  it("in an untrusted group, a free-text question offers only Decline and no reply box; a trusted group's is unchanged", async () => {
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
    await bridge.stop();

    lc = new FakeLoomcycle();
    tg = new FakeTelegram();
    await start({ TELEGRAM_BRIDGE_TRUSTED_CHATS: "-100500" });
    run = await sayAndStart(message("@LoomBot ask me", alice, group, mention));
    run.events.push({ type: "interruption_pending", interruption: ask });
    await waitFor(() => tg.callsTo("sendMessage").length === 1, "trusted card");
    card = tg.callsTo("sendMessage")[0]!.params;
    expect(String(card.text)).not.toContain("Only the buttons can answer here.");
    expect((card.reply_markup as Markup).force_reply).toBe(true);
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

  it("sets the webhook with its secret and the updates it handles", async () => {
    await start({ TELEGRAM_BRIDGE_SET_WEBHOOK: "1", TELEGRAM_BRIDGE_PUBLIC_URL: "https://bot.example.com" });
    expect(tg.callsTo("setWebhook")[0]!.params).toEqual({
      url: "https://bot.example.com/telegram/webhook",
      secret_token: SECRET,
      allowed_updates: ["message", "callback_query"],
    });
  });
});
