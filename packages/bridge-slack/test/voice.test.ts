// The Slack voice port (RFC GA): where the bot token may go on a download,
// what a download takes, and the three-step upload of a spoken reply.

import { VoiceRefusedError, type BridgeContext, type Logger, type VoiceIn, type VoiceOut } from "@loomcycle/bridge-core";
import { beforeEach, describe, expect, it } from "vitest";

import { SlackApi } from "../src/api.js";
import { createSlackAdapter } from "../src/bridge.js";
import { loadConfig } from "../src/config.js";
import type { SlackRef } from "../src/identity.js";
import { readEvent, type EventEnvelope } from "../src/inbound.js";
import { slackSignature } from "../src/verify.js";
import { createSlackVoice, fileNameOf, MAX_UPLOAD_BYTES, SlackFileError } from "../src/voice.js";
import { FakeSlack, FILES_ORIGIN, settle, silentLogger } from "./fakes.js";

const TOKEN = "xoxb-voice-test";
const CLIP_URL = `${FILES_ORIGIN}/files-pri/T1-F1/download/audio_message.webm`;

const clip = (url: string | null = CLIP_URL): VoiceIn => ({ kind: "clip", ref: { id: "F1", ...(url !== null ? { url } : {}) }, mime: "audio/webm", seconds: 3, bytes: 4 });
const audio = (over: Partial<VoiceOut> = {}): VoiceOut => ({ bytes: new Uint8Array([1, 2, 3]), mime: "audio/mpeg", seconds: 1, label: "AI voice", ...over });
const dmRef: SlackRef = { team: "T1", channel: "D1", channelType: "im", messageTs: "100.1" };
const threadRef: SlackRef = { team: "T1", channel: "C1", channelType: "channel", threadTs: "200.1", messageTs: "200.5" };

const audioResponse = (bytes: Uint8Array, headers: Record<string, string> = {}) => () =>
  new Response(bytes, { status: 200, headers: { "content-type": "audio/webm", ...headers } });

/** chunked streams n bytes in pieces, with no Content-Length. */
function chunked(n: number, piece = 1024): () => Response {
  return () => {
    let sent = 0;
    const body = new ReadableStream<Uint8Array>({
      pull(c) {
        if (sent >= n) return c.close();
        const k = Math.min(piece, n - sent);
        sent += k;
        c.enqueue(new Uint8Array(k));
      },
    });
    return new Response(body, { status: 200, headers: { "content-type": "audio/webm" } });
  };
}

