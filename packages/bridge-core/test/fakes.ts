// Test doubles for the two edges: a scripted loomcycle and a recording platform.

import type { AgentEvent, ContinueOptions, MintedUserToken, RunOptions, SpeechInfo, WhoamiResponse } from "@loomcycle/client";

import type { CanonicalCard } from "../src/cards.js";
import type { AdminClient, BridgePort, Logger, OutboundActivity, RunClient, StreamLike } from "../src/ports.js";
import type { VoiceCapabilities, VoiceClient, VoiceIn, VoiceOut, VoicePort } from "../src/voice.js";

/** What the fake loomcycle's speech side answers, when a test turns it on. */
export interface FakeSpeech {
  surfaces: { speech: boolean; transcription: boolean };
  info: SpeechInfo;
  transcript: string;
  language?: string;
  /** Fail transcribe calls with this error, in order, then succeed. */
  transcribeErrors: unknown[];
}

export function fakeSpeech(over: Partial<FakeSpeech> = {}): FakeSpeech {
  return {
    surfaces: { speech: true, transcription: true },
    info: {
      speech: { enabled: true, default_language: "en", languages: { en: { voices: ["v1"], default_voice: "v1" } }, formats: ["wav", "ogg"] },
      transcription: { enabled: true },
    },
    transcript: "what is the weather",
    language: "en",
    transcribeErrors: [],
    ...over,
  };
}

/** A push-driven async iterable: the test feeds the events a run would stream. */
export class EventChannel<T> implements AsyncIterable<T> {
  private items: T[] = [];
  private waiters: Array<(r: IteratorResult<T>) => void> = [];
  private done = false;
  private error?: unknown;

  push(v: T): void {
    const w = this.waiters.shift();
    if (w) w({ value: v, done: false });
    else this.items.push(v);
  }

  end(): void {
    this.done = true;
    for (const w of this.waiters.splice(0)) w({ value: undefined as never, done: true });
  }

  fail(err: unknown): void {
    this.error = err;
    this.end();
  }

  [Symbol.asyncIterator](): AsyncIterator<T> {
    return {
      next: () => {
        if (this.items.length) return Promise.resolve({ value: this.items.shift() as T, done: false });
        if (this.error) return Promise.reject(this.error);
        if (this.done) return Promise.resolve({ value: undefined as never, done: true });
        return new Promise((resolve) => this.waiters.push(resolve));
      },
    };
  }
}

export function httpError(status: number): Error {
  return Object.assign(new Error(`http ${status}`), { status });
}

export interface StartedRun {
  token: string;
  kind: "start" | "continue";
  opts: RunOptions | ContinueOptions;
  events: EventChannel<AgentEvent>;
}

type Call = { method: string; token: string; args: unknown[] };

export class FakeLoomcycle {
  readonly runs: StartedRun[] = [];
  readonly calls: Call[] = [];
  readonly createdUsers: Array<{ subject: string; access_mode?: string }> = [];
  readonly mints: Array<{ subject: string; opts: Parameters<AdminClient["mintUserToken"]>[1] }> = [];
  /** Set to fail the next call to the named method with this error. */
  failNext: Record<string, unknown> = {};
  /** What runStreaming / continueSession throw when started (admission refusal). */
  startError?: unknown;
  existingUsers = new Set<string>();
  ttlMs = 12 * 3600 * 1000;
  now = () => Date.now();
  private tokenSeq = 0;

  callsTo(method: string): Call[] {
    return this.calls.filter((c) => c.method === method);
  }

  /** Set to give the client the speech calls (RFC GA). */
  speech?: FakeSpeech;

