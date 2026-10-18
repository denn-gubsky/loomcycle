import { ConfigError, sharedSubject, VERB, type CanonicalCard, type RawRequest } from "@loomcycle/bridge-core";
import { describe, expect, it } from "vitest";

import { MAX_RETRY_AFTER_SECONDS, TelegramApi, TelegramError, isParseError } from "../src/api.js";
import { FREE_TEXT_HINT, parseCallbackData, renderCard } from "../src/cards.js";
import { loadConfig } from "../src/config.js";
import { markdownToHtml } from "../src/format.js";
import { addressedText, displayName, identify, NOT_ALLOWED_NOTICE, topicOf, type AccessPolicy, type TgChat, type TgMessage } from "../src/identity.js";
import { SeenSet, verifySecret } from "../src/verify.js";
import { FakeTelegram, tgError } from "./fakes.js";

const TOKEN = "123456:AAbbCC_dd-EE";
const SECRET = "s3cret_value-1";

const baseEnv = {
  LOOMCYCLE_BASE_URL: "http://loomcycle.internal:8787",
  LOOMCYCLE_TOKEN: "lc-tenant-token",
  BRIDGE_AGENT: "helper",
  TELEGRAM_BOT_TOKEN: TOKEN,
  TELEGRAM_BRIDGE_WEBHOOK_SECRET: SECRET,
  TELEGRAM_BRIDGE_ALLOWED_USERS: "42, 43",
};

function problems(env: Record<string, string | undefined>): string[] {
  try {
    loadConfig(env);
    return [];
  } catch (err) {
    if (err instanceof ConfigError) return err.problems;
    throw err;
  }
}

describe("config", () => {
  it("reads the bot's keys with Telegram's defaults", () => {
    const cfg = loadConfig(baseEnv);
    expect(cfg.port).toBe(8443);
    expect(cfg.apiBaseUrl).toBe("https://api.telegram.org");
    expect(cfg.streaming).toBe(false);
    expect([...cfg.allowedUsers]).toEqual(["42", "43"]);
    expect(cfg.allowAnyone).toBe(false);
    expect(cfg.setWebhook).toBe(false);
  });

  it("refuses to start open to anyone unless told so", () => {
    const env = { ...baseEnv, TELEGRAM_BRIDGE_ALLOWED_USERS: undefined };
    expect(problems(env).join("\n")).toMatch(/TELEGRAM_BRIDGE_ALLOW_ANYONE=1/);
    expect(loadConfig({ ...env, TELEGRAM_BRIDGE_ALLOW_ANYONE: "1" }).allowAnyone).toBe(true);
    expect(loadConfig({ ...env, TELEGRAM_BRIDGE_ALLOWED_CHATS: "-1001234567890" }).allowedChats.has("-1001234567890")).toBe(true);
  });

  it("holds the webhook secret to Telegram's rule", () => {
    expect(problems({ ...baseEnv, TELEGRAM_BRIDGE_WEBHOOK_SECRET: undefined })).toContain(
      "TELEGRAM_BRIDGE_WEBHOOK_SECRET is required (the webhook's secret_token)",
    );
    for (const bad of ["has space", "semi;colon", "a".repeat(257), "ünïcode"]) {
      expect(problems({ ...baseEnv, TELEGRAM_BRIDGE_WEBHOOK_SECRET: bad }).join()).toMatch(/1-256 characters/);
    }
    expect(problems({ ...baseEnv, TELEGRAM_BRIDGE_WEBHOOK_SECRET: "A".repeat(256) })).toEqual([]);
  });

  it("lets only BRIDGE_DEV leave the secret unset, and never when it sets the webhook", () => {
    const env = { ...baseEnv, TELEGRAM_BRIDGE_WEBHOOK_SECRET: undefined, BRIDGE_DEV: "1" };
    expect(problems(env)).toEqual([]);
    expect(
      problems({ ...env, TELEGRAM_BRIDGE_SET_WEBHOOK: "1", TELEGRAM_BRIDGE_PUBLIC_URL: "https://bot.example.com" }).join(),
    ).toMatch(/WEBHOOK_SECRET is required/);
  });

  it("refuses ids that are not numbers, and lists every problem at once", () => {
    const p = problems({ ...baseEnv, TELEGRAM_BRIDGE_ALLOWED_USERS: "42,@alice", TELEGRAM_BRIDGE_TRUSTED_CHATS: "x", BRIDGE_AGENT: undefined });
    expect(p).toEqual(
      expect.arrayContaining([
        "BRIDGE_AGENT is required",
        "TELEGRAM_BRIDGE_ALLOWED_USERS must be a comma list of numeric Telegram ids",
        "TELEGRAM_BRIDGE_TRUSTED_CHATS must be a comma list of numeric Telegram ids",
      ]),
    );
  });

  it("needs an https public URL to set the webhook", () => {
    expect(problems({ ...baseEnv, TELEGRAM_BRIDGE_SET_WEBHOOK: "1" })).toContain("TELEGRAM_BRIDGE_SET_WEBHOOK=1 needs TELEGRAM_BRIDGE_PUBLIC_URL");
    expect(problems({ ...baseEnv, TELEGRAM_BRIDGE_PUBLIC_URL: "http://bot.example.com" })).toContain(
      "TELEGRAM_BRIDGE_PUBLIC_URL must be an https:// URL",
    );
    expect(loadConfig({ ...baseEnv, TELEGRAM_BRIDGE_PUBLIC_URL: "https://bot.example.com/tg/" }).publicUrl).toBe("https://bot.example.com/tg");
  });

  it("sends the bot token over plain http only to this machine", () => {
    expect(problems({ ...baseEnv, TELEGRAM_BRIDGE_API_BASE_URL: "http://api.example.com" }).join()).toMatch(/http:\/\/ to localhost/);
    expect(loadConfig({ ...baseEnv, TELEGRAM_BRIDGE_API_BASE_URL: "http://127.0.0.1:8081/" }).apiBaseUrl).toBe("http://127.0.0.1:8081");
  });

  it("reads the streaming switch, refusing a typo rather than reading it as off", () => {
    expect(loadConfig({ ...baseEnv, TELEGRAM_BRIDGE_STREAMING: "1" }).streaming).toBe(true);
    expect(problems({ ...baseEnv, TELEGRAM_BRIDGE_STREAMING: "yes" })).toContain("TELEGRAM_BRIDGE_STREAMING must be 1 or 0");
  });

  it("never echoes the bot token in a problem", () => {
    const p = problems({ ...baseEnv, TELEGRAM_BOT_TOKEN: "not a token value" });
    expect(p).toContain("TELEGRAM_BOT_TOKEN does not look like a bot token (<id>:<secret>)");
    expect(p.join()).not.toContain("not a token value");
  });
});

