// Voice messages (RFC GA §5.3): the Discord side of the core's VoicePort.
//
// In: a person picks "Ask by voice" on their own voice message (a message
// command; the bridge has no Gateway to see messages). The recording is the
// message's attachment on Discord's CDN, at a signed URL that needs no token.
//
// Out: a voice message is a bot-token channel message with flag
// IS_VOICE_MESSAGE and one OGG/Opus attachment. It may carry no text, so it
// replies to the text reply, whose last line says the voice is generated.

import { downloadPinned, PinnedDownloadError, VoiceRefusedError, type VoiceIn, type VoiceOut, type VoicePort } from "@loomcycle/bridge-core";

import { DiscordApiError, USER_AGENT, type DiscordApi } from "./api.js";
import type { Interaction } from "./interaction.js";
import type { DiscordPort, DiscordRef } from "./port.js";

export const IS_VOICE_MESSAGE = 1 << 13;
/** The hosts Discord serves attachments from. Nothing is fetched elsewhere. */
export const VOICE_HOSTS: ReadonlySet<string> = new Set(["cdn.discordapp.com", "media.discordapp.net"]);
/** Used until an interaction in the channel says otherwise
 *  (attachment_size_limit): the lowest free-tier limit Discord has had. */
export const DEFAULT_MAX_UPLOAD_BYTES = 10 * 1024 * 1024;
export const VOICE_FILENAME = "voice-message.ogg";
/** Decision D10: Discord is sent an empty waveform, as community clients do,
 *  unless P0 shows it refuses one. The client then draws a flat bar. The seam
 *  for a real one is here: loomcycle may add an X-Loomcycle-Speech-Waveform
 *  header (256 one-byte samples, base64), which would replace this constant. */
export const WAVEFORM = "";
const MAX_LIMITS = 10_000;
/** Discord's error codes for "not here": Missing Permissions (the bot lacks
 *  SEND_VOICE_MESSAGES or ATTACH_FILES), Cannot send messages to this user,
 *  You cannot send voice messages in this channel, and 40094 as RFC GA names
 *  it (verify in P0). */
const REFUSED_CODES = new Set([50013, 50007, 50173, 40094]);
const AUDIO_TYPE = /^(audio\/[\w.+-]+|video\/[\w.+-]+|application\/ogg|application\/octet-stream)$/i;

export const NEEDS_VOICE = "Ask by voice works on a voice message, or a message with one audio file.";
export const OWN_VOICE_ONLY = "You can ask by voice only with your own voice message.";
export const VOICE_OFF = "Voice messages aren't turned on for this bot.";

/** voiceOfMessage reads the recording an Ask by voice interaction points at:
 *  a voice message, or a message whose one attachment is audio.
 *
 *  Only the person who recorded it may ask with it. The words are its
 *  author's; taken from someone else they would be the invoker's request in
 *  another person's voice, run under the invoker's trust, and they would
 *  send a person's recording to transcription without that person asking.
 *  This also keeps the bridge from transcribing its own voice replies. */
export function voiceOfMessage(ix: Interaction): { voice: VoiceIn } | { refusal: string } {
  const targetId = ix.data?.target_id;
  const msg = typeof targetId === "string" ? ix.data?.resolved?.messages?.[targetId] : undefined;
  const attachments = Array.isArray(msg?.attachments) ? msg.attachments : [];
  if (!msg || attachments.length !== 1) return { refusal: NEEDS_VOICE };
  const a = attachments[0]!;
  const flagged = typeof msg.flags === "number" && (msg.flags & IS_VOICE_MESSAGE) !== 0;
  const mime = baseMime(a.content_type);
  if (!flagged && !mime?.startsWith("audio/")) return { refusal: NEEDS_VOICE };
  const invoker = (ix.member?.user ?? ix.user)?.id;
  if (!invoker || msg.author?.id !== invoker || msg.author.bot) return { refusal: OWN_VOICE_ONLY };
  if (!voiceUrl(a.url)) return { refusal: NEEDS_VOICE };
  const finite = (n: unknown) => (typeof n === "number" && Number.isFinite(n) && n >= 0 ? n : undefined);
  const seconds = finite(a.duration_secs);
  const bytes = finite(a.size);
  return {
    voice: {
      kind: flagged ? "voice" : "audio_file",
      ref: a.url,
      mime: mime ?? "audio/ogg",
      ...(seconds !== undefined ? { seconds } : {}),
      ...(bytes !== undefined ? { bytes } : {}),
    },
  };
}

/** voiceUrl is an attachment URL the bridge may fetch: https, on Discord's
 *  CDN, with no credentials or port in it. */
export function voiceUrl(raw: unknown): URL | undefined {
  if (typeof raw !== "string") return undefined;
  let u: URL;
  try {
    u = new URL(raw);
  } catch {
    return undefined;
  }
  if (u.protocol !== "https:" || u.username || u.password || u.port) return undefined;
  return VOICE_HOSTS.has(u.hostname.toLowerCase()) ? u : undefined;
}

/** baseMime is a content type without its parameters, lower-cased. */
export function baseMime(raw: unknown): string | undefined {
  if (typeof raw !== "string") return undefined;
  const m = raw.split(";")[0]!.trim().toLowerCase();
  return m || undefined;
}

/** oggSeconds reads an Ogg Opus stream's length from its last page's granule
 *  position, less the pre-skip in its OpusHead (Opus is always 48 kHz), for
 *  when loomcycle sent no duration. undefined when it is not Ogg Opus. */
