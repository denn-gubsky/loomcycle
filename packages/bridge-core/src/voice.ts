// Voice messages (RFC GA): a person sends a voice note instead of typing, and
// can hear the agent's reply as one.
//
// The bridge does the work over loomcycle's speech routes, with the
// conversation's own token, so the person's budget, access rules and usage
// rows apply: download the recording, transcribe it, hand the text to the
// turn the way typed text goes, and, when the reply should be spoken, voice
// it and post the audio in the platform's own voice-message form. Nothing is
// kept: the audio lives in memory for one request.
//
// A platform opts in by giving its adapter a VoicePort. One without it
// behaves exactly as before.

import type { SpeechInfo, SpeechResult, TranscribeOptions, TranscribeResult, WhoamiResponse } from "@loomcycle/client";

/** A recording an inbound activity carries, before anything is downloaded. */
export interface VoiceIn {
  /** A voice note, a round video note, a platform's own clip, or an audio
   *  file someone attached. */
  kind: "voice" | "video_note" | "clip" | "audio_file";
  /** What the adapter downloads from: a file id, a URL. Opaque to the core. */
  ref: unknown;
  mime?: string;
  /** From the platform's metadata, when it gives one, before any download. */
  seconds?: number;
  bytes?: number;
}

/** The audio to post as a voice reply. */
export interface VoiceOut {
  bytes: Uint8Array;
  /** `audio/ogg`, `audio/mpeg` or `audio/wav`. */
  mime: string;
  seconds?: number;
  /** Says the voice is generated ("AI voice"), and whether it is the reply's
   *  first part only. A platform shows it as a caption, a title or a line of
   *  the text it replies to. */
  label: string;
}

/** Thrown by sendVoice when the platform refused the voice message for this
 *  conversation (a person's privacy setting, a missing permission): spoken
 *  replies stop there until someone turns them on again. */
export class VoiceRefusedError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "VoiceRefusedError";
  }
}

export interface VoiceCapabilities {
  /** The formats this platform shows as a voice message, best first. Empty:
   *  it cannot post one, and replies stay text. */
  voiceFormats: Array<"ogg" | "mp3" | "wav">;
  /** "native": a real voice message; "file": an audio attachment; "none": the
   *  platform takes voice in only. */
  out: "native" | "file" | "none";
  /** The largest audio the platform accepts from the bot. */
  maxUploadBytes: number;
}

/** The platform side of voice. `TRef` is the conversation's reply reference. */
export interface VoicePort<TRef> {
  /** Download a recording with the platform's credential, sent only to its
   *  own file host, reading at most maxBytes (more is an error). */
  download(v: VoiceIn, maxBytes: number, signal: AbortSignal): Promise<{ bytes: Uint8Array; mime: string; name?: string }>;
  /** Post a voice reply. Absent where the platform takes voice in only. */
  sendVoice?(ref: TRef, audio: VoiceOut): Promise<void>;
  /** A "listening…" or "recording…" indicator, where the platform has one. */
  indicate?(ref: TRef, what: "listening" | "speaking"): Promise<void>;
  readonly capabilities: VoiceCapabilities;
}

/** The speech calls a bridge makes with a conversation's token.
 *  `LoomcycleClient` satisfies it structurally. */
export interface VoiceClient {
  transcribe(file: Blob | Uint8Array, opts?: TranscribeOptions): Promise<TranscribeResult>;
  speech(input: string, opts?: { language?: string; format?: "wav" | "mp3" | "ogg"; signal?: AbortSignal }): Promise<SpeechResult>;
  speechInfo(opts?: { signal?: AbortSignal }): Promise<SpeechInfo>;
  whoami(opts?: { signal?: AbortSignal }): Promise<WhoamiResponse>;
}

/** When a reply is spoken. */
export type VoiceReplyMode = "mirror" | "always" | "never";

/** The bridge's voice settings (BRIDGE_VOICE_*). */
export interface VoiceConfig {
  enabled: boolean;
  /** Take voice from an untrusted speaker, like their text (decision D6). */
  untrusted: boolean;
  reply: VoiceReplyMode;
  maxSeconds: number;
  maxBytes: number;
  /** The longest stretch of a reply that is spoken; a longer reply is spoken
   *  up to here (decision D8). */
  replyMaxChars: number;
  /** Quote what was heard before the agent answers (decision D5). */
  echo: boolean;
  /** Says a voice reply is generated. Never empty. */
  label: string;
  /** A language hint when the platform gives none. */
  language?: string;
}

export const DEFAULT_VOICE_CONFIG: VoiceConfig = {
  enabled: false,
  untrusted: true,
  reply: "mirror",
  maxSeconds: 120,
  maxBytes: 8 * 1024 * 1024,
  replyMaxChars: 1500,
  echo: false,
  label: "AI voice",
};

/** The fixed line the agent sees before a transcript (decision D9), so it can
 *  tell the message was spoken and answer in a form that reads well aloud. It
 *  is added by the bridge after transcription and never taken from the
 *  audio; whether a message counts as voice comes from how it arrived. */
