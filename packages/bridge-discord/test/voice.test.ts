// The Discord side of voice messages (RFC GA §5.3): which message Ask by
// voice takes, the download from Discord's CDN, and the voice message out.

import { VoiceRefusedError, type Logger, type VoiceOut } from "@loomcycle/bridge-core";
import { describe, expect, it } from "vitest";

import { DiscordApi } from "../src/api.js";
import { ASK_BY_VOICE, commandSpecs, registerCommands } from "../src/commands.js";
import type { Interaction } from "../src/interaction.js";
import { DiscordPort } from "../src/port.js";
import {
  DEFAULT_MAX_UPLOAD_BYTES,
  DiscordVoice,
  IS_VOICE_MESSAGE,
  NEEDS_VOICE,
  oggSeconds,
  OWN_VOICE_ONLY,
  voiceOfMessage,
  voiceUrl,
} from "../src/voice.js";
import { FakeDiscord } from "./discord.js";
import { silentLogger } from "./fakes.js";

const APP = "100000000000000001";
const GUILD = "200000000000000002";
const CHANNEL = "300000000000000003";
const ALICE = "500000000000000005";
const BOB = "500000000000000006";
const MSG = "600000000000000006";
const CDN_URL = "https://cdn.discordapp.com/attachments/3/6/voice-message.ogg?ex=1&is=2&hm=abc";

type Attachment = NonNullable<NonNullable<NonNullable<Interaction["data"]>["resolved"]>["messages"]>[string]["attachments"];

function askByVoice(over: { author?: string; bot?: boolean; flags?: number; attachments?: Attachment; invoker?: string } = {}): Interaction {
  return {
    id: "900000000000000001",
    type: 2,
    token: "itok",
    guild_id: GUILD,
    channel: { id: CHANNEL, type: 0 },
    member: { user: { id: over.invoker ?? ALICE } },
    locale: "uk",
    data: {
      type: 3,
      name: ASK_BY_VOICE,
      target_id: MSG,
      resolved: {
        messages: {
          [MSG]: {
            id: MSG,
            author: { id: over.author ?? ALICE, bot: over.bot },
            flags: over.flags ?? IS_VOICE_MESSAGE,
            attachments: over.attachments ?? [{ id: "1", url: CDN_URL, content_type: "audio/ogg", size: 9000, duration_secs: 4.2 }],
          },
        },
      },
    },
  };
}

/** A minimal Ogg Opus stream: an OpusHead page, then a last page whose
 *  granule position is pre-skip + 3 s at 48 kHz. */
function oggOpus(seconds: number, preSkip = 312): Uint8Array {
  const page = (granule: bigint, payload: Uint8Array) => {
    const b = new Uint8Array(27 + payload.length);
    b.set([0x4f, 0x67, 0x67, 0x53, 0]);
    new DataView(b.buffer).setBigUint64(6, granule, true);
    b.set(payload, 27);
    return b;
  };
  const head = new Uint8Array(19);
  head.set(new TextEncoder().encode("OpusHead"));
  head[8] = 1;
  head[9] = 1;
  new DataView(head.buffer).setUint16(10, preSkip, true);
  const first = page(0n, head);
  const middle = page(0xffff_ffff_ffff_ffffn, new Uint8Array(40));
  const last = page(BigInt(Math.round(seconds * 48_000) + preSkip), new Uint8Array(40));
  const out = new Uint8Array(first.length + middle.length + last.length);
  out.set(first);
  out.set(last, first.length);
  // The "no packet ends here" page last of all, as an encoder may leave it.
  out.set(middle, first.length + last.length);
  return out;
}

