// The two edges of a bridge, as narrow interfaces.
//
// Everything between them (tokens, conversations, rendering) is written
// against these, so it is testable without a chat platform or a loomcycle
// server. `LoomcycleClient` satisfies the loomcycle side structurally; each
// bridge package supplies the platform side as a `BridgePort`.

import type {
  AgentEvent,
  ContinueOptions,
  MintedUserToken,
  RunOptions,
  TokenSurface as ClientTokenSurface,
} from "@loomcycle/client";

import type { CanonicalCard } from "./cards.js";
import type { FileClient } from "./files.js";
import type { VoiceClient } from "./voice.js";

/** The run-plane calls a bridge makes with a user's own token. */
export interface RunClient {
  runStreaming(opts: RunOptions): AsyncIterable<AgentEvent>;
  continueSession(opts: ContinueOptions): AsyncIterable<AgentEvent>;
  sendRunInput(runId: string, text: string): Promise<{ delivered: boolean }>;
  cancelTurn(runId: string, opts?: { reason?: string }): Promise<unknown>;
  cancelAgent(agentId: string, opts?: { reason?: string }): Promise<unknown>;
  resolveInterrupt(
    runId: string,
    interruptId: string,
    opts: { answer?: string; resolvedBy?: string },
  ): Promise<unknown>;
  cancelInterrupt(runId: string, interruptId: string, opts?: { resolvedBy?: string }): Promise<unknown>;
  reviewRun(runId: string, decision: "approve" | "reject", opts?: { feedback?: string }): Promise<unknown>;
  /** The run's questions with the given status, to check a question saved
   *  before a restart (RFC HG §4.3). Optional, so a client double written
   *  before it still fits; without it a saved question is not restored. */
  listRunInterrupts?(runId: string, opts?: { status?: "pending" }): Promise<{ interrupts?: Array<{ interrupt_id: string; status?: string }> }>;
  /** Re-attaches to a run's stream, replaying its events, then tailing. Used
   *  only after a restart, to show what the run says once its saved question
   *  is answered. Optional, as listRunInterrupts. */
  streamRunByID?(runId: string, opts?: { signal?: AbortSignal }): AsyncIterable<AgentEvent>;
}

/** The two calls a bridge makes with its tenant-operator token. */
export interface AdminClient {
  createUser(body: { subject: string; display_name?: string; access_mode?: string }): Promise<unknown>;
  mintUserToken(
    subject: string,
    opts: {
      ttlSeconds?: number;
      name?: string;
      maxActive?: number;
      agents?: string[];
      /** Off-run surfaces beside agents: "speech", "transcription" (RFC GA,
       *  loomcycle 1.141 and later), "inbox_put" (RFC HE, 1.168 and later).
       *  An older server refuses a surface it does not know. Typed as the
       *  installed client types it, so any LoomcycleClient still fits. */
      surfaces?: ClientTokenSurface[];
      overrides?: "none" | string[];
      rate?: { perMinute: number; burst?: number };
    },
  ): Promise<MintedUserToken>;
}

/** An off-run surface a user token may be granted beside its agent. */
export type TokenSurface = "speech" | "transcription" | "inbox_put";

/** Builds a loomcycle client authenticated with the given bearer. The speech
 *  calls and the inbox put are optional so a client double written before
 *  them still fits, and so does a published client older than the inbox put
 *  (1.168.0): a bridge with voice or files on needs them, and says so. */
export type ClientFactory = (token: string) => RunClient & AdminClient & Partial<VoiceClient> & Partial<FileClient>;

/** What a bridge sends to the platform: plain text, a typing indicator, or a
 *  card. The platform's port renders the card in its own format. */
export type OutboundActivity =
  | { type: "message"; text: string }
  | { type: "typing" }
  | { type: "message"; card: CanonicalCard };

/** One streamed message: it grows as the answer is written. */
export interface StreamLike {
  /** True once the user pressed the platform's Stop, where it has one. */
  readonly canceled: boolean;
  /** An informative update ("Using Read…"), shown until text starts. */
  update(text: string): void;
  /** Append text to the message. */
  emit(text: string): void;
  /** Finish the message. A platform may return a promise or a value; callers await it. */
  close(): unknown;
}

/** The platform side. `TRef` is whatever a reply needs to find its way back:
 *  a stored conversation reference, a channel and thread, a chat id. It must
 *  be plain JSON, since it is kept in the conversation store. */
export interface BridgePort<TRef> {
  send(ref: TRef, activity: OutboundActivity): Promise<void>;
  /** Present only on a platform that can stream a message as it is written. */
  createStream?(ref: TRef): StreamLike;
}

export interface Logger {
  info(msg: string, fields?: Record<string, unknown>): void;
  warn(msg: string, fields?: Record<string, unknown>): void;
  error(msg: string, fields?: Record<string, unknown>): void;
}

/** Logs one JSON line per call. Callers pass ids, never tokens or message text. */
export const consoleLogger: Logger = {
  info: (msg, fields) => console.log(JSON.stringify({ level: "info", msg, ...fields })),
  warn: (msg, fields) => console.warn(JSON.stringify({ level: "warn", msg, ...fields })),
  error: (msg, fields) => console.error(JSON.stringify({ level: "error", msg, ...fields })),
};
