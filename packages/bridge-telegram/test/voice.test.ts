// Voice messages on Telegram (RFC GA): which messages reach the core's voice
// path, the download from the Bot API's file host, and the voice-note reply.

import net from "node:net";

import { keyFromSecret, MemoryStore, VOICE_TEXT, VoiceRefusedError, type Bridge, type VoiceIn } from "@loomcycle/bridge-core";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { TelegramApi, TelegramError } from "../src/api.js";
import { createTelegramBridge } from "../src/bridge.js";
import { loadConfig } from "../src/config.js";
import { addressedText, TEXT_ONLY_NOTICE, voiceOf, type TelegramRef, type TgMessage, type TgUpdate } from "../src/identity.js";
import { TelegramVoice } from "../src/voice.js";
import { fakeSpeech, FakeGate, FakeLoomcycle, FakeTelegram, settle, silentLogger, waitFor, type FormFile } from "./fakes.js";

const TOKEN = "123456:SECRETtokenValue_x";
const bot = { id: 999, username: "LoomBot" };
const alice = { id: 42, is_bot: false, first_name: "Alice", language_code: "uk" };
const privateChat = { id: 42, type: "private" };
const group = { id: -100500, type: "supergroup", title: "Team" };
const voiceNote = { file_id: "v-1", duration: 7, mime_type: "audio/ogg", file_size: 12_000 };

function msg(over: Partial<TgMessage>): TgMessage {
  return { message_id: 5, chat: privateChat, from: alice, ...over } as TgMessage;
}

describe("voiceOf", () => {
  it("maps a private voice note to a VoiceIn from its metadata", () => {
    expect(voiceOf(msg({ voice: voiceNote }), bot, { audio: false })).toEqual<VoiceIn>({
      kind: "voice",
      ref: "v-1",
      mime: "audio/ogg",
      seconds: 7,
      bytes: 12_000,
    });
  });

  it("maps a round video note to video/mp4, which the Bot API never states", () => {
    expect(voiceOf(msg({ video_note: { file_id: "vn", duration: 3 } }), bot, { audio: false })).toEqual({
      kind: "video_note",
      ref: "vn",
      mime: "video/mp4",
      seconds: 3,
    });
  });

  it("takes an audio file only when the operator asked for them", () => {
    const m = msg({ audio: { file_id: "a1", duration: 200, mime_type: "audio/mpeg" } });
    expect(voiceOf(m, bot, { audio: false })).toBeUndefined();
    expect(voiceOf(m, bot, { audio: true })).toEqual({ kind: "audio_file", ref: "a1", mime: "audio/mpeg", seconds: 200 });
  });

  it("takes a group voice note only when it replies to the bot", () => {
    expect(voiceOf(msg({ chat: group, voice: voiceNote }), bot, { audio: false })).toBeUndefined();
    expect(voiceOf(msg({ chat: group, voice: voiceNote, reply_to_message: { from: { id: 7 } } }), bot, { audio: false })).toBeUndefined();
    expect(voiceOf(msg({ chat: group, voice: voiceNote, reply_to_message: { from: { id: 999, is_bot: true } } }), bot, { audio: false })?.kind).toBe("voice");
  });

  it("replaces a MIME type that is not audio or video with the kind's default", () => {
    expect(voiceOf(msg({ voice: { file_id: "v", mime_type: "text/html" } }), bot, { audio: false })?.mime).toBe("audio/ogg");
    expect(voiceOf(msg({ voice: { file_id: "v", mime_type: "audio/ogg\r\nx: y" } }), bot, { audio: false })?.mime).toBe("audio/ogg");
  });

  it("ignores a message with text, a photo, or a voice note without a file id", () => {
    expect(voiceOf(msg({ text: "hi" }), bot, { audio: true })).toBeUndefined();
    expect(voiceOf(msg({}), bot, { audio: true })).toBeUndefined();
    expect(voiceOf(msg({ voice: { file_id: "" } }), bot, { audio: true })).toBeUndefined();
  });
});

describe("addressedText and /voice", () => {
  it("takes an unaddressed /voice in a group only when voice is on", () => {
    const m = msg({ chat: group, text: "/voice on" });
    expect(addressedText(m, bot)).toBeUndefined();
    expect(addressedText(m, bot, { voice: true })).toBe("/voice on");
    // Addressed to the bot, it is ours either way.
    expect(addressedText(msg({ chat: group, text: "/voice@LoomBot off" }), bot)).toBe("/voice off");
  });
});