describe("Ask by voice: which message it takes", () => {
  it("takes the invoker's own voice message, with its metadata, as kind voice", () => {
    expect(voiceOfMessage(askByVoice())).toEqual({
      voice: { kind: "voice", ref: CDN_URL, mime: "audio/ogg", seconds: 4.2, bytes: 9000 },
    });
  });

  it("takes the invoker's message with one audio file as audio_file, its type without parameters", () => {
    const ix = askByVoice({ flags: 0, attachments: [{ url: CDN_URL, content_type: "audio/mpeg; charset=binary", size: 10 }] });
    expect(voiceOfMessage(ix)).toEqual({ voice: { kind: "audio_file", ref: CDN_URL, mime: "audio/mpeg", bytes: 10 } });
  });

  it("defaults a voice message with no content type to audio/ogg", () => {
    const ix = askByVoice({ attachments: [{ url: CDN_URL }] });
    expect(voiceOfMessage(ix)).toEqual({ voice: { kind: "voice", ref: CDN_URL, mime: "audio/ogg" } });
  });

  it("refuses a message that is not voice: text only, an image, two files", () => {
    expect(voiceOfMessage(askByVoice({ flags: 0, attachments: [] }))).toEqual({ refusal: NEEDS_VOICE });
    expect(voiceOfMessage(askByVoice({ flags: 0, attachments: [{ url: CDN_URL, content_type: "image/png" }] }))).toEqual({ refusal: NEEDS_VOICE });
    const two = [{ url: CDN_URL, content_type: "audio/ogg" }, { url: CDN_URL, content_type: "audio/ogg" }];
    expect(voiceOfMessage(askByVoice({ attachments: two }))).toEqual({ refusal: NEEDS_VOICE });
    const ix = askByVoice();
    ix.data!.target_id = "600000000000000999";
    expect(voiceOfMessage(ix)).toEqual({ refusal: NEEDS_VOICE });
  });

  it("refuses someone else's voice message, and the bot's own voice reply", () => {
    expect(voiceOfMessage(askByVoice({ author: BOB }))).toEqual({ refusal: OWN_VOICE_ONLY });
    expect(voiceOfMessage(askByVoice({ author: APP, bot: true }))).toEqual({ refusal: OWN_VOICE_ONLY });
  });

  it("refuses an attachment that is not on Discord's CDN", () => {
    const ix = askByVoice({ attachments: [{ url: "https://evil.example/a.ogg", content_type: "audio/ogg" }] });
    expect(voiceOfMessage(ix)).toEqual({ refusal: NEEDS_VOICE });
  });
});

describe("voiceUrl", () => {
  it("allows https on Discord's two CDN hosts only, with no credentials or port", () => {
    expect(voiceUrl(CDN_URL)?.hostname).toBe("cdn.discordapp.com");
    expect(voiceUrl("https://media.discordapp.net/attachments/1/2/a.ogg")?.hostname).toBe("media.discordapp.net");
    for (const bad of [
      "http://cdn.discordapp.com/a.ogg",
      "https://cdn.discordapp.com.evil.example/a.ogg",
      "https://evil.example/cdn.discordapp.com/a.ogg",
      "https://user:pw@cdn.discordapp.com/a.ogg",
      "https://cdn.discordapp.com:8443/a.ogg",
      "file:///etc/passwd",
      "not a url",
      42,
    ]) {
      expect(voiceUrl(bad)).toBeUndefined();
    }
  });
});

