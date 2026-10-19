import { VERB } from "@loomcycle/bridge-core";
import { describe, expect, it } from "vitest";

import { TelegramApi } from "../src/api.js";
import { TelegramPort } from "../src/port.js";
import type { TelegramStream } from "../src/stream.js";
import { FakeTelegram, silentLogger, tgError, waitFor } from "./fakes.js";

function port(tg: FakeTelegram, streaming = false, allowedChats: ReadonlySet<string> = new Set()) {
  const api = new TelegramApi({ token: "1:x", baseUrl: "https://api.telegram.org", fetch: tg.fetch, sleep: async () => {} });
  return new TelegramPort({ api, log: silentLogger, streaming, trustedChats: new Set(), allowedChats, timing: { minIntervalMs: 20 } });
}

const topic = { chatId: -100500, threadId: 7 };

describe("port", () => {
  it("sends a message as HTML into the conversation's topic", async () => {
    const tg = new FakeTelegram();
    await port(tg).send(topic, { type: "message", text: "**hi** <there>" });
    expect(tg.callsTo("sendMessage")[0]!.params).toEqual({ chat_id: -100500, message_thread_id: 7, link_preview_options: { is_disabled: true }, text: "<b>hi</b> &lt;there&gt;", parse_mode: "HTML" });
  });

  it("resends as plain text when Telegram refuses the HTML", async () => {
    const tg = new FakeTelegram();
    tg.reply.sendMessage = (p) => (p.parse_mode ? tgError(400, "Bad Request: can't parse entities: unclosed tag") : undefined);
    await port(tg).send({ chatId: 42 }, { type: "message", text: "**hi**" });
    expect(tg.callsTo("sendMessage").map((c) => c.params)).toEqual([
      { chat_id: 42, link_preview_options: { is_disabled: true }, text: "<b>hi</b>", parse_mode: "HTML" },
      { chat_id: 42, link_preview_options: { is_disabled: true }, text: "**hi**" },
    ]);
  });

  it("does not hide other failures behind the plain-text resend", async () => {
    const tg = new FakeTelegram();
    tg.reply.sendMessage = () => tgError(403, "Forbidden: bot was blocked by the user");
    await expect(port(tg).send({ chatId: 42 }, { type: "message", text: "x" })).rejects.toMatchObject({ code: 403 });
    expect(tg.callsTo("sendMessage")).toHaveLength(1);
  });

  it("tells an allowed group that became a supergroup its new id, once, and never allows it", async () => {
    const tg = new FakeTelegram();
    const upgraded = () => tgError(400, "Bad Request: group chat was upgraded to a supergroup chat", { migrate_to_chat_id: -1009999 });
    tg.reply.sendMessage = (p) => (p.chat_id === -500 ? upgraded() : undefined);
    const p = port(tg, false, new Set(["-500"]));
    await expect(p.send({ chatId: -500 }, { type: "message", text: "x" })).rejects.toMatchObject({ migrateToChatId: -1009999 });
    await waitFor(() => tg.callsTo("sendMessage").length === 2, "upgrade notice");
    expect(tg.callsTo("sendMessage")[1]!.params).toEqual({
      chat_id: -1009999,
      link_preview_options: { is_disabled: true },
      text: "This group was upgraded, so its id changed to -1009999. Ask the operator to allow it.",
      parse_mode: "HTML",
    });
    // A second send into the old id: the error again, no second notice.
    await expect(p.send({ chatId: -500 }, { type: "message", text: "y" })).rejects.toMatchObject({ code: 400 });
    await new Promise((r) => setTimeout(r, 30));
    expect(tg.callsTo("sendMessage").map((c) => c.params.chat_id)).toEqual([-500, -1009999, -500]);
  });

  it("never writes into a new chat id because of a group that was not allowed", async () => {
    const tg = new FakeTelegram();
    tg.reply.sendMessage = () => tgError(400, "Bad Request: group chat was upgraded to a supergroup chat", { migrate_to_chat_id: -1009999 });
    await expect(port(tg, false, new Set(["-777"])).send({ chatId: -500 }, { type: "message", text: "x" })).rejects.toMatchObject({ code: 400 });
    await new Promise((r) => setTimeout(r, 30));
    expect(tg.callsTo("sendMessage")).toHaveLength(1);
  });

  it("sends typing as a chat action and never waits out a flood limit for it", async () => {
    const tg = new FakeTelegram();
    await port(tg).send(topic, { type: "typing" });
    expect(tg.callsTo("sendChatAction")[0]!.params).toEqual({ chat_id: -100500, message_thread_id: 7, action: "typing" });
  });

  it("sends a card with its inline keyboard", async () => {
    const tg = new FakeTelegram();
    await port(tg).send({ chatId: 42 }, {
      type: "message",
      card: {
        kind: "review",
        runId: "r",
        buttons: [{ id: "apprAAAA", label: "Approve", action: { verb: VERB.approve, runId: "r" } }],
      },
    });
    expect(tg.callsTo("sendMessage")[0]!.params.reply_markup).toEqual({ inline_keyboard: [[{ text: "Approve", callback_data: "apprAAAA" }]] });
  });

  it("offers the core a stream only when streaming is on", () => {
    const tg = new FakeTelegram();
    expect(port(tg).asBridgePort().createStream).toBeUndefined();
    expect(port(tg, true).asBridgePort().createStream).toBeTypeOf("function");
  });
});