describe("slack voice: download", () => {
  let slack: FakeSlack;
  let calls: Array<{ url: string; auth?: string }>;
  let voice: ReturnType<typeof createSlackVoice>;
  /** A fetch that records every request, wherever it goes, then defers to FakeSlack. */
  let fetchAll: typeof fetch;

  beforeEach(() => {
    slack = new FakeSlack();
    calls = [];
    fetchAll = (async (input: string | URL | Request, init?: RequestInit) => {
      const url = String(input);
      calls.push({ url, auth: ((init?.headers ?? {}) as Record<string, string>).authorization });
      if (!url.startsWith(`${FILES_ORIGIN}/`)) return new Response("<html>elsewhere</html>", { status: 200, headers: { "content-type": "audio/webm" } });
      return slack.fetch(input, init);
    }) as typeof fetch;
    voice = createSlackVoice({ api: new SlackApi({ baseUrl: slack.base, token: TOKEN, fetch: fetchAll }), botToken: TOKEN, fetch: fetchAll, streamsDms: true });
  });

  const get = (v: VoiceIn = clip(), max = 1024) => voice.download(v, max, AbortSignal.timeout(5000));

  it("fetches url_private_download from files.slack.com with the bot token, and returns the audio", async () => {
    slack.files.set(CLIP_URL, audioResponse(new Uint8Array([9, 8, 7, 6]), { "content-type": "audio/webm; codecs=opus" }));
    const got = await get();
    expect(Array.from(got.bytes)).toEqual([9, 8, 7, 6]);
    expect(got.mime).toBe("audio/webm");
    expect(calls).toEqual([{ url: CLIP_URL, auth: `Bearer ${TOKEN}` }]);
  });

  it("never sends the token to a host other than files.slack.com, or over plain http", async () => {
    for (const url of [
      "https://evil.example/files-pri/x",
      "https://files.slack.com.evil.example/x",
      "https://evil.example@files.slack.com.evil.example/x",
      "http://files.slack.com/files-pri/x",
      "https://files.slack.com:8443/files-pri/x",
      "https://user:pw@files.slack.com/files-pri/x",
      "not a url",
    ]) {
      await expect(get(clip(url)), url).rejects.toMatchObject({ name: "SlackFileError", code: "not_slack_files_host" });
    }
    expect(calls).toEqual([]);
  });

  it("refuses a file the event gives no download URL for, without a request", async () => {
    await expect(get(clip(null))).rejects.toMatchObject({ code: "no_download_url" });
    expect(calls).toEqual([]);
  });

  it("follows a redirect within files.slack.com, and refuses one off it before sending the token", async () => {
    const moved = `${FILES_ORIGIN}/files-pri/T1-F1/moved.webm`;
    slack.files.set(CLIP_URL, () => new Response(null, { status: 302, headers: { location: moved } }));
    slack.files.set(moved, audioResponse(new Uint8Array([1])));
    expect(Array.from((await get()).bytes)).toEqual([1]);
    expect(calls.map((c) => c.url)).toEqual([CLIP_URL, moved]);

    calls.length = 0;
    slack.files.set(CLIP_URL, () => new Response(null, { status: 302, headers: { location: "https://cdn.evil.example/steal" } }));
    await expect(get()).rejects.toMatchObject({ code: "not_slack_files_host" });
    expect(calls.map((c) => c.url)).toEqual([CLIP_URL]);
  });

  it("refuses Slack's login page (text/html) and anything that is not audio or video", async () => {
    for (const type of ["text/html; charset=utf-8", "application/json", "image/png", ""]) {
      slack.files.set(CLIP_URL, () => new Response("<html>sign in</html>", { status: 200, headers: type ? { "content-type": type } : {} }));
      await expect(get(), type).rejects.toMatchObject({ code: "not_audio" });
    }
    slack.files.set(CLIP_URL, () => new Response(new Uint8Array([1]), { status: 200, headers: { "content-type": "video/mp4" } }));
    expect((await get()).mime).toBe("video/mp4");
  });

  it("caps the bytes while reading, whatever Content-Length says", async () => {
    slack.files.set(CLIP_URL, chunked(4096));
    await expect(get(clip(), 1024)).rejects.toMatchObject({ code: "too_large" });
    slack.files.set(CLIP_URL, chunked(1024));
    expect((await get(clip(), 1024)).bytes.byteLength).toBe(1024);
    slack.files.set(CLIP_URL, audioResponse(new Uint8Array(10), { "content-length": "999999" }));
    await expect(get(clip(), 1024)).rejects.toMatchObject({ code: "too_large" });
  });

  it("reports a failed download by a code, never the URL", async () => {
    slack.files.set(CLIP_URL, () => new Response("gone", { status: 404 }));
    const err = (await get().catch((e: unknown) => e)) as SlackFileError;
    expect(err).toBeInstanceOf(SlackFileError);
    expect(err.code).toBe("http_404");
    expect(String(err.message)).not.toContain("files-pri");
  });
});