function req(headers: Record<string, string | string[]>): RawRequest {
  return { method: "POST", path: "/telegram/webhook", query: new URLSearchParams(), headers, body: Buffer.from("{}") };
}

describe("webhook secret", () => {
  it("accepts only the exact secret in the header", () => {
    expect(verifySecret(req({ "x-telegram-bot-api-secret-token": SECRET }), SECRET)).toBe(true);
    expect(verifySecret(req({ "x-telegram-bot-api-secret-token": `${SECRET}x` }), SECRET)).toBe(false);
    expect(verifySecret(req({ "x-telegram-bot-api-secret-token": SECRET.slice(0, -1) }), SECRET)).toBe(false);
    expect(verifySecret(req({}), SECRET)).toBe(false);
  });

  it("refuses a repeated header, and refuses everything when no secret is configured", () => {
    expect(verifySecret(req({ "x-telegram-bot-api-secret-token": [SECRET, SECRET] }), SECRET)).toBe(false);
    expect(verifySecret(req({ "x-telegram-bot-api-secret-token": "" }), "")).toBe(false);
  });
});

describe("update dedup", () => {
  it("handles an update id once", () => {
    const s = new SeenSet();
    expect(s.firstTime(1)).toBe(true);
    expect(s.firstTime(1)).toBe(false);
    expect(s.firstTime(2)).toBe(true);
  });

  it("forgets ids past their age and past the size bound", () => {
    let now = 0;
    const s = new SeenSet(3, 1000, () => now);
    s.firstTime(1);
    now = 1500;
    expect(s.firstTime(1)).toBe(true);
    s.firstTime(2);
    s.firstTime(3);
    s.firstTime(4);
    // 1 was the oldest and is gone; 4 is remembered.
    expect(s.firstTime(4)).toBe(false);
    expect(s.firstTime(1)).toBe(true);
  });
});