export function oggSeconds(b: Uint8Array): number | undefined {
  const view = new DataView(b.buffer, b.byteOffset, b.byteLength);
  // "OggS", then stream structure version 0.
  const isPage = (i: number) => b[i] === 0x4f && b[i + 1] === 0x67 && b[i + 2] === 0x67 && b[i + 3] === 0x53 && b[i + 4] === 0;
  let head = -1;
  for (let i = 0; i + 19 <= b.length && i < 512; i++) {
    if (String.fromCharCode(...b.subarray(i, i + 8)) === "OpusHead") {
      head = i;
      break;
    }
  }
  if (head < 0) return undefined;
  const preSkip = view.getUint16(head + 10, true);
  for (let i = b.length - 27; i >= 0; i--) {
    if (!isPage(i)) continue;
    const granule = view.getBigUint64(i + 6, true);
    // All ones: no packet ends on this page.
    if (granule === 0xffff_ffff_ffff_ffffn) continue;
    const samples = Number(granule) - preSkip;
    return samples > 0 ? Math.round((samples / 48_000) * 100) / 100 : undefined;
  }
  return undefined;
}

/** discordMessageOf is the message a refused download had before the
 *  download moved into bridge-core. */
function discordMessageOf(err: PinnedDownloadError): string {
  switch (err.reason) {
    case "url":
    case "host":
      return err.detail.redirected ? "discord voice download: redirected off Discord's CDN" : "discord voice download: not a Discord attachment URL";
    case "redirect":
      return "discord voice download: redirected off Discord's CDN";
    case "status":
      return `discord voice download: http ${err.detail.status}`;
    case "type":
      return "discord voice download: not audio";
    case "size":
      return "discord voice download: larger than allowed";
  }
}

export interface DiscordVoiceOptions {
  /** The fetch attachments are downloaded with (tests). */
  fetch?: typeof fetch;
}

/** DiscordVoice is the bridge's VoicePort. Its errors never carry the
 *  attachment URL: its signature is good until it expires. */
export class DiscordVoice implements VoicePort<DiscordRef> {
  readonly capabilities = { voiceFormats: ["ogg" as const], out: "native" as const, maxUploadBytes: DEFAULT_MAX_UPLOAD_BYTES };
  /** attachment_size_limit by channel, from the interactions seen there. */
  private readonly limits = new Map<string, number>();
  private readonly fetch: typeof fetch;

  constructor(
    private readonly api: DiscordApi,
    private readonly port: DiscordPort,
    opts: DiscordVoiceOptions = {},
  ) {
    this.fetch = opts.fetch ?? globalThis.fetch;
  }

  /** observe records the upload limit an interaction reports for its channel. */
  observe(channelId: string, attachmentSizeLimit: unknown): void {
    if (typeof attachmentSizeLimit !== "number" || !Number.isSafeInteger(attachmentSizeLimit) || attachmentSizeLimit <= 0) return;
    if (this.limits.size >= MAX_LIMITS) this.limits.clear();
    this.limits.set(channelId, attachmentSizeLimit);
  }

  async download(v: VoiceIn, maxBytes: number, signal: AbortSignal): Promise<{ bytes: Uint8Array; mime: string; name?: string }> {
    const first = voiceUrl(v.ref);
    if (!first) throw new Error("discord voice download: not a Discord attachment URL");
    let got: { bytes: Uint8Array; contentType: string };
    try {
      // No Authorization header: the URL is signed, and the bot token is for
      // Discord's API only. Every hop must be on Discord's CDN.
      got = await downloadPinned(first.href, {
        allowed: (u) => voiceUrl(u.href) !== undefined,
        headers: { "user-agent": USER_AGENT },
        accept: (ct) => AUDIO_TYPE.test(baseMime(ct) ?? ""),
        maxBytes,
        signal,
        fetch: this.fetch,
      });
    } catch (err) {
      throw err instanceof PinnedDownloadError ? new Error(discordMessageOf(err)) : err;
    }
    const served = baseMime(got.contentType)!;
    const mime = v.mime ?? (served.startsWith("audio/") ? served : "audio/ogg");
    return { bytes: got.bytes, mime, name: VOICE_FILENAME };
  }

  async sendVoice(ref: DiscordRef, audio: VoiceOut): Promise<void> {
    // pickFormat offers Discord only ogg; anything else would not show as voice.
    if (baseMime(audio.mime) !== "audio/ogg") throw new Error("discord voice: only audio/ogg is a voice message");
    const limit = this.limits.get(ref.channelId) ?? DEFAULT_MAX_UPLOAD_BYTES;
    if (audio.bytes.byteLength > limit) throw new Error("discord voice: larger than this channel takes");
    const seconds = audio.seconds ?? oggSeconds(audio.bytes);
    if (seconds === undefined || !(seconds > 0)) throw new Error("discord voice: no duration");
    try {
      const replyTo = await this.port.labelLast(ref, audio.label);
      await this.api.createVoiceMessage(
        ref.channelId,
        {
          flags: IS_VOICE_MESSAGE,
          attachments: [{ id: 0, filename: VOICE_FILENAME, duration_secs: seconds, waveform: WAVEFORM }],
          message_reference: { message_id: replyTo, fail_if_not_exists: false },
        },
        { bytes: audio.bytes, filename: VOICE_FILENAME, contentType: "audio/ogg" },
      );
    } catch (err) {
      if (err instanceof DiscordApiError && (err.status === 403 || (err.code !== undefined && REFUSED_CODES.has(err.code)))) {
        throw new VoiceRefusedError(`discord refused the voice message (http ${err.status}${err.code !== undefined ? `, code ${err.code}` : ""})`);
      }
      throw err;
    }
  }

  async indicate(ref: DiscordRef): Promise<void> {
    // The port skips it while a deferred response shows Discord's own
    // "thinking…", and backs off a channel that refuses it.
    await this.port.send(ref, { type: "typing" });
  }
}
