// Test doubles for the two edges: a scripted loomcycle and a recording Teams.

import type { AgentEvent, ContinueOptions, MintedUserToken, RunOptions } from "@loomcycle/client";

import type { AdminClient, ConversationRef, Logger, OutboundActivity, RunClient, StreamLike, TeamsPort } from "../src/ports.js";

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

  client = (token: string): RunClient & AdminClient => {
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
    return {
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

export class FakeTeams implements TeamsPort {
  readonly sent: Array<{ ref: ConversationRef; activity: OutboundActivity }> = [];
  readonly streams: FakeStream[] = [];
  async send(ref: ConversationRef, activity: OutboundActivity): Promise<void> {
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
  cards(): Array<Record<string, unknown>> {
    return this.sent.flatMap((s) => (s.activity.type === "message" && "card" in s.activity ? [s.activity.card] : []));
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