describe("download", () => {
  const setup = (fetchImpl?: typeof fetch) => {
    const d = new FakeDiscord();
    const api = new DiscordApi({ baseUrl: d.base, applicationId: APP, botToken: "bot", log: silentLogger, fetch: d.fetch });
    const voice = new DiscordVoice(api, new DiscordPort(api, silentLogger), { fetch: fetchImpl ?? d.fetch });
    return { d, voice };
  };
  const signal = () => AbortSignal.timeout(5000);
  const note = { kind: "voice" as const, ref: CDN_URL, mime: "audio/ogg" };

  it("fetches the signed URL with no credential and no automatic redirect", async () => {
    const { d, voice } = setup();
    d.files.set(CDN_URL, { body: new Uint8Array([1, 2, 3]) });
    const got = await voice.download(note, 100, signal());
    expect(got).toEqual({ bytes: new Uint8Array([1, 2, 3]), mime: "audio/ogg", name: "voice-message.ogg" });
    expect(d.downloads).toEqual([{ url: CDN_URL, authorization: undefined, redirect: "manual" }]);
    expect(d.calls).toHaveLength(0);
  });

  it("refuses a URL off Discord's CDN without fetching it", async () => {
    const { d, voice } = setup();
    await expect(voice.download({ ...note, ref: "https://evil.example/a.ogg" }, 100, signal())).rejects.toThrow(/not a Discord attachment/);
    expect(d.downloads).toHaveLength(0);
  });

  it("follows a redirect within the CDN and refuses one off it", async () => {
    const { d, voice } = setup();
    const moved = "https://media.discordapp.net/attachments/3/6/v.ogg";
    d.files.set(CDN_URL, { status: 302, headers: { location: moved } });
    d.files.set(moved, { body: new Uint8Array([7]) });
    expect((await voice.download(note, 100, signal())).bytes).toEqual(new Uint8Array([7]));

    d.files.set(CDN_URL, { status: 302, headers: { location: "https://evil.example/v.ogg" } });
    await expect(voice.download(note, 100, signal())).rejects.toThrow(/redirected off/);
    expect(d.downloads.map((x) => x.url)).not.toContain("https://evil.example/v.ogg");
  });

  it("refuses more than maxBytes, by Content-Length and while streaming, without reading the rest", async () => {
    const { d, voice } = setup();
    d.files.set(CDN_URL, { body: new Uint8Array(101), headers: { "content-type": "audio/ogg", "content-length": "101" } });
    await expect(voice.download(note, 100, signal())).rejects.toThrow(/larger than allowed/);

    // No Content-Length: the stream is cut once it passes the cap.
    let pulled = 0;
    const streaming = (async () => {
      const body = new ReadableStream<Uint8Array>({
        pull(c) {
          pulled++;
          c.enqueue(new Uint8Array(40));
          if (pulled >= 100) c.close();
        },
      });
      return new Response(body, { status: 200, headers: { "content-type": "audio/ogg" } });
    }) as typeof fetch;
    const s = setup(streaming);
    await expect(s.voice.download(note, 100, signal())).rejects.toThrow(/larger than allowed/);
    expect(pulled).toBeLessThan(10);
  });

  it("refuses an answer that is not audio", async () => {
    const { d, voice } = setup();
    d.files.set(CDN_URL, { body: new TextEncoder().encode("<html>"), headers: { "content-type": "text/html; charset=utf-8" } });
    await expect(voice.download(note, 100, signal())).rejects.toThrow(/not audio/);
  });

  it("names neither the signed URL nor any token in its errors", async () => {
    const { d, voice } = setup();
    d.files.set(CDN_URL, { status: 403, body: new Uint8Array() });
    const err = (await voice.download(note, 100, signal()).catch((e: unknown) => e)) as Error;
    expect(err.message).toMatch(/http 403/);
    expect(err.message).not.toMatch(/hm=|discordapp/);
  });
});