const policy = (p: Partial<AccessPolicy> = {}): AccessPolicy => ({
  allowedUsers: new Set(["42"]),
  allowedChats: new Set(["-100500"]),
  allowAnyone: false,
  trustedChats: new Set(),
  ...p,
});

const alice = { id: 42, first_name: "Alice", last_name: "Smith" };
const mallory = { id: 66, first_name: "Mallory" };
const privateChat: TgChat = { id: 42, type: "private" };
const group: TgChat = { id: -100500, type: "supergroup", title: "Team" };

describe("identity", () => {
  it("a private chat is the person's own, trusted conversation", () => {
    expect(identify(privateChat, alice, undefined, policy())).toEqual({
      ok: true,
      speaker: { subject: "telegram-42", name: "Alice Smith", trust: "trusted" },
      scope: "personal",
      conversationId: "tg:42",
      runAs: { subject: "telegram-42", name: "Alice Smith" },
    });
  });

  it("a group runs as the group itself, with untrusted speakers by default", () => {
    const id = identify(group, alice, undefined, policy());
    expect(id).toEqual({
      ok: true,
      speaker: { subject: "telegram-42", name: "Alice Smith", trust: "untrusted" },
      scope: "group",
      conversationId: "tg:-100500",
      runAs: { subject: sharedSubject("telegram", "-100500"), name: "Telegram group -100500" },
    });
  });

  it("an operator-trusted group's speakers are trusted", () => {
    const id = identify(group, alice, undefined, policy({ trustedChats: new Set(["-100500"]) }));
    expect(id.ok && id.speaker.trust).toBe("trusted");
  });

  it("a forum topic is its own session in the group's memory", () => {
    const a = identify(group, alice, 7, policy());
    const b = identify(group, alice, 8, policy());
    expect(a.ok && a.conversationId).toBe("tg:-100500:7");
    expect(b.ok && b.conversationId).toBe("tg:-100500:8");
    expect(a.ok && b.ok && a.runAs.subject === b.runAs.subject).toBe(true);
  });

  it("only a forum topic's thread id names a session, not a reply thread", () => {
    expect(topicOf({ is_topic_message: true, message_thread_id: 7 })).toBe(7);
    expect(topicOf({ message_thread_id: 7 })).toBeUndefined();
  });

  it("refuses a user or a group outside the allow-list, with a notice", () => {
    expect(identify({ id: 66, type: "private" }, mallory, undefined, policy())).toEqual({
      ok: false,
      reason: "user_not_allowed",
      notice: NOT_ALLOWED_NOTICE,
      conversationId: "tg:66",
    });
    expect(identify({ id: -1009, type: "group" }, alice, undefined, policy())).toMatchObject({ ok: false, reason: "chat_not_allowed" });
    // An allowed user in a group that is not allowed is still refused.
    expect(identify({ id: -1009, type: "supergroup" }, alice, undefined, policy()).ok).toBe(false);
  });

  it("ALLOW_ANYONE opens private chats and groups", () => {
    expect(identify({ id: 66, type: "private" }, mallory, undefined, policy({ allowAnyone: true })).ok).toBe(true);
    expect(identify({ id: -1009, type: "group" }, mallory, undefined, policy({ allowAnyone: true })).ok).toBe(true);
  });

  it("drops bots, channels and a private chat that is not the sender's", () => {
    expect(identify(privateChat, { id: 42, is_bot: true }, undefined, policy())).toEqual({ ok: false, reason: "self_or_bot" });
    expect(identify({ id: -1001, type: "channel" }, alice, undefined, policy({ allowAnyone: true }))).toEqual({ ok: false, reason: "unsupported_chat" });
    expect(identify({ id: 43, type: "private" }, alice, undefined, policy()).ok).toBe(false);
    expect(identify(privateChat, undefined, undefined, policy())).toEqual({ ok: false, reason: "no_user" });
  });

  it("makes a display name safe to put before a message", () => {
    expect(displayName({ id: 1, first_name: "Eve]\n[Alice", last_name: "x" })).toBe("Eve Alice x");
    expect(displayName({ id: 1, first_name: "\n" })).toBe("user 1");
    expect(displayName({ id: 1, username: "u".repeat(100) })).toHaveLength(64);
  });
});

