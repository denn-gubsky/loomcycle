// The two edges of the bridge, as narrow interfaces.
//
// Everything between them (identity, tokens, conversations, rendering) is
// written against these, so it is testable without Teams or a loomcycle
// server. The real implementations are thin: `LoomcycleClient` satisfies the
// loomcycle side structurally, and `BridgeApp` (bridge.ts) the Teams side.

import type {
  AgentEvent,
  ContinueOptions,
  MintedUserToken,
  RunOptions,
} from "@loomcycle/client";

/** The run-plane calls the bridge makes with a user's own token. */
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
}

/** The two calls the bridge makes with its tenant-operator token. */
export interface AdminClient {
  createUser(body: { subject: string; display_name?: string; access_mode?: string }): Promise<unknown>;
  mintUserToken(
    subject: string,
    opts: {
      ttlSeconds?: number;
      name?: string;
      maxActive?: number;
      agents?: string[];
      overrides?: "none" | string[];
      rate?: { perMinute: number; burst?: number };
    },
  ): Promise<MintedUserToken>;
}

/** Builds a loomcycle client authenticated with the given bearer. */
export type ClientFactory = (token: string) => RunClient & AdminClient;

/** A Teams conversation reference, stored as plain JSON. */
export type ConversationRef = {
  conversation: { id: string; conversationType?: string; tenantId?: string };
  serviceUrl: string;
  [key: string]: unknown;
};

/** What the bridge sends to Teams: plain text, a typing indicator, or a card. */
export type OutboundActivity =
  | { type: "message"; text: string }
  | { type: "typing" }
  | { type: "message"; card: Record<string, unknown> };

/** One streamed message in a 1:1 chat (the Teams SDK's HttpStream). */
export interface StreamLike {
  /** True once the user pressed Stop. */
  readonly canceled: boolean;
  /** An informative update ("Using Read…"), shown until text starts. */
  update(text: string): void;
  /** Append text to the message. */
  emit(text: string): void;
  /** Finish the message. The SDK returns a promise or a value; callers await it. */
  close(): unknown;
}

export interface TeamsPort {
  send(ref: ConversationRef, activity: OutboundActivity): Promise<void>;
  createStream(ref: ConversationRef): StreamLike;
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
