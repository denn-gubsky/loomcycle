// What agent output must never do on Telegram: make Telegram fetch a link
// (a preview leaks the URL, query string and all, with no click), or carry
// markup of its own beyond what the converter means to produce.

import { VERB } from "@loomcycle/bridge-core";
import { describe, expect, it } from "vitest";

import { TelegramApi } from "../src/api.js";
import { renderCard } from "../src/cards.js";
import { markdownToHtml } from "../src/format.js";
import { TelegramPort } from "../src/port.js";
import { FakeTelegram, silentLogger, tgError, waitFor } from "./fakes.js";

const NO_PREVIEW = { is_disabled: true };
const TEXT_METHODS = new Set(["sendMessage", "editMessageText"]);

function port(tg: FakeTelegram, streaming = false) {
  const api = new TelegramApi({ token: "1:x", baseUrl: "https://api.telegram.org", fetch: tg.fetch, sleep: async () => {} });
  return new TelegramPort({ api, log: silentLogger, streaming, timing: { minIntervalMs: 5 } });
}

/** textCalls are every call that puts text in a chat. */
const textCalls = (tg: FakeTelegram) => tg.calls.filter((c) => TEXT_METHODS.has(c.method));

const LEAK = "see [this](https://attacker.example/c?secret=abc) and https://attacker.example/b?x=1";

describe("no link previews", () => {
  it("a message, its plain-text resend and a card all disable the preview", async () => {
    const tg = new FakeTelegram();
    tg.reply.sendMessage = (p, n) => (n === 2 && p.parse_mode ? tgError(400, "Bad Request: can't parse entities") : undefined);
    const p = port(tg);
    await p.send({ chatId: 42 }, { type: "message", text: LEAK });
    await p.send({ chatId: 42, threadId: 3 }, { type: "message", text: LEAK }); // refused HTML, resent plain
    await p.send({ chatId: 42 }, {
      type: "message",
      card: {
        kind: "question",
        runId: "r",
        interruptId: "i",
        question: LEAK,
        options: [],
        buttons: [{ id: "declAAAA", label: "Decline", action: { verb: VERB.decline, runId: "r", interruptId: "i" } }],
      },
    });
    await p.editMessage(42, 7, { html: markdownToHtml(LEAK), plain: LEAK });
    expect(textCalls(tg).length).toBe(5);
    for (const c of textCalls(tg)) expect(c.params.link_preview_options).toEqual(NO_PREVIEW);
  });

  it("a streamed answer disables the preview on its first send, every edit and every page", async () => {
    const tg = new FakeTelegram();
    const s = port(tg, true).asBridgePort().createStream!({ chatId: 42 });
    s.emit(LEAK);
    await waitFor(() => tg.callsTo("sendMessage").length === 1, "first send");
    s.emit(" more");
    await waitFor(() => tg.callsTo("editMessageText").length === 1, "edit");
    s.emit(`\n${"y".repeat(5000)}`);
    await s.close();
    expect(textCalls(tg).length).toBeGreaterThanOrEqual(4);
    for (const c of textCalls(tg)) expect(c.params.link_preview_options).toEqual(NO_PREVIEW);
  });
});

describe("no markup from agent text", () => {
  it("escapes a raw link, mention or Telegram tag the agent writes", () => {
    for (const raw of [
      '<a href="https://attacker.example/">click</a>',
      '<a href="tg://user?id=123">Alice</a>',
      '<tg-emoji emoji-id="1">x</tg-emoji>',
      '<span class="tg-spoiler">x</span>',
      "&lt;b&gt; stays escaped text",
    ]) {
      const html = markdownToHtml(raw);
      expect(html).not.toMatch(/<(a|tg-|span)/);
      expect(html.startsWith("&lt;") || html.startsWith("&amp;")).toBe(true);
    }
  });

  it("makes no tg:// link, so agent text cannot mention or point at a Telegram user or chat", () => {
    for (const md of ["[Alice](tg://user?id=123)", "[join](tg://join?invite=abc)", "[x](TG://user?id=1)", "[x](tg:user?id=1)"]) {
      expect(markdownToHtml(md)).not.toContain("<a ");
    }
    const card = renderCard({ kind: "result", message: "[Alice](tg://user?id=123)" });
    expect(card.html).not.toContain("<a ");
  });

  it("links only http and https", () => {
    expect(markdownToHtml("[x](https://e.com/a)")).toBe('<a href="https://e.com/a">x</a>');
    expect(markdownToHtml("[x](http://e.com/a)")).toBe('<a href="http://e.com/a">x</a>');
    for (const md of ["[x](mailto:a@b.c)", "[x](javascript:alert(1))", "[x](data:text/html,hi)", "[x](file:///etc/passwd)"]) {
      expect(markdownToHtml(md)).not.toContain("<a ");
    }
  });
});