describe("sendVoice", () => {
  const ogg = oggOpus(3);
  const audio = (over: Partial<VoiceOut> = {}): VoiceOut => ({ bytes: ogg, mime: "audio/ogg", seconds: 2.5, label: "AI voice", ...over });
  const setup = () => {
    const d = new FakeDiscord();
    const api = new DiscordApi({ baseUrl: d.base, applicationId: APP, botToken: "bot", log: silentLogger, fetch: d.fetch });
    const warnings: string[] = [];
    const log: Logger = { ...silentLogger, warn: (m) => warnings.push(m) };
    const port = new DiscordPort(api, log);
    const voice = new DiscordVoice(api, port, { fetch: d.fetch });
    return { d, port, voice, warnings };
  };

  it("posts a voice message with the bot token: flag 8192, one ogg file, no text, replying to the labelled reply", async () => {
    const { d, port, voice } = setup();
    port.track("i1", "tok1", CHANNEL, { fallback: "Done." });
    await port.send({ channelId: CHANNEL }, { type: "message", text: "Sunny, 21 degrees." });
    // FakeDiscord numbers the messages it creates from 700000000000000001.
    const replyId = "700000000000000001";
    await voice.sendVoice({ channelId: CHANNEL }, audio());

    // The reply's text gets the label as its last line, through the token it went with.
    expect(d.calls[1]).toMatchObject({
      method: "PATCH",
      path: `/webhooks/${APP}/tok1/messages/@original`,
      authorization: undefined,
      body: { content: "Sunny, 21 degrees.\n-# AI voice", allowed_mentions: { parse: [] } },
    });
    const post = d.calls[2]!;
    expect(post).toMatchObject({ method: "POST", path: `/channels/${CHANNEL}/messages`, authorization: "Bot bot", body: undefined });
    expect(post.form!.payload).toEqual({
      flags: IS_VOICE_MESSAGE,
      attachments: [{ id: 0, filename: "voice-message.ogg", duration_secs: 2.5, waveform: "" }],
      message_reference: { message_id: replyId, fail_if_not_exists: false },
    });
    expect(post.form!.payload).not.toHaveProperty("content");
    expect(post.form!.payload).not.toHaveProperty("embeds");
    expect(post.form!.payload).not.toHaveProperty("components");
    expect(post.form!.files).toEqual([{ field: "files[0]", name: "voice-message.ogg", type: "audio/ogg", bytes: ogg }]);
    port.stop();
  });

  it("labels a reply sent with the bot token by editing it with the bot token", async () => {
    const { d, port, voice } = setup();
    await port.send({ channelId: CHANNEL }, { type: "message", text: "late reply" });
    await voice.sendVoice({ channelId: CHANNEL }, audio());
    expect(d.calls.map((c) => [c.method, c.path, c.authorization])).toEqual([
      ["POST", `/channels/${CHANNEL}/messages`, "Bot bot"],
      ["PATCH", `/channels/${CHANNEL}/messages/700000000000000001`, "Bot bot"],
      ["POST", `/channels/${CHANNEL}/messages`, "Bot bot"],
    ]);
  });

  it("posts the label on its own, and replies to it, when the reply text cannot carry it", async () => {
    const { d, port, voice, warnings } = setup();
    // No text before it at all.
    await voice.sendVoice({ channelId: CHANNEL }, audio({ label: "AI voice · first part" }));
    expect(d.calls[0]).toMatchObject({ method: "POST", path: `/channels/${CHANNEL}/messages`, body: { content: "-# AI voice · first part" } });
    expect((d.calls[1]!.form!.payload as { message_reference: unknown }).message_reference).toEqual({ message_id: "700000000000000001", fail_if_not_exists: false });

    // A full-length reply: no room for the line.
    d.calls.length = 0;
    await port.send({ channelId: CHANNEL }, { type: "message", text: "x".repeat(2000) });
    await voice.sendVoice({ channelId: CHANNEL }, audio());
    expect(d.calls.map((c) => [c.method, (c.body as { content?: string } | undefined)?.content?.slice(0, 12)])).toEqual([
      ["POST", "xxxxxxxxxxxx"],
      ["POST", "-# AI voice"],
      ["POST", undefined],
    ]);

    // The edit fails: the label still goes out.
    d.calls.length = 0;
    await port.send({ channelId: CHANNEL }, { type: "message", text: "short" });
    d.on(/^PATCH/, { status: 500 });
    await voice.sendVoice({ channelId: CHANNEL }, audio());
    expect(d.calls.map((c) => c.method)).toEqual(["POST", "PATCH", "POST", "POST"]);
    expect(warnings).toContain("could not label the reply text; posting the label on its own");
  });

  it("never posts an unlabelled voice message", async () => {
    const { d, voice } = setup();
    d.on(/^POST \/channels\/\d+\/messages$/, { status: 500 });
    await expect(voice.sendVoice({ channelId: CHANNEL }, audio())).rejects.toThrow();
    expect(d.calls.filter((c) => c.form)).toHaveLength(0);
  });

  it("maps a missing permission or a forbidden channel to VoiceRefusedError, and other failures to plain errors", async () => {
    for (const reply of [
      { status: 403, body: { code: 50013, message: "Missing Permissions" } },
      { status: 400, body: { code: 50173 } },
      { status: 403, body: { code: 50001 } },
    ]) {
      const { d, port, voice } = setup();
      await port.send({ channelId: CHANNEL }, { type: "message", text: "the reply" });
      d.on(/^POST \/channels\/\d+\/messages$/, reply);
      const err = await voice.sendVoice({ channelId: CHANNEL }, audio()).catch((e: unknown) => e);
      expect(err).toBeInstanceOf(VoiceRefusedError);
    }
    const { d, port, voice } = setup();
    await port.send({ channelId: CHANNEL }, { type: "message", text: "the reply" });
    d.on(/^POST \/channels\/\d+\/messages$/, { status: 500 });
    const err = await voice.sendVoice({ channelId: CHANNEL }, audio()).catch((e: unknown) => e);
    expect(err).toBeInstanceOf(Error);
    expect(err).not.toBeInstanceOf(VoiceRefusedError);
  });

  it("reads the duration from the Ogg stream when loomcycle sent none", async () => {
    expect(oggSeconds(ogg)).toBe(3);
    expect(oggSeconds(new Uint8Array([1, 2, 3]))).toBeUndefined();
    const { d, voice } = setup();
    await voice.sendVoice({ channelId: CHANNEL }, audio({ seconds: undefined }));
    const payload = d.calls.find((c) => c.form)!.form!.payload as { attachments: Array<{ duration_secs: number }> };
    expect(payload.attachments[0]!.duration_secs).toBe(3);
  });

  it("refuses audio that is not ogg, or larger than the channel takes, before calling Discord", async () => {
    const { d, voice } = setup();
    await expect(voice.sendVoice({ channelId: CHANNEL }, audio({ mime: "audio/mpeg" }))).rejects.toThrow(/only audio\/ogg/);
    voice.observe(CHANNEL, ogg.length - 1);
    await expect(voice.sendVoice({ channelId: CHANNEL }, audio())).rejects.toThrow(/larger than this channel takes/);
    expect(d.calls).toHaveLength(0);
    expect(voice.capabilities).toEqual({ voiceFormats: ["ogg"], out: "native", maxUploadBytes: DEFAULT_MAX_UPLOAD_BYTES });
  });
});