describe("slack voice: spoken reply", () => {
  let slack: FakeSlack;
  const make = (streamsDms = true) =>
    createSlackVoice({ api: new SlackApi({ baseUrl: slack.base, token: TOKEN, fetch: slack.fetch }), botToken: TOKEN, fetch: slack.fetch, streamsDms });

  beforeEach(() => {
    slack = new FakeSlack();
  });

  it("uploads the audio in three steps into the conversation's thread, titled with the label", async () => {
    await make().sendVoice!(threadRef, audio({ label: "AI voice · first part; the rest is in the text" }));

    expect(slack.calls.map((c) => c.method)).toEqual(["files.getUploadURLExternal", "file", "files.completeUploadExternal"]);
    const [getUrl, upload, complete] = slack.calls;
    expect(getUrl!.body).toEqual({ filename: "reply.mp3", length: "3" });
    expect(getUrl!.auth).toBe(`Bearer ${TOKEN}`);
    expect(upload!.url).toBe(`${FILES_ORIGIN}/upload/v1/UPLOAD1`);
    expect(Array.from(upload!.bytes!)).toEqual([1, 2, 3]);
    // The upload URL carries its own authority.
    expect(upload!.auth).toBeUndefined();
    expect(complete!.body).toEqual({
      files: JSON.stringify([{ id: "FUP1", title: "AI voice · first part; the rest is in the text" }]),
      channel_id: "C1",
      thread_ts: "200.1",
    });
  });

  it("puts a DM's audio beside its text: in the answered message's thread when DMs stream, else top-level", async () => {
    await make(true).sendVoice!(dmRef, audio());
    expect(slack.to("files.completeUploadExternal")[0]!.body).toMatchObject({ channel_id: "D1", thread_ts: "100.1" });
    await make(false).sendVoice!(dmRef, audio());
    expect(slack.to("files.completeUploadExternal")[1]!.body).not.toHaveProperty("thread_ts");
  });

  it("names the file by its format", () => {
    expect(fileNameOf("audio/mpeg")).toBe("reply.mp3");
    expect(fileNameOf("audio/ogg; codecs=opus")).toBe("reply.ogg");
    expect(fileNameOf("audio/wav")).toBe("reply.wav");
    expect(fileNameOf("audio/x-wav")).toBe("reply.wav");
  });

  it("turns Slack's refusal to post a file here into VoiceRefusedError, at either API step", async () => {
    for (const code of ["not_in_channel", "channel_not_found", "missing_scope"]) {
      slack.reply("files.completeUploadExternal", { json: { ok: false, error: code } });
      await expect(make().sendVoice!(threadRef, audio()), code).rejects.toBeInstanceOf(VoiceRefusedError);
    }
    slack.reply("files.getUploadURLExternal", { json: { ok: false, error: "missing_scope" } });
    await expect(make().sendVoice!(threadRef, audio())).rejects.toBeInstanceOf(VoiceRefusedError);
  });

  it("leaves any other failure an ordinary error, so spoken replies are not turned off", async () => {
    slack.reply("files.completeUploadExternal", { json: { ok: false, error: "internal_error" } });
    const err = await make().sendVoice!(threadRef, audio()).catch((e: unknown) => e);
    expect(err).not.toBeInstanceOf(VoiceRefusedError);
    expect(err).toMatchObject({ code: "internal_error" });
  });

  it("refuses an upload URL that is not https, sending nothing to it", async () => {
    slack.reply("files.getUploadURLExternal", { json: { ok: true, upload_url: "http://files.slack.com/upload/v1/X", file_id: "F9" } });
    await expect(make().sendVoice!(threadRef, audio())).rejects.toMatchObject({ code: "no_upload_url" });
    expect(slack.to("file")).toEqual([]);
  });

  it("offers mp3 first, as an audio file, up to 50 MB", () => {
    expect(make().capabilities).toEqual({ voiceFormats: ["mp3", "ogg", "wav"], out: "file", maxUploadBytes: MAX_UPLOAD_BYTES });
    expect(make().indicate).toBeUndefined();
  });
});

const clipFile = {
  id: "F1",
  subtype: "slack_audio",
  mimetype: "audio/webm",
  size: 4000,
  duration_ms: 3500,
  url_private_download: CLIP_URL,
  name: "audio_message.webm",
};
const dmShare = (over: Record<string, unknown> = {}) => ({
  type: "message",
  subtype: "file_share",
  channel_type: "im",
  user: "UALICE",
  text: "",
  ts: "1.1",
  channel: "D1",
  team: "T1",
  files: [clipFile],
  ...over,
});
const mentionShare = (over: Record<string, unknown> = {}) => ({
  type: "app_mention",
  user: "UALICE",
  text: "<@UBOT>",
  ts: "2.1",
  thread_ts: "1.0",
  channel: "C1",
  team: "T1",
  files: [clipFile],
  ...over,
});

