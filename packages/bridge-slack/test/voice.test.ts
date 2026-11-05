// The Slack voice port (RFC GA): where the bot token may go on a download,
// what a download takes, and the three-step upload of a spoken reply.

import { VoiceRefusedError, type VoiceIn, type VoiceOut } from "@loomcycle/bridge-core";
import { beforeEach, describe, expect, it } from "vitest";

import { SlackApi } from "../src/api.js";
import type { SlackRef } from "../src/identity.js";
import { createSlackVoice, fileNameOf, MAX_UPLOAD_BYTES, SlackFileError } from "../src/voice.js";
import { FakeSlack, FILES_ORIGIN } from "./fakes.js";

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