function voicePort(tg: FakeTelegram, replyTarget?: (ref: TelegramRef) => number | undefined) {
  const api = new TelegramApi({ token: TOKEN, baseUrl: "https://api.telegram.org", fetch: tg.fetch, sleep: async () => {} });
  return new TelegramVoice({ api, ...(replyTarget ? { replyTarget } : {}) });
}

const signal = () => AbortSignal.timeout(5_000);
const vin: VoiceIn = { kind: "voice", ref: "v-1", mime: "audio/ogg", seconds: 7 };

/** caught runs f and returns what it threw. */
async function caught(f: () => Promise<unknown>): Promise<Error> {
  try {
    await f();
  } catch (err) {
    return err as Error;
  }
  throw new Error("expected a throw");
}

describe("download", () => {
  it("asks getFile, then fetches from the Bot API's file host without following redirects", async () => {
    const tg = new FakeTelegram();
    let init: RequestInit | undefined;
    tg.file = (_p, i) => {
      init = i;
      return new Response(new Uint8Array([1, 2, 3]), { status: 200 });
    };
    const got = await voicePort(tg).download(vin, 1000, signal());
    expect(tg.callsTo("getFile")[0]!.params).toEqual({ file_id: "v-1" });
    expect(tg.callsTo("file")[0]!.path).toBe(`/file/bot${TOKEN}/voice/file_1.oga`);
    expect(init?.redirect).toBe("manual");
    expect(got).toEqual({ bytes: new Uint8Array([1, 2, 3]), mime: "audio/ogg", name: "voice.ogg" });
  });

  it("stops reading a body that grows past maxBytes, while it streams", async () => {
    const tg = new FakeTelegram();
    let pulled = 0;
    tg.file = () =>
      new Response(
        new ReadableStream<Uint8Array>({
          // 100 KB in 100-byte chunks: far past the 1,000-byte cap.
          pull(c) {
            pulled++;
            if (pulled > 1000) c.close();
            else c.enqueue(new Uint8Array(100));
          },
        }),
        { status: 200 },
      );
    const err = await caught(() => voicePort(tg).download(vin, 1000, signal()));
    expect(err).toBeInstanceOf(TelegramError);
    expect((err as TelegramError).code).toBe(413);
    // Cut off just past the cap, not read to the end.
    expect(pulled).toBeLessThan(20);
  });

  it("refuses a declared length over maxBytes, and a getFile size over it, before reading", async () => {
    const tg = new FakeTelegram();
    tg.file = () => new Response(new Uint8Array(10), { status: 200, headers: { "content-length": "5000" } });
    expect(((await caught(() => voicePort(tg).download(vin, 1000, signal()))) as TelegramError).code).toBe(413);

    const tg2 = new FakeTelegram();
    tg2.reply.getFile = () => ({ ok: true, result: { file_id: "v-1", file_size: 5000, file_path: "voice/file_1.oga" } });
    expect(((await caught(() => voicePort(tg2).download(vin, 1000, signal()))) as TelegramError).code).toBe(413);
    expect(tg2.callsTo("file")).toHaveLength(0);
  });

  it("refuses a redirect, and an answer from another host", async () => {
    const tg = new FakeTelegram();
    tg.file = () => new Response(null, { status: 302, headers: { location: "https://evil.example/x" } });
    const err = await caught(() => voicePort(tg).download(vin, 1000, signal()));
    expect((err as TelegramError).description).toBe("redirect refused");

    const tg2 = new FakeTelegram();
    tg2.file = () => {
      const r = new Response(new Uint8Array([1]), { status: 200 });
      Object.defineProperty(r, "url", { value: "https://evil.example/x" });
      return r;
    };
    expect(((await caught(() => voicePort(tg2).download(vin, 1000, signal()))) as TelegramError).description).toBe("answered from another host");
  });

  it("refuses a file path that could steer the URL, without fetching it", async () => {
    for (const path of ["../bot/getMe", "/etc/passwd", "https://evil.example/x", "voice/../../x", "voice/a?b", ".hidden", ""]) {
      const tg = new FakeTelegram();
      tg.reply.getFile = () => ({ ok: true, result: { file_id: "v-1", file_path: path } });
      const err = await caught(() => voicePort(tg).download(vin, 1000, signal()));
      expect(err).toBeInstanceOf(TelegramError);
      expect(tg.callsTo("file")).toHaveLength(0);
    }
  });

  it("never puts the bot token in a thrown error", async () => {
    const answers: Array<() => Response> = [
      () => {
        // fetch's own error names the URL it called.
        throw new TypeError(`fetch failed: https://api.telegram.org/file/bot${TOKEN}/voice/file_1.oga`);
      },
      () => new Response("nope", { status: 404 }),
      () => new Response(null, { status: 301, headers: { location: `https://evil.example/${TOKEN}` } }),
      () => new Response(new Uint8Array(5000), { status: 200 }),
    ];
    for (const answer of answers) {
      const tg = new FakeTelegram();
      tg.file = answer;
      const err = await caught(() => voicePort(tg).download(vin, 1000, signal()));
      expect(err).toBeInstanceOf(TelegramError);
      for (const s of [err.message, String(err), err.stack ?? "", JSON.stringify(err), String((err as { cause?: unknown }).cause ?? "")]) {
        expect(s).not.toContain(TOKEN);
        expect(s).not.toContain("SECRET");
      }
    }
  });
});

