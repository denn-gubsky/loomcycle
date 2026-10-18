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

  it("treats only http and https as links", () => {
    expect(markdownToHtml("[x](https://e.com/a)")).toBe("x (https://e.com/a)");
    expect(markdownToHtml("[x](http://e.com/a)")).toBe("x (http://e.com/a)");
    for (const md of ["[x](mailto:a@b.c)", "[x](javascript:alert(1))", "[x](data:text/html,hi)", "[x](file:///etc/passwd)"]) {
      expect(markdownToHtml(md)).not.toContain("<a ");
    }
  });
});

/** shown is what a reader sees: the text with tags removed and entities read. */
const shown = (html: string) =>
  html.replace(/<[^>]+>/g, "").replace(/&lt;/g, "<").replace(/&gt;/g, ">").replace(/&quot;/g, '"').replace(/&amp;/g, "&");

describe("no link spoofing", () => {
  it("never lets a label hide a link's target: the target is always shown", () => {
    const cases: Array<[string, string]> = [
      ["https://your-bank.example/login", "https://attacker.example/phish?u=1"],
      ["your bank", "https://attacker.example/a_b_c/_x_"],
      ["**Click** here", "http://attacker.example/?a=1&b=2"],
      ["https://attacker.example/same", "https://attacker.example/same"],
    ];
    for (const [label, target] of cases) {
      const html = markdownToHtml(`see [${label}](${target}) now`);
      expect(html).not.toMatch(/<a[\s>]/);
      expect(shown(html)).toContain(target);
    }
  });

  it("renders a link as its label and its URL, or the URL alone when they match", () => {
    expect(markdownToHtml("[docs](https://e.com/a?b=1&c=2)")).toBe("docs (https://e.com/a?b=1&amp;c=2)");
    expect(markdownToHtml("[https://e.com/x](https://e.com/x)")).toBe("https://e.com/x");
  });

  it("shows a bare URL exactly as written, untouched by emphasis", () => {
    const url = "https://e.com/_x_/**y**";
    expect(shown(markdownToHtml(`go to ${url} now`))).toContain(url);
  });

  it("makes no link from any mix of markers", () => {
    const parts = ["[label]", "(https://e.com/x)", "[", "](", ")", "**", "_", "`", " ", '<a href="x">', "\n"];
    let seed = 11;
    const rand = () => (seed = (seed * 1103515245 + 12345) % 2 ** 31) / 2 ** 31;
    for (let n = 0; n < 2000; n++) {
      const s = Array.from({ length: 1 + Math.floor(rand() * 12) }, () => parts[Math.floor(rand() * parts.length)]).join("");
      expect(markdownToHtml(s)).not.toMatch(/<a[\s>]/);
    }
  });
});