  client = (token: string): RunClient & AdminClient & Partial<VoiceClient> => {
    const rec = (method: string, args: unknown[]) => {
      this.calls.push({ method, token, args });
      const f = this.failNext[method];
      if (f) {
        delete this.failNext[method];
        throw f;
      }
    };
    const start = (kind: "start" | "continue", opts: RunOptions | ContinueOptions): AsyncIterable<AgentEvent> => {
      rec(kind === "start" ? "runStreaming" : "continueSession", [opts]);
      const events = new EventChannel<AgentEvent>();
      if (this.startError) {
        events.fail(this.startError);
        this.startError = undefined;
      }
      this.runs.push({ token, kind, opts, events });
      return events;
    };
    const sp = this.speech;
    const voice: Partial<VoiceClient> = sp
      ? {
          whoami: async () => {
            rec("whoami", []);
            return { tenant_id: "acme", subject: "x", scopes: [], is_admin: false, legacy: false, effective: { surfaces: { ...sp.surfaces } } } as unknown as WhoamiResponse;
          },
          speechInfo: async () => {
            rec("speechInfo", []);
            return sp.info;
          },
          transcribe: async (file, opts) => {
            rec("transcribe", [file, opts]);
            const e = sp.transcribeErrors.shift();
            if (e) throw e;
            return { text: sp.transcript, ...(sp.language ? { language: sp.language } : {}) };
          },
          speech: async (input, opts) => {
            rec("speech", [input, opts]);
            return { bytes: new Uint8Array([1, 2, 3]), contentType: opts?.format === "ogg" ? "audio/ogg" : "audio/wav", seconds: 2.5 };
          },
        }
      : {};
    return {
      ...voice,
      runStreaming: (opts) => start("start", opts),
      continueSession: (opts) => start("continue", opts),
      sendRunInput: async (runId, text) => {
        rec("sendRunInput", [runId, text]);
        return { delivered: true };
      },
      cancelTurn: async (runId, o) => rec("cancelTurn", [runId, o]),
      cancelAgent: async (agentId, o) => rec("cancelAgent", [agentId, o]),
      resolveInterrupt: async (runId, id, o) => rec("resolveInterrupt", [runId, id, o]),
      cancelInterrupt: async (runId, id, o) => rec("cancelInterrupt", [runId, id, o]),
      reviewRun: async (runId, d, o) => rec("reviewRun", [runId, d, o]),
      createUser: async (body) => {
        rec("createUser", [body]);
        if (this.existingUsers.has(body.subject)) throw httpError(409);
        this.existingUsers.add(body.subject);
        this.createdUsers.push(body);
      },
      mintUserToken: async (subject, opts): Promise<MintedUserToken> => {
        rec("mintUserToken", [subject, opts]);
        this.mints.push({ subject, opts });
        const token = `tok-${subject}-${++this.tokenSeq}`;
        return {
          def_id: `def-${this.tokenSeq}`,
          token,
          token_suffix: token.slice(-4),
          name: `u-${opts.name}`,
          scopes: ["runs:create"],
          created_at: new Date(this.now()).toISOString(),
          expires_at: new Date(this.now() + this.ttlMs).toISOString(),
          warning: "",
        };
      },
    };
  };
}

export class FakeStream implements StreamLike {
  canceled = false;
  updates: string[] = [];
  emits: string[] = [];
  closed = false;
  update(text: string): void {
    this.updates.push(text);
  }
  emit(text: string): void {
    this.emits.push(text);
  }
  async close(): Promise<void> {
    this.closed = true;
  }
  get text(): string {
    return this.emits.join("");
  }
}

/** The reply reference the fake platform uses: a conversation id. */
export type FakeRef = { conversation: string };

export class FakePlatform implements BridgePort<FakeRef> {
  readonly sent: Array<{ ref: FakeRef; activity: OutboundActivity }> = [];
  readonly streams: FakeStream[] = [];
  async send(ref: FakeRef, activity: OutboundActivity): Promise<void> {
    this.sent.push({ ref, activity });
  }
  createStream(): StreamLike {
    const s = new FakeStream();
    this.streams.push(s);
    return s;
  }
  messages(): string[] {
    return this.sent.flatMap((s) => (s.activity.type === "message" && "text" in s.activity ? [s.activity.text] : []));
  }
  cards(): CanonicalCard[] {
    return this.sent.flatMap((s) => (s.activity.type === "message" && "card" in s.activity ? [s.activity.card] : []));
  }
}

/** A platform that cannot stream: its port has no createStream. */
export class FakeBatchPlatform implements BridgePort<FakeRef> {
  readonly sent: Array<{ ref: FakeRef; activity: OutboundActivity }> = [];
  async send(ref: FakeRef, activity: OutboundActivity): Promise<void> {
    this.sent.push({ ref, activity });
  }
  messages(): string[] {
    return this.sent.flatMap((s) => (s.activity.type === "message" && "text" in s.activity ? [s.activity.text] : []));
  }
}

export const silentLogger: Logger = { info() {}, warn() {}, error() {} };

/** waitFor polls until cond holds, failing the test after a short while. */
export async function waitFor(cond: () => boolean, what = "condition"): Promise<void> {
  for (let i = 0; i < 200; i++) {
    if (cond()) return;
    await new Promise((r) => setTimeout(r, 5));
  }
  throw new Error(`timed out waiting for ${what}`);
}

/** settle lets queued promise callbacks run. */
export const settle = () => new Promise((r) => setTimeout(r, 10));

/** A platform's voice side that records what it was asked to do. */
export class FakeVoicePort implements VoicePort<FakeRef> {
  readonly downloads: VoiceIn[] = [];
  readonly voices: Array<{ ref: FakeRef; audio: VoiceOut }> = [];
  readonly indications: string[] = [];
  /** Set to make sendVoice throw this. */
  sendError?: unknown;
  sendVoice?: (ref: FakeRef, audio: VoiceOut) => Promise<void>;

  constructor(
    readonly capabilities: VoiceCapabilities = { voiceFormats: ["ogg", "mp3"], out: "native", maxUploadBytes: 1_000_000 },
    opts: { canSend?: boolean } = {},
  ) {
    if (opts.canSend !== false) {
      this.sendVoice = async (ref, audio) => {
        if (this.sendError) throw this.sendError;
        this.voices.push({ ref, audio });
      };
    }
  }

  async download(v: VoiceIn): Promise<{ bytes: Uint8Array; mime: string; name?: string }> {
    this.downloads.push(v);
    return { bytes: new Uint8Array([9, 9, 9]), mime: v.mime ?? "audio/ogg", name: "voice.ogg" };
  }

  async indicate(_ref: FakeRef, what: "listening" | "speaking"): Promise<void> {
    this.indications.push(what);
  }
}