describe("Ask by voice registration", () => {
  it("registers the message command beside the slash commands only with voice on", async () => {
    expect(commandSpecs([0]).map((c) => c.name)).toEqual(["ask", "new", "stop", "help"]);
    const specs = commandSpecs([0], { voice: true });
    expect(specs.map((c) => [c.type, c.name])).toEqual([
      [1, "ask"],
      [1, "new"],
      [1, "stop"],
      [1, "help"],
      [3, "Ask by voice"],
    ]);
    const msg = specs[4]!;
    // Discord: a message command has an empty description and no options, and
    // a name of 1–32 characters.
    expect(msg).toEqual({ type: 3, name: ASK_BY_VOICE, description: "", contexts: [0], integration_types: [0] });
    expect(msg.name.length).toBeLessThanOrEqual(32);

    const d = new FakeDiscord();
    const api = new DiscordApi({ baseUrl: d.base, applicationId: APP, botToken: "bot", log: silentLogger, fetch: d.fetch });
    await registerCommands(api, { allowedGuilds: new Set([GUILD]), allowDms: true, voice: { enabled: true } }, silentLogger);
    for (const c of d.calls) expect((c.body as Array<{ name: string }>).map((s) => s.name)).toContain(ASK_BY_VOICE);
    d.calls.length = 0;
    await registerCommands(api, { allowedGuilds: new Set([GUILD]), allowDms: true, voice: { enabled: false } }, silentLogger);
    for (const c of d.calls) expect((c.body as Array<{ name: string }>).map((s) => s.name)).not.toContain(ASK_BY_VOICE);
  });
});