describe("slack voice: events", () => {
  const self = { userId: "UBOT" };
  const env = (event: Record<string, unknown>): EventEnvelope => ({ type: "event_callback", team_id: "T1", event_id: "Ev1", event });
  const on = { voice: true };

  it("reads a DM clip as a voice message, sized from Slack's metadata", () => {
    expect(readEvent(env(dmShare()), self, on)).toEqual({
      kind: "voice",
      fields: { team: "T1", speakerTeam: "T1", otherTeams: [], user: "UALICE", channel: "D1", channelType: "im", threadTs: undefined, ts: "1.1" },
      voice: { kind: "clip", ref: { id: "F1", url: CLIP_URL }, mime: "audio/webm", seconds: 3.5, bytes: 4000 },
    });
  });

  it("reads an audio file someone shared as an audio_file, and a clip mentioning the bot in a channel thread", () => {
    const mp3 = { id: "F2", mimetype: "audio/mpeg", size: 9000, url_private_download: `${FILES_ORIGIN}/files-pri/T1-F2/download/memo.mp3` };
    expect(readEvent(env(dmShare({ files: [mp3] })), self, on)).toMatchObject({
      kind: "voice",
      voice: { kind: "audio_file", ref: { id: "F2" }, mime: "audio/mpeg", bytes: 9000 },
    });
    for (const e of [mentionShare(), mentionShare({ subtype: "file_share" })]) {
      expect(readEvent(env(e), self, on)).toMatchObject({ kind: "voice", fields: { channel: "C1", channelType: "channel", threadTs: "1.0" }, voice: { kind: "clip" } });
    }
  });

  it("leaves out a length or size Slack does not give as a number", () => {
    const r = readEvent(env(dmShare({ files: [{ ...clipFile, size: "big", duration_ms: -1 }] })), self, on);
    expect(r.kind === "voice" && r.voice).toEqual({ kind: "clip", ref: { id: "F1", url: CLIP_URL }, mime: "audio/webm" });
  });

  it("drops the bot's own upload and every bot's, before the file_share subtype is let through", () => {
    for (const e of [dmShare({ user: "UBOT" }), dmShare({ bot_id: "BBOT" }), dmShare({ bot_id: "B2", user: "UOTHER" }), mentionShare({ user: "UBOT" })]) {
      expect(readEvent(env(e), self, on)).toEqual({ kind: "self_or_bot" });
    }
  });

  it("still drops every other subtype, and a file_share that is not exactly one audio file", () => {
    const image = { id: "F3", mimetype: "image/png", url_private_download: `${FILES_ORIGIN}/x.png` };
    const video = { id: "F4", subtype: "slack_video", mimetype: "video/mp4", url_private_download: `${FILES_ORIGIN}/x.mp4` };
    for (const e of [
      dmShare({ subtype: "message_changed" }),
      dmShare({ subtype: "bot_message" }),
      dmShare({ subtype: "thread_broadcast" }),
      dmShare({ subtype: "file_comment" }),
      dmShare({ files: [image] }),
      dmShare({ files: [video] }),
      dmShare({ files: [clipFile, clipFile] }),
      dmShare({ files: [] }),
      dmShare({ files: undefined }),
      dmShare({ files: [{ ...clipFile, id: undefined }] }),
    ]) {
      expect(readEvent(env(e), self, on)).toEqual({ kind: "self_or_bot" });
    }
  });

  it("ignores a channel's plain clip that does not mention the bot, as it ignores plain text there", () => {
    expect(readEvent(env(dmShare({ channel_type: "channel", channel: "C1" })), self, on)).toEqual({ kind: "ignore" });
  });

  it("reads everything as before voice existed when voice is off", () => {
    expect(readEvent(env(dmShare()), self)).toEqual({ kind: "self_or_bot" });
    expect(readEvent(env(mentionShare({ text: "<@UBOT> hi" })), self)).toMatchObject({ kind: "message", text: "hi" });
  });
});

const SECRET = "test-signing-secret";

function adapterWith(env: Record<string, string>, slack: FakeSlack, logger: Logger = silentLogger) {
  const cfg = loadConfig({
    LOOMCYCLE_BASE_URL: "http://loomcycle.test",
    LOOMCYCLE_TOKEN: "admin-token",
    BRIDGE_AGENT: "helper",
    SLACK_BOT_TOKEN: "xoxb-test",
    SLACK_SIGNING_SECRET: SECRET,
    SLACK_BRIDGE_ALLOWED_TEAMS: "T1",
    SLACK_BRIDGE_API_BASE_URL: slack.base,
    ...env,
  });
  return createSlackAdapter(cfg, { fetch: slack.fetch, logger });
}

