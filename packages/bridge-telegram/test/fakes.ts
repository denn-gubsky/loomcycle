// Test doubles: a scripted loomcycle (copied from bridge-core's tests, with
// the client types derived from the core's ports so this package needs no
// @loomcycle/client of its own) and a fake Telegram Bot API.

import type { AdminClient, Logger, RunClient } from "@loomcycle/bridge-core";

type RunOptions = Parameters<RunClient["runStreaming"]>[0];
type ContinueOptions = Parameters<RunClient["continueSession"]>[0];
type AgentEvent = RunClient["runStreaming"] extends (o: never) => AsyncIterable<infer E> ? E : never;
type MintedUserToken = Awaited<ReturnType<AdminClient["mintUserToken"]>>;

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

export interface TgCall {
  method: string;
  params: Record<string, unknown>;
  /** The request path, to check the token is where Telegram wants it. */
  path: string;
}

/** A fake Bot API behind `fetch`: records each call, answers ok by default.
 *  `reply` overrides the answer for one method; returning an envelope with
 *  ok:false makes the call fail as Telegram would. */
export class FakeTelegram {
  readonly calls: TgCall[] = [];
  private nextMessageId = 100;
  reply: Record<string, (params: Record<string, unknown>, n: number) => object | undefined> = {};
  bot = { id: 999, is_bot: true, first_name: "Loom", username: "LoomBot" };

  fetch = (async (input: string | URL | Request, init?: RequestInit): Promise<Response> => {
    const url = new URL(typeof input === "string" ? input : input instanceof URL ? input.href : input.url);
    const method = url.pathname.split("/").pop() ?? "";
    const params = JSON.parse(String(init?.body ?? "{}")) as Record<string, unknown>;
    this.calls.push({ method, params, path: url.pathname });
    const n = this.calls.filter((c) => c.method === method).length;
    const custom = this.reply[method]?.(params, n);
    const env = custom ?? { ok: true, result: this.defaultResult(method) };
    return new Response(JSON.stringify(env), { status: (env as { ok?: boolean; error_code?: number }).ok === false ? ((env as { error_code?: number }).error_code ?? 400) : 200 });
  }) as typeof fetch;

  private defaultResult(method: string): unknown {
    if (method === "getMe") return this.bot;
    if (method === "sendMessage") return { message_id: this.nextMessageId++ };
    return true;
  }

  callsTo(method: string): TgCall[] {
    return this.calls.filter((c) => c.method === method);
  }

  /** texts are the texts of every sendMessage, in order. */
  texts(): string[] {
    return this.callsTo("sendMessage").map((c) => String(c.params.text));
  }
}

/** FakeGate is loomcycle's external-gate surface (/v1/_auth/assert, revoke):
 *  it reads each posted JWT's payload (the real server verifies the
 *  signature; these tests are about what the bridge asserts) and issues
 *  "tok-<gate>:<sub>-<n>" tokens. */
export class FakeGate {
  readonly asserts: Array<{ gate: string; payload: Record<string, unknown> }> = [];
  readonly revokes: Array<{ gate: string; payload: Record<string, unknown> }> = [];
  private seq = 0;

  fetch = (async (input: string | URL | Request, init?: RequestInit): Promise<Response> => {
    const url = new URL(typeof input === "string" ? input : input instanceof URL ? input.href : input.url);
    const body = JSON.parse(String(init?.body ?? "{}")) as { gate: string; assertion?: string; request?: string };
    const jwt = body.assertion ?? body.request ?? "";
    const payload = JSON.parse(Buffer.from(jwt.split(".")[1] ?? "", "base64url").toString() || "{}") as Record<string, unknown>;
    if (url.pathname === "/v1/_auth/revoke") {
      this.revokes.push({ gate: body.gate, payload });
      return new Response(null, { status: 204 });
    }
    this.asserts.push({ gate: body.gate, payload });
    const subject = `${body.gate}:${String(payload.sub)}`;
    return new Response(
      JSON.stringify({ token: `tok-${subject}-${++this.seq}`, expires_at: new Date(Date.now() + 12 * 3600_000).toISOString(), subject, tenant: "acme" }),
      { status: 200 },
    );
  }) as typeof fetch;
}

export function tgError(code: number, description: string, parameters?: object): object {
  return { ok: false, error_code: code, description, ...(parameters ? { parameters } : {}) };
}