const bot = { id: 999, username: "LoomBot" };
const msg = (text: string, chat = group, extra: Partial<TgMessage> = {}): TgMessage => ({ message_id: 1, chat, from: alice, text, ...extra });

describe("addressed text", () => {
  it("takes every message in a private chat", () => {
    expect(addressedText(msg("hello", privateChat), bot)).toBe("hello");
  });

  it("strips the bot's own suffix from a command and ignores a command for another bot", () => {
    expect(addressedText(msg("/new@LoomBot"), bot)).toBe("/new");
    expect(addressedText(msg("/stop@loombot"), bot)).toBe("/stop");
    expect(addressedText(msg("/new@OtherBot"), bot)).toBeUndefined();
    expect(addressedText(msg("/new@OtherBot", privateChat), bot)).toBeUndefined();
  });

  it("in a group, takes an unaddressed command only when it is one of the bridge's", () => {
    expect(addressedText(msg("/help"), bot)).toBe("/help");
    expect(addressedText(msg("/weather"), bot)).toBeUndefined();
    expect(addressedText(msg("/weather@LoomBot today"), bot)).toBe("/weather today");
    expect(addressedText(msg("/weather", privateChat), bot)).toBe("/weather");
  });

  it("shows the help for /start", () => {
    expect(addressedText(msg("/start", privateChat), bot)).toBe("/help");
  });

  it("in a group, takes a mention or a reply to the bot and nothing else", () => {
    expect(addressedText(msg("just chatting"), bot)).toBeUndefined();
    const text = "@LoomBot what is new?";
    expect(addressedText(msg(text, group, { entities: [{ type: "mention", offset: 0, length: 8 }] }), bot)).toBe("what is new?");
    // A mention of someone else does not count.
    expect(addressedText(msg("@Alice hi", group, { entities: [{ type: "mention", offset: 0, length: 6 }] }), bot)).toBeUndefined();
    expect(addressedText(msg("yes please", group, { reply_to_message: { from: { id: 999, is_bot: true } } }), bot)).toBe("yes please");
    expect(addressedText(msg("yes please", group, { reply_to_message: { from: { id: 5 } } }), bot)).toBeUndefined();
  });

  it("ignores a message with no text", () => {
    expect(addressedText({ message_id: 1, chat: privateChat, from: alice }, bot)).toBeUndefined();
    expect(addressedText(msg("   ", privateChat), bot)).toBeUndefined();
  });
});

/** wellFormed checks that html uses only Telegram's tags, properly nested, and
 *  that no other "<" survives. */
function wellFormed(html: string): boolean {
  const stack: string[] = [];
  const re = /<(\/?)(b|i|s|code|pre|a)(?:\s+(?:class|href)="[^"<>]*")?>|<|>/g;
  for (const m of html.matchAll(re)) {
    if (m[0] === "<" || m[0] === ">") return false;
    if (m[1]) {
      if (stack.pop() !== m[2]) return false;
    } else stack.push(m[2]!);
  }
  return stack.length === 0;
}