function signedEvent(event: Record<string, unknown>) {
  const body = JSON.stringify({ type: "event_callback", team_id: "T1", event_id: `Ev-${Math.random()}`, event });
  const ts = String(Math.floor(Date.now() / 1000));
  return {
    method: "POST",
    path: "/slack/events",
    query: new URLSearchParams(),
    headers: { "x-slack-request-timestamp": ts, "x-slack-signature": slackSignature(SECRET, ts, body) },
    body: Buffer.from(body),
  };
}

describe("slack voice: adapter", () => {
  /** A router that only records what reaches it. */
  function spyRouter() {
    const voices: unknown[][] = [];
    const messages: unknown[][] = [];
    const router = {
      onVoice: async (...a: unknown[]) => void voices.push(a),
      onMessage: async (...a: unknown[]) => void messages.push(a),
    } as unknown as BridgeContext<SlackRef>["router"];
    return { router, voices, messages };
  }

  it("routes a DM clip to the core's onVoice with its VoiceIn, its identity and its reply reference", async () => {
    const slack = new FakeSlack();
    const adapter = adapterWith({ BRIDGE_VOICE: "1" }, slack);
    await adapter.learnSelf();
    const spy = spyRouter();
    await adapter.routes({ router: spy.router, log: silentLogger })["POST /slack/events"](signedEvent(dmShare()));
    await settle();

    expect(spy.messages).toEqual([]);
    expect(spy.voices).toHaveLength(1);
    const [id, ref, voiceIn, locale] = spy.voices[0]!;
    expect(id).toMatchObject({ ok: true, scope: "personal", speaker: { subject: "slack-t1-ualice", trust: "trusted" } });
    expect(ref).toEqual({ team: "T1", channel: "D1", channelType: "im", messageTs: "1.1" });
    expect(voiceIn).toEqual({ kind: "clip", ref: { id: "F1", url: CLIP_URL }, mime: "audio/webm", seconds: 3.5, bytes: 4000 });
    expect(locale).toBeUndefined();
  });

  it("never routes a clip with voice off, and offers the core a voice port either way", async () => {
    const slack = new FakeSlack();
    const adapter = adapterWith({}, slack);
    await adapter.learnSelf();
    const spy = spyRouter();
    await adapter.routes({ router: spy.router, log: silentLogger })["POST /slack/events"](signedEvent(dmShare()));
    await settle();
    expect(spy.voices).toEqual([]);
    expect(spy.messages).toEqual([]);
    expect(adapter.voice?.capabilities.out).toBe("file");
  });

  it("warns at start when voice is on and the token lacks files:read or files:write, and still starts", async () => {
    const warnings: Array<{ msg: string; attrs?: Record<string, unknown> }> = [];
    const logger: Logger = { info() {}, error() {}, warn: (msg, attrs) => void warnings.push({ msg, attrs }) };
    const voiceWarnings = () => warnings.filter((w) => w.msg.includes("BRIDGE_VOICE"));

    const slack = new FakeSlack();
    slack.reply("auth.test", { headers: { "x-oauth-scopes": "chat:write,im:history,files:read" }, json: { ok: true, user_id: "UBOT", team_id: "T1" } });
    expect(await adapterWith({ BRIDGE_VOICE: "1" }, slack, logger).learnSelf()).toEqual({ userId: "UBOT" });
    expect(voiceWarnings()).toEqual([{ msg: expect.any(String), attrs: { missing: "files:write" } }]);

    warnings.length = 0;
    slack.reply("auth.test", { headers: { "x-oauth-scopes": "chat:write, files:read, files:write" }, json: { ok: true, user_id: "UBOT", team_id: "T1" } });
    await adapterWith({ BRIDGE_VOICE: "1" }, slack, logger).learnSelf();
    // Voice off: the scopes are not its concern. No header: nothing is known.
    slack.reply("auth.test", { headers: { "x-oauth-scopes": "chat:write" }, json: { ok: true, user_id: "UBOT", team_id: "T1" } });
    await adapterWith({}, slack, logger).learnSelf();
    await adapterWith({ BRIDGE_VOICE: "1" }, slack, logger).learnSelf();
    expect(voiceWarnings()).toEqual([]);
  });
});