export const VOICE_PREFIX = "(voice message)";

/** What a person hears when a voice message cannot be taken. */
export const VOICE_TEXT = {
  tooLong: (maxSeconds: number) =>
    `That voice message is too long; please keep it under ${formatSeconds(maxSeconds)} or type it.`,
  unavailable: "I can only read text for now.",
  download: "I couldn't download that voice message. Please try again or type it.",
  empty: "I couldn't make out any words. Please try again or type it.",
  failed: "I can't listen to voice messages right now. Please type it.",
  partial: "first part; the rest is in the text",
} as const;

function formatSeconds(s: number): string {
  if (s % 60 === 0) return s === 60 ? "1 minute" : `${s / 60} minutes`;
  return `${s} seconds`;
}

/** The longest an echo quote gets. */
export const ECHO_MAX_CHARS = 300;

/** echoLine quotes what was heard, cut to ECHO_MAX_CHARS. */
export function echoLine(transcript: string): string {
  const t = transcript.replace(/\s+/g, " ").trim();
  const cut = t.length > ECHO_MAX_CHARS ? `${t.slice(0, ECHO_MAX_CHARS - 1).trimEnd()}…` : t;
  return `🎙 "${cut}"`;
}

/** languageHint turns a platform locale (`en`, `pt-BR`, `uk_UA`) into the
 *  ISO 639-1 code the transcription route takes, or undefined. */
export function languageHint(locale: string | undefined): string | undefined {
  const m = /^([a-z]{2})(?:[-_].*)?$/i.exec(locale?.trim() ?? "");
  return m ? m[1]!.toLowerCase() : undefined;
}

/** spokenPart is the stretch of a reply that is read aloud: all of it when it
 *  fits in max characters, else its first part, cut at the last paragraph
 *  end in the final fifth before max, else the last sentence end, else the
 *  last word, never inside a fenced code block (which is dropped whole). */
export function spokenPart(text: string, max: number): { text: string; partial: boolean } {
  const t = text.trim();
  if (t.length <= max) return { text: t, partial: false };
  let head = t.slice(0, max);
  // A code fence opened and not closed inside head would be read half: cut
  // before it.
  const fences = [...head.matchAll(/^```/gm)];
  if (fences.length % 2 === 1) head = head.slice(0, fences[fences.length - 1]!.index).trimEnd();
  const floor = Math.floor(head.length * 0.8);
  const para = head.lastIndexOf("\n\n");
  let cut = para >= floor ? para : -1;
  if (cut < 0) {
    const sentence = /[.!?…](?:["'”’)\]]*)(?=\s|$)/g;
    let last = -1;
    for (const m of head.matchAll(sentence)) last = m.index + m[0].length;
    cut = last > 0 ? last : head.lastIndexOf(" ");
  }
  if (cut <= 0) cut = head.length;
  return { text: head.slice(0, cut).trimEnd(), partial: true };
}

/** pickFormat is the first format the platform shows as voice that the
 *  deployment returns, or undefined when there is none. */
export function pickFormat(caps: VoiceCapabilities, info: SpeechInfo | undefined): "ogg" | "mp3" | "wav" | undefined {
  const offered = new Set(info?.speech?.formats ?? []);
  return caps.voiceFormats.find((f) => offered.has(f));
}

interface Cached<T> {
  value: T;
  at: number;
}

/** How long what loomcycle says about a caller (its surfaces, its speech
 *  settings) is reused before it is asked again. */
export const VOICE_INFO_TTL_MS = 10 * 60 * 1000;

/** VoiceInfoCache remembers, per run identity, whether it may transcribe and
 *  speak and what the deployment offers it, so a voice note does not cost two
 *  extra round trips. A refusal clears the entry. */
export class VoiceInfoCache {
  private readonly surfaces = new Map<string, Cached<{ speech: boolean; transcription: boolean }>>();
  private readonly info = new Map<string, Cached<SpeechInfo>>();

  constructor(private readonly now: () => number = Date.now) {}

  async surfacesOf(subject: string, client: VoiceClient): Promise<{ speech: boolean; transcription: boolean }> {
    const hit = this.fresh(this.surfaces, subject);
    if (hit) return hit;
    const me = await client.whoami();
    const s = me.effective?.surfaces ?? {};
    const value = { speech: s.speech === true, transcription: s.transcription === true };
    this.surfaces.set(subject, { value, at: this.now() });
    return value;
  }

  async infoOf(subject: string, client: VoiceClient): Promise<SpeechInfo> {
    const hit = this.fresh(this.info, subject);
    if (hit) return hit;
    const value = await client.speechInfo();
    this.info.set(subject, { value, at: this.now() });
    return value;
  }

  forget(subject: string): void {
    this.surfaces.delete(subject);
    this.info.delete(subject);
  }

  private fresh<T>(m: Map<string, Cached<T>>, key: string): T | undefined {
    const c = m.get(key);
    if (!c) return undefined;
    if (this.now() - c.at > VOICE_INFO_TTL_MS) {
      m.delete(key);
      return undefined;
    }
    return c.value;
  }
}