const open = (tg: FakeTelegram) => port(tg, true).asBridgePort().createStream!({ chatId: 42 }) as TelegramStream;

describe("stream", () => {
  it("sends one message once text starts and edits it as it grows", async () => {
    const tg = new FakeTelegram();
    const s = open(tg);
    s.emit("Hello");
    await waitFor(() => tg.callsTo("sendMessage").length === 1, "first send");
    s.emit(" **there**");
    await waitFor(() => tg.callsTo("editMessageText").length === 1, "edit");
    s.emit(", Alice");
    await s.close();
    expect(tg.callsTo("sendMessage")).toHaveLength(1);
    expect(tg.callsTo("editMessageText").at(-1)!.params).toMatchObject({ chat_id: 42, message_id: 100, text: "Hello <b>there</b>, Alice", parse_mode: "HTML" });
    expect(tg.callsTo("sendChatAction")).toHaveLength(1);
  });

  it("coalesces a burst of text into one update", async () => {
    const tg = new FakeTelegram();
    const s = open(tg);
    for (const w of ["a", "b", "c", "d"]) s.emit(w);
    await waitFor(() => tg.callsTo("sendMessage").length === 1, "first send");
    await s.close();
    expect(tg.callsTo("sendMessage")[0]!.params.text).toBe("abcd");
    expect(tg.callsTo("editMessageText")).toHaveLength(0);
  });

  it("keeps edits at least the minimum interval apart", async () => {
    const tg = new FakeTelegram();
    const at: number[] = [];
    tg.reply.editMessageText = () => {
      at.push(Date.now());
      return undefined;
    };
    const s = open(tg);
    for (let i = 0; i < 8; i++) {
      s.emit(`w${i} `);
      await new Promise((r) => setTimeout(r, 6));
    }
    await s.close();
    expect(at.length).toBeGreaterThan(1);
    for (let i = 1; i < at.length - 1; i++) expect(at[i]! - at[i - 1]!).toBeGreaterThanOrEqual(18);
  });

  it("backs off for retry_after on a 429 instead of dropping the update", async () => {
    const tg = new FakeTelegram();
    tg.reply.editMessageText = (p) => (p.text === "slow down" ? tgError(429, "Too Many Requests", { retry_after: 0.05 }) : undefined);
    const s = open(tg);
    s.emit("slow");
    await waitFor(() => tg.callsTo("sendMessage").length === 1, "first send");
    const t = Date.now();
    s.emit(" down");
    await waitFor(() => tg.callsTo("editMessageText").length === 1, "refused edit");
    s.emit("!");
    await waitFor(() => tg.callsTo("editMessageText").some((c) => c.params.text === "slow down!"), "next edit");
    expect(Date.now() - t).toBeGreaterThanOrEqual(40);
    await s.close();
  });

  it("finishes a long answer page by page", async () => {
    const tg = new FakeTelegram();
    const s = open(tg);
    const line = "x".repeat(99) + "\n";
    for (let i = 0; i < 90; i++) s.emit(line); // 9,000 characters
    await s.close();
    const finals = tg.texts();
    expect(finals.length).toBe(3);
    for (const t of finals) expect(t.length).toBeLessThanOrEqual(4000);
    expect(finals.join("").replace(/\n/g, "")).toBe("x".repeat(99 * 90));
  });

  it("shows a tool update until the answer starts", async () => {
    const tg = new FakeTelegram();
    const s = open(tg);
    s.update("Using WebSearch…");
    await waitFor(() => tg.callsTo("sendMessage").some((c) => c.params.text === "<i>Using WebSearch…</i>"), "status");
    s.emit("Found it");
    s.update("Using Read…");
    await s.close();
    expect(tg.callsTo("editMessageText").at(-1)!.params.text).toBe("Found it");
    expect(tg.calls.filter((c) => String(c.params.text).includes("Using Read"))).toHaveLength(0);
  });

  it("treats 'message is not modified' as done", async () => {
    const tg = new FakeTelegram();
    tg.reply.editMessageText = () => tgError(400, "Bad Request: message is not modified");
    const s = open(tg);
    s.emit("same");
    await waitFor(() => tg.callsTo("sendMessage").length === 1, "first send");
    s.emit(" more");
    await expect(s.close()).resolves.toBeUndefined();
  });
});