describe("markdown to Telegram HTML", () => {
  it("converts the common forms", () => {
    expect(markdownToHtml("**bold** and *it* and _it2_ and ~~gone~~")).toBe("<b>bold</b> and <i>it</i> and <i>it2</i> and <s>gone</s>");
    expect(markdownToHtml("run `ls -la` now")).toBe("run <code>ls -la</code> now");
    expect(markdownToHtml("see [the docs](https://example.com/a?b=1&c=2)")).toBe('see <a href="https://example.com/a?b=1&amp;c=2">the docs</a>');
    expect(markdownToHtml("```js\nif (a < b && c) {}\n```")).toBe('<pre><code class="language-js">if (a &lt; b &amp;&amp; c) {}</code></pre>');
    expect(markdownToHtml("## Title\nbody")).toBe("<b>Title</b>\nbody");
  });

  it("escapes everything that is not one of its own tags", () => {
    expect(markdownToHtml("<b>not bold</b> & <script>x</script>")).toBe("&lt;b&gt;not bold&lt;/b&gt; &amp; &lt;script&gt;x&lt;/script&gt;");
    expect(markdownToHtml("`<i>`")).toBe("<code>&lt;i&gt;</code>");
  });

  it("leaves snake_case, bullets and lone markers as text", () => {
    expect(markdownToHtml("call my_func_name now")).toBe("call my_func_name now");
    expect(markdownToHtml("* item one\n* item two")).toBe("* item one\n* item two");
    expect(markdownToHtml("2 * 3 = 6 and **unclosed")).toBe("2 * 3 = 6 and **unclosed");
  });

  it("links only schemes Telegram opens, and quotes the href", () => {
    expect(markdownToHtml("[x](javascript:alert(1))")).toBe("[x](javascript:alert(1))");
    expect(markdownToHtml('[x](https://e.com/"onmouseover=1)')).toBe('<a href="https://e.com/&quot;onmouseover=1">x</a>');
  });

  it("runs an unclosed fence to the end, so a streamed answer stays valid", () => {
    expect(markdownToHtml("before\n```py\nx = 1")).toBe('before\n<pre><code class="language-py">x = 1</code></pre>');
  });

  it("never puts code inside another entity, which Telegram forbids", () => {
    const html = markdownToHtml("**see `x` here** and [a `b`](https://e.com)");
    expect(html).not.toMatch(/<b>[^<]*<code>/);
    expect(html).toContain('<a href="https://e.com">a b</a>');
    expect(wellFormed(html)).toBe(true);
  });

  it("keeps its placeholders unforgeable", () => {
    expect(markdownToHtml("\u00010\u0001 \u00020\u0002 `x`")).toBe("0 0 <code>x</code>");
  });

  it("is well formed for any mix of markers", () => {
    const parts = ["**", "*", "_", "__", "~~", "`", "```", "\n", "[", "](https://e.com)", "a", " ", "<", "&", "b_c", "# "];
    let seed = 7;
    const rand = () => (seed = (seed * 1103515245 + 12345) % 2 ** 31) / 2 ** 31;
    for (let n = 0; n < 3000; n++) {
      const len = 1 + Math.floor(rand() * 14);
      const s = Array.from({ length: len }, () => parts[Math.floor(rand() * parts.length)]).join("");
      const html = markdownToHtml(s);
      if (!wellFormed(html)) throw new Error(`not well formed: ${JSON.stringify(s)} -> ${JSON.stringify(html)}`);
    }
  });
});

const question = (options: string[]): CanonicalCard => {
  const base = { runId: "run-1", interruptId: "int-1" };
  return {
    kind: "question",
    ...base,
    question: "Which **file**?",
    context: "Only Alice can answer.",
    options,
    buttons: [
      ...(options.length
        ? options.map((o, i) => ({ id: `opt${i}AAAA`, label: o, action: { verb: VERB.answer, ...base, answer: o } }))
        : [{ id: "sendAAAA", label: "Send", action: { verb: VERB.answer, ...base } }]),
      { id: "declAAAA", label: "Decline", action: { verb: VERB.decline, ...base } },
    ],
  };
};

describe("cards", () => {
  it("renders a question's options as one button per row, carrying the button id", () => {
    const r = renderCard(question(["a.txt", "b.txt"]));
    expect(r.html).toBe("Which <b>file</b>?\n\nOnly Alice can answer.");
    expect(r.replyMarkup).toEqual({
      inline_keyboard: [[{ text: "a.txt", callback_data: "opt0AAAA" }], [{ text: "b.txt", callback_data: "opt1AAAA" }], [{ text: "Decline", callback_data: "declAAAA" }]],
    });
  });

  it("asks for a reply to a free-text question, with Decline and the reply box", () => {
    const r = renderCard(question([]));
    expect(r.html).toContain(FREE_TEXT_HINT);
    expect(r.plain).toBe(`Which **file**?\n\nOnly Alice can answer.\n\n${FREE_TEXT_HINT}`);
    expect(r.replyMarkup).toEqual({ inline_keyboard: [[{ text: "Decline", callback_data: "declAAAA" }]], force_reply: true });
  });

  it("renders a review with approve and send back on one row", () => {
    const r = renderCard({
      kind: "review",
      runId: "r",
      round: 2,
      buttons: [
        { id: "apprAAAA", label: "Approve", action: { verb: VERB.approve, runId: "r" } },
        { id: "rejeAAAA", label: "Send back", action: { verb: VERB.reject, runId: "r" } },
      ],
    });
    expect(r.plain).toBe("The agent's answer is waiting for your review. (round 2)");
    expect(r.replyMarkup?.inline_keyboard).toEqual([[{ text: "Approve", callback_data: "apprAAAA" }, { text: "Send back", callback_data: "rejeAAAA" }]]);
  });

  it("renders a result card as text with no keyboard", () => {
    expect(renderCard({ kind: "result", message: "Answered: a.txt" })).toEqual({ html: "Answered: a.txt", plain: "Answered: a.txt" });
  });

  it("refuses a button id past Telegram's 64 bytes rather than draw a dead button", () => {
    const card = question(["x"]);
    if (card.kind !== "question") throw new Error("unreachable");
    card.buttons[0]!.id = "é".repeat(33); // 66 bytes, 33 characters
    expect(() => renderCard(card)).toThrow(/66 bytes/);
  });

  it("round-trips a button: what is drawn is what a press hands the core", () => {
    const r = renderCard(question(["a.txt"]));
    for (const row of r.replyMarkup!.inline_keyboard) {
      for (const b of row) {
        expect(Buffer.byteLength(b.callback_data)).toBeLessThanOrEqual(64);
        expect(parseCallbackData(b.callback_data)).toEqual({ buttonId: b.callback_data });
      }
    }
  });

  it("reads nothing from callback data that cannot be a button id", () => {
    expect(parseCallbackData(undefined)).toBeUndefined();
    expect(parseCallbackData("")).toBeUndefined();
    expect(parseCallbackData("x".repeat(65))).toBeUndefined();
    expect(parseCallbackData({ verb: "lc.answer" })).toBeUndefined();
  });
});

