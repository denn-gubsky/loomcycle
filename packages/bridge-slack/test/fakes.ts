// Test doubles: a scripted loomcycle (copied from bridge-core's tests) and a
// recording fake of Slack's Web API.

import type { AdminClient, Logger, RunClient } from "@loomcycle/bridge-core";

// The loomcycle client's types, read off the core's ports, so this package
// needs no dependency of its own on @loomcycle/client.
type RunOptions = Parameters<RunClient["runStreaming"]>[0];
type ContinueOptions = Parameters<RunClient["continueSession"]>[0];
type AgentEvent = ReturnType<RunClient["runStreaming"]> extends AsyncIterable<infer E> ? E : never;
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

export type { AgentEvent };

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

export interface SlackCall {
  url: string;
  method: string;
  auth?: string;
  body: Record<string, unknown>;
}

type Reply = { status?: number; headers?: Record<string, string>; json?: Record<string, unknown> };

/** A recording fake of Slack's Web API (and of response_urls), as `fetch`. */
export class FakeSlack {
  readonly calls: SlackCall[] = [];
  /** Queued replies by method, used before the defaults. */
  readonly script: Record<string, Reply[]> = {};
  private seq = 0;

  constructor(readonly base = "https://slack.test/api") {}

  reply(method: string, ...replies: Reply[]): void {
    (this.script[method] ??= []).push(...replies);
  }

  to(method: string): SlackCall[] {
    return this.calls.filter((c) => c.method === method);
  }

  fetch = (async (input: string | URL | Request, init?: RequestInit) => {
    const url = String(input);
    const method = url.startsWith(`${this.base}/`) ? url.slice(this.base.length + 1) : "response_url";
    const headers = (init?.headers ?? {}) as Record<string, string>;
    this.calls.push({ url, method, auth: headers.authorization, body: JSON.parse(String(init?.body ?? "{}")) });
    const r = this.script[method]?.shift() ?? { json: this.defaultReply(method) };
    return new Response(JSON.stringify(r.json ?? { ok: true }), {
      status: r.status ?? 200,
      headers: { "content-type": "application/json", ...(r.headers ?? {}) },
    });
  }) as typeof fetch;

  private defaultReply(method: string): Record<string, unknown> {
    switch (method) {
      case "auth.test":
        return { ok: true, user_id: "UBOT", team_id: "T1", bot_id: "BBOT" };
      case "users.info":
        return { ok: true, user: { name: "ada", profile: { display_name: "Ada", real_name: "Ada L" } } };
      case "chat.postMessage":
      case "chat.startStream":
        return { ok: true, channel: "C", ts: `1700000000.${String(++this.seq).padStart(6, "0")}` };
      default:
        return { ok: true };
    }
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

/** Every form of Slack mention that pings people, as an agent (or a prompt
 *  injection through it) might write them. */
export const MENTION_FORMS = ["<!channel>", "<!here>", "<!everyone>", "<!subteam^S0123>", "<@U0123ABC>", "@channel", "@here", "@everyone", "@Here"];

/** A mention Slack would turn into a notification. */
export const LIVE_MENTION = /<[!@]|@(here|channel|everyone)\b/i;