describe("sendVoice", () => {
  const audio = { bytes: new Uint8Array([9, 8, 7]), mime: "audio/ogg", seconds: 2.6, label: "AI voice" };

  it("posts the audio as a multipart voice note, captioned, replying to the text", async () => {
    const tg = new FakeTelegram();
    await voicePort(tg, () => 321).sendVoice({ chatId: -100500, threadId: 7 }, audio);
    const [call] = tg.callsTo("sendVoice");
    expect(call!.kind).toBe("form");
    const { voice, ...rest } = call!.params;
    expect(rest).toEqual({
      chat_id: "-100500",
      message_thread_id: "7",
      caption: "AI voice",
      duration: "3",
      reply_parameters: JSON.stringify({ message_id: 321, allow_sending_without_reply: true }),
    });
    expect(voice).toEqual<FormFile>({ name: "voice.ogg", type: "audio/ogg", bytes: new Uint8Array([9, 8, 7]) });
  });

  it("replies to nothing when the bridge's text message is not known", async () => {
    const tg = new FakeTelegram();
    await voicePort(tg).sendVoice({ chatId: 42 }, { ...audio, mime: "audio/mpeg" });
    const p = tg.callsTo("sendVoice")[0]!.params;
    expect(p.reply_parameters).toBeUndefined();
    expect((p.voice as FormFile).name).toBe("voice.mp3");
  });

  it("turns Telegram's VOICE_MESSAGES_FORBIDDEN into a VoiceRefusedError, and passes other errors on", async () => {
    const tg = new FakeTelegram();
    tg.reply.sendVoice = () => ({ ok: false, error_code: 400, description: "Bad Request: VOICE_MESSAGES_FORBIDDEN" });
    expect(await caught(() => voicePort(tg).sendVoice({ chatId: 42 }, audio))).toBeInstanceOf(VoiceRefusedError);

    const tg2 = new FakeTelegram();
    tg2.reply.sendVoice = () => ({ ok: false, error_code: 400, description: "Bad Request: chat not found" });
    const err = await caught(() => voicePort(tg2).sendVoice({ chatId: 42 }, audio));
    expect(err).toBeInstanceOf(TelegramError);
    expect(err).not.toBeInstanceOf(VoiceRefusedError);
  });

  it("shows typing while listening and record_voice while speaking, never retried", async () => {
    const tg = new FakeTelegram();
    const p = voicePort(tg);
    await p.indicate({ chatId: 42 }, "listening");
    await p.indicate({ chatId: -1, threadId: 3 }, "speaking");
    expect(tg.callsTo("sendChatAction").map((c) => c.params)).toEqual([
      { chat_id: 42, action: "typing" },
      { chat_id: -1, message_thread_id: 3, action: "record_voice" },
    ]);
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

describe("telegram bridge with voice", () => {
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
      LOOMCYCLE_BASE_URL: "https://loomcycle.test",
      BRIDGE_GATE: "telegram",
      BRIDGE_GATE_AUDIENCE: "https://lc.example",
      BRIDGE_GATE_SECRET: "s".repeat(40),
      BRIDGE_AGENT: "helper",
      BRIDGE_VOICE: "1",
      TELEGRAM_BOT_TOKEN: "123:abc",
      TELEGRAM_BRIDGE_WEBHOOK_SECRET: "hook",
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
      message: { typingEveryMs: 60_000 },
    });
    await bridge.start();
  }

  beforeEach(() => {
    lc = new FakeLoomcycle();
    lc.speech = fakeSpeech();
    tg = new FakeTelegram();
    gate = new FakeGate();
  });

  afterEach(async () => {
    // A run that never streamed its agent id cannot be cancelled; end it so
    // stop does not wait on it.
    for (const r of lc.runs) r.events.end();
    await bridge?.stop();
  });

  function post(update: object) {
    return fetch(`${base}/telegram/webhook`, {
      method: "POST",
      headers: { "content-type": "application/json", "x-telegram-bot-api-secret-token": "hook" },
      body: JSON.stringify(update),
    });
  }

  const update = (m: object, from: object = alice, chat: object = privateChat): TgUpdate =>
    ({ update_id: nextUpdate++, message: { message_id: 50 + nextUpdate, chat, from, ...m } }) as TgUpdate;

  it("routes a private voice note to the core's voice path with its VoiceIn and the sender's locale", async () => {
    await start();
    const onVoice = vi.spyOn(bridge.router, "onVoice");
    await post(update({ voice: voiceNote }));
    await waitFor(() => lc.runs.length === 1, "run");
    expect(onVoice).toHaveBeenCalledTimes(1);
    const [id, ref, voice, locale] = onVoice.mock.calls[0]!;
    expect(id).toMatchObject({ ok: true, scope: "personal" });
    expect(ref).toEqual({ chatId: 42 });
    expect(voice).toEqual({ kind: "voice", ref: "v-1", mime: "audio/ogg", seconds: 7, bytes: 12_000 });
    expect(locale).toBe("uk");
    // Downloaded through getFile, transcribed with the speaker's token and a
    // language hint, and handed to the agent marked as spoken.
    expect(tg.callsTo("getFile")[0]!.params).toEqual({ file_id: "v-1" });
    const [tr] = lc.callsTo("transcribe");
    expect(tr!.token).toMatch(/^tok-telegram:42-/);
    expect(tr!.args[1]).toMatchObject({ contentType: "audio/ogg", filename: "voice.ogg", language: "uk" });
    expect(lc.runs[0]!.opts.segments).toEqual([{ role: "user", content: [{ type: "trusted-text", text: "(voice message)\nwhat is the weather" }] }]);
    // The gate is asked for the speech surfaces with the user's token.
    expect(gate.asserts[0]!.payload).toMatchObject({ token: { surfaces: ["speech", "transcription"] } });
  });

  it("answers in text, then a captioned voice note replying to that text", async () => {
    await start();
    await post(update({ voice: voiceNote }));
    await waitFor(() => lc.runs.length === 1, "run");
    const run = lc.runs[0]!;
    run.events.push({ type: "session", session_id: "sess-1" });
    run.events.push({ type: "agent", run_id: "run-1", agent_id: "agent-1", session_id: "sess-1", parent_agent_id: null });
    run.events.push({ type: "text", text: "It is sunny." });
    run.events.push({ type: "awaiting_input" });
    await waitFor(() => tg.callsTo("sendVoice").length === 1, "voice reply");
    const textId = 100; // the fake's first sendMessage id
    expect(tg.texts()).toEqual(["It is sunny."]);
    const p = tg.callsTo("sendVoice")[0]!.params;
    expect(p.caption).toBe("AI voice");
    expect(p.reply_parameters).toBe(JSON.stringify({ message_id: textId, allow_sending_without_reply: true }));
    expect((p.voice as FormFile).type).toBe("audio/ogg");
    expect(lc.callsTo("speech")[0]!.args).toEqual(["It is sunny.", { format: "ogg", language: "en" }]);
    expect(tg.callsTo("sendChatAction").map((c) => c.params.action)).toContain("record_voice");
  });

  it("ignores a group voice note that does not reply to the bot, and takes one that does", async () => {
    await start();
    const onVoice = vi.spyOn(bridge.router, "onVoice");
    await post(update({ voice: voiceNote }, alice, group));
    await settle();
    expect(onVoice).not.toHaveBeenCalled();
    expect(tg.callsTo("sendMessage")).toHaveLength(0);
    expect(lc.calls).toHaveLength(0);

    await post(update({ voice: voiceNote, reply_to_message: { from: { id: 999, is_bot: true } } }, alice, group));
    await waitFor(() => lc.runs.length === 1, "run");
    expect(onVoice).toHaveBeenCalledTimes(1);
    expect(onVoice.mock.calls[0]![0]).toMatchObject({ ok: true, scope: "group" });
  });

  it("takes an audio file only with TELEGRAM_BRIDGE_VOICE_AUDIO=1; without it the text-only notice", async () => {
    await start();
    const onVoice = vi.spyOn(bridge.router, "onVoice");
    await post(update({ audio: { file_id: "a1", duration: 30, mime_type: "audio/mpeg" } }));
    await waitFor(() => tg.callsTo("sendMessage").length === 1, "notice");
    expect(tg.texts()).toEqual([TEXT_ONLY_NOTICE]);
    expect(onVoice).not.toHaveBeenCalled();
    await bridge.stop();

    await start({ TELEGRAM_BRIDGE_VOICE_AUDIO: "1" });
    const onVoice2 = vi.spyOn(bridge.router, "onVoice");
    await post(update({ audio: { file_id: "a1", duration: 30, mime_type: "audio/mpeg" } }));
    await waitFor(() => onVoice2.mock.calls.length === 1, "voice");
    expect(onVoice2.mock.calls[0]![2]).toMatchObject({ kind: "audio_file", ref: "a1", mime: "audio/mpeg" });
  });

  it("keeps the text-only notice for a photo or a sticker with voice on", async () => {
    await start();
    const onVoice = vi.spyOn(bridge.router, "onVoice");
    await post(update({ photo: [{ file_id: "p1", width: 1, height: 1 }] }));
    await post(update({ sticker: { file_id: "s1" } }));
    await waitFor(() => tg.callsTo("sendMessage").length === 2, "notices");
    expect(tg.texts()).toEqual([TEXT_ONLY_NOTICE, TEXT_ONLY_NOTICE]);
    expect(onVoice).not.toHaveBeenCalled();
  });

  it("says exactly today's text-only notice when loomcycle grants no transcription", async () => {
    // The core's notice must stay the one the bridge gave before voice.
    expect(VOICE_TEXT.unavailable).toBe(TEXT_ONLY_NOTICE);
    lc.speech!.surfaces = { speech: false, transcription: false };
    await start();
    await post(update({ voice: voiceNote }));
    await waitFor(() => tg.callsTo("sendMessage").length === 1, "notice");
    expect(tg.texts()).toEqual([TEXT_ONLY_NOTICE]);
    expect(tg.callsTo("getFile")).toHaveLength(0);
    expect(lc.runs).toHaveLength(0);
  });

  it("with voice off, a voice note gets the text-only notice and never reaches the voice path", async () => {
    await start({ BRIDGE_VOICE: "0" });
    const onVoice = vi.spyOn(bridge.router, "onVoice");
    await post(update({ voice: voiceNote }));
    await waitFor(() => tg.callsTo("sendMessage").length === 1, "notice");
    expect(tg.texts()).toEqual([TEXT_ONLY_NOTICE]);
    expect(onVoice).not.toHaveBeenCalled();
    expect(gate.asserts).toHaveLength(0);
  });

  it("lists /voice in /help when voice is on", async () => {
    await start();
    await post(update({ text: "/help" }));
    await waitFor(() => tg.callsTo("sendMessage").length === 1, "help");
    expect(tg.texts()[0]).toContain("/voice on");
  });

  it("a stranger's voice note gets only the usual refusal, and nothing is downloaded", async () => {
    await start();
    const stranger = { id: 66, is_bot: false, first_name: "Stranger" };
    await post(update({ voice: voiceNote }, stranger, { id: 66, type: "private" }));
    await waitFor(() => tg.callsTo("sendMessage").length === 1, "refusal");
    expect(tg.texts()[0]).toMatch(/isn't available here/);
    expect(tg.callsTo("getFile")).toHaveLength(0);
    expect(lc.calls).toHaveLength(0);
  });
});