describe("Bot API client", () => {
  const api = (tg: FakeTelegram, sleeps: number[] = []) =>
    new TelegramApi({ token: TOKEN, baseUrl: "https://api.telegram.org", fetch: tg.fetch, sleep: async (ms) => void sleeps.push(ms) });

  it("puts the token in the path, as Telegram wants, and returns the result", async () => {
    const tg = new FakeTelegram();
    expect(await api(tg).call("getMe")).toMatchObject({ username: "LoomBot" });
    expect(tg.calls[0]!.path).toBe(`/bot${TOKEN}/getMe`);
  });

  it("waits out a 429 for retry_after seconds, then retries", async () => {
    const tg = new FakeTelegram();
    tg.reply.sendMessage = (_p, n) => (n === 1 ? tgError(429, "Too Many Requests: retry after 2", { retry_after: 2 }) : undefined);
    const sleeps: number[] = [];
    await api(tg, sleeps).call("sendMessage", { chat_id: 1, text: "x" });
    expect(sleeps).toEqual([2000]);
    expect(tg.callsTo("sendMessage")).toHaveLength(2);
  });

  it("does not wait out a long flood limit, or any 429 when asked not to", async () => {
    const tg = new FakeTelegram();
    tg.reply.sendMessage = () => tgError(429, "Too Many Requests", { retry_after: MAX_RETRY_AFTER_SECONDS + 1 });
    await expect(api(tg).call("sendMessage", {})).rejects.toMatchObject({ code: 429, retryAfter: MAX_RETRY_AFTER_SECONDS + 1 });
    tg.reply.sendChatAction = () => tgError(429, "Too Many Requests", { retry_after: 1 });
    await expect(api(tg).call("sendChatAction", {}, { retry: false })).rejects.toMatchObject({ code: 429 });
    expect(tg.callsTo("sendChatAction")).toHaveLength(1);
  });

  it("keeps the token and URL out of every error", async () => {
    const failing = (async () => {
      throw new TypeError(`fetch failed: https://api.telegram.org/bot${TOKEN}/sendMessage`);
    }) as typeof fetch;
    const err = await new TelegramApi({ token: TOKEN, baseUrl: "https://api.telegram.org", fetch: failing }).call("sendMessage").catch((e: unknown) => e);
    expect(err).toBeInstanceOf(TelegramError);
    expect(String((err as Error).message)).toBe("telegram sendMessage failed (0)");
    expect(JSON.stringify(err) + String((err as Error).stack)).not.toContain(TOKEN);

    const tg = new FakeTelegram();
    tg.reply.sendMessage = () => tgError(400, "Bad Request: can't parse entities: Unsupported start tag");
    const e2 = await api(tg).call("sendMessage").catch((e: unknown) => e);
    expect((e2 as Error).message).toBe("telegram sendMessage failed (400)");
    expect(isParseError(e2)).toBe(true);
  });
});
