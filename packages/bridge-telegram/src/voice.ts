// Voice messages on Telegram (RFC GA): the platform side of the core's
// VoicePort. The core decides whether a voice note is taken, transcribes it
// and decides whether a reply is spoken; this file downloads the recording
// and posts the spoken reply as a native voice note.

import { VoiceRefusedError, type VoiceCapabilities, type VoiceIn, type VoiceOut, type VoicePort } from "@loomcycle/bridge-core";

import { TelegramError, type TelegramApi } from "./api.js";
import type { TelegramRef } from "./identity.js";

/** The most a bot may download through the Bot API. */
export const MAX_BOT_DOWNLOAD_BYTES = 20 * 1024 * 1024;
/** The most sendVoice takes. */
export const MAX_VOICE_UPLOAD_BYTES = 50 * 1024 * 1024;
/** sendVoice's caption limit. */
const MAX_CAPTION = 1024;

/** sendVoice shows OGG/Opus, MP3 and M4A as a voice note; loomcycle returns
 *  ogg or mp3 (with speech.encode), never m4a. OGG first: it is what the
 *  Telegram clients record. */
export const TELEGRAM_VOICE_CAPABILITIES: VoiceCapabilities = {
  voiceFormats: ["ogg", "mp3"],
  out: "native",
  maxUploadBytes: MAX_VOICE_UPLOAD_BYTES,
};

export interface TelegramVoiceOptions {
  api: TelegramApi;
  /** The bridge's latest text message in the conversation, which the voice
   *  note replies to, if known. */
  replyTarget?: (ref: TelegramRef) => number | undefined;
}

function threadParams(ref: TelegramRef): Record<string, string> {
  return ref.threadId === undefined ? { chat_id: String(ref.chatId) } : { chat_id: String(ref.chatId), message_thread_id: String(ref.threadId) };
}

/** The name the recording is handed to loomcycle under. Made up from its
 *  kind and type, never Telegram's file_path, which names nothing useful. */
function fileName(kind: VoiceIn["kind"], mime: string): string {
  const ext = mime.includes("ogg") || mime.includes("opus") ? "ogg" : mime.includes("mpeg") || mime.includes("mp3") ? "mp3" : mime.includes("mp4") || mime.includes("m4a") ? "mp4" : "bin";
  return `${kind}.${ext}`;
}

export class TelegramVoice implements VoicePort<TelegramRef> {
  readonly capabilities = TELEGRAM_VOICE_CAPABILITIES;

  constructor(private readonly opts: TelegramVoiceOptions) {}

  async download(v: VoiceIn, maxBytes: number, signal: AbortSignal): Promise<{ bytes: Uint8Array; mime: string; name: string }> {
    if (typeof v.ref !== "string" || !v.ref) throw new TelegramError("getFile", 0, "no file id");
    const cap = Math.min(maxBytes, MAX_BOT_DOWNLOAD_BYTES);
    const file = await this.opts.api.call<{ file_path?: string; file_size?: number }>("getFile", { file_id: v.ref });
    if (typeof file?.file_path !== "string") throw new TelegramError("getFile", 0, "no file path");
    if (typeof file.file_size === "number" && file.file_size > cap) throw new TelegramError("file", 413, "file too large");
    const bytes = await this.opts.api.downloadFile(file.file_path, cap, signal);
    const mime = v.mime ?? (v.kind === "video_note" ? "video/mp4" : "audio/ogg");
    return { bytes, mime, name: fileName(v.kind, mime) };
  }

  /** sendVoice posts the reply as a voice note, captioned with the core's
   *  label ("AI voice"), replying to the bridge's text message when it is
   *  known. A person who forbids voice notes from others makes Telegram
   *  refuse it; that is a VoiceRefusedError, which stops spoken replies in
   *  the conversation. */
  async sendVoice(ref: TelegramRef, audio: VoiceOut): Promise<void> {
    const form = new FormData();
    for (const [k, v] of Object.entries(threadParams(ref))) form.set(k, v);
    form.set("voice", new Blob([audio.bytes as Uint8Array<ArrayBuffer>], { type: audio.mime }), fileName("voice", audio.mime));
    form.set("caption", audio.label.slice(0, MAX_CAPTION));
    if (typeof audio.seconds === "number" && audio.seconds > 0) form.set("duration", String(Math.round(audio.seconds)));
    const target = this.opts.replyTarget?.(ref);
    if (target !== undefined) form.set("reply_parameters", JSON.stringify({ message_id: target, allow_sending_without_reply: true }));
    try {
      await this.opts.api.callForm("sendVoice", form);
    } catch (err) {
      if (err instanceof TelegramError && /VOICE_MESSAGES_FORBIDDEN/.test(err.description)) {
        throw new VoiceRefusedError("telegram refused the voice message (the person forbids voice messages)");
      }
      throw err;
    }
  }

  /** indicate shows "typing…" while a voice note is transcribed: Telegram has
   *  no "listening" action, and the agent's answer that follows is typed.
   *  While a reply is voiced it shows "recording a voice message…", which is
   *  what the person is about to get; upload_voice would only cover the last
   *  second of upload. Cosmetic, so never retried. */
  async indicate(ref: TelegramRef, what: "listening" | "speaking"): Promise<void> {
    const action = what === "speaking" ? "record_voice" : "typing";
    const params: Record<string, unknown> = ref.threadId === undefined ? { chat_id: ref.chatId } : { chat_id: ref.chatId, message_thread_id: ref.threadId };
    await this.opts.api.call("sendChatAction", { ...params, action }, { retry: false });
  }
}
