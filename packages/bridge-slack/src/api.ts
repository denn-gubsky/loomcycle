// Slack's Web API over fetch: one POST per method, JSON in and out.
//
// Slack answers most failures with HTTP 200 and `{ok: false, error}`; a rate
// limit is HTTP 429 with Retry-After in seconds. Errors carry the method and
// Slack's error code only: never the token, a body, or a URL.

export class SlackApiError extends Error {
  constructor(
    readonly method: string,
    /** Slack's error code ("channel_not_found"), or "http_<status>". */
    readonly code: string,
    readonly status?: number,
  ) {
    super(`slack ${method}: ${code}`);
    this.name = "SlackApiError";
  }
}

export function slackErrorCode(err: unknown): string | undefined {
  return err instanceof SlackApiError ? err.code : undefined;
}

export interface SlackApiOptions {
  baseUrl: string;
  token: string;
  fetch?: typeof fetch;
  sleep?: (ms: number) => Promise<void>;
  /** Retries of a rate-limited call. */
  maxRetries?: number;
  /** A Retry-After longer than this is not waited out: the call fails. */
  maxRetryAfterMs?: number;
}

type Body = Record<string, unknown>;
type Encoded = { type: string; text: string };

const json = (body: Body): Encoded => ({ type: "application/json; charset=utf-8", text: JSON.stringify(body) });
export type SlackResponse = { ok: true } & Record<string, unknown>;

const defaultSleep = (ms: number) => new Promise<void>((r) => setTimeout(r, ms));

/** Slack's error codes are snake_case words; anything else is not logged as is. */
function safeCode(v: unknown): string {
  return typeof v === "string" && /^[a-z0-9_]{1,64}$/.test(v) ? v : "unknown_error";
}

export class SlackApi {
  private readonly fetch: typeof fetch;
  private readonly sleep: (ms: number) => Promise<void>;
  private readonly maxRetries: number;
  private readonly maxRetryAfterMs: number;

  constructor(private readonly opts: SlackApiOptions) {
    this.fetch = opts.fetch ?? globalThis.fetch;
    this.sleep = opts.sleep ?? defaultSleep;
    this.maxRetries = opts.maxRetries ?? 3;
    this.maxRetryAfterMs = opts.maxRetryAfterMs ?? 30_000;
  }

  /** call invokes a Web API method with the bot token. */
  async call(method: string, body: Body): Promise<SlackResponse> {
    return (await this.post(method, `${this.opts.baseUrl}/${method}`, json(body), true)).data;
  }

  /** callWithHeaders is call, also returning the response's headers
   *  (auth.test reports the token's scopes in x-oauth-scopes). */
  callWithHeaders(method: string, body: Body): Promise<{ data: SlackResponse; headers: Headers }> {
    return this.post(method, `${this.opts.baseUrl}/${method}`, json(body), true);
  }

  /** callForm invokes a method form-encoded: the file upload methods are not
   *  documented to take JSON, and every method takes a form. */
  async callForm(method: string, params: Record<string, string>): Promise<SlackResponse> {
    const body = { type: "application/x-www-form-urlencoded", text: new URLSearchParams(params).toString() };
    return (await this.post(method, `${this.opts.baseUrl}/${method}`, body, true)).data;
  }

  /** respond posts to an interaction's response_url, which carries its own
   *  authority: the bot token is never sent there. */
  async respond(url: string, body: Body): Promise<void> {
    await this.post("response_url", url, json(body), false);
  }

  private async post(method: string, url: string, body: Encoded, withToken: boolean): Promise<{ data: SlackResponse; headers: Headers }> {
    const headers: Record<string, string> = { "content-type": body.type };
    if (withToken) headers.authorization = `Bearer ${this.opts.token}`;
    for (let attempt = 0; ; attempt++) {
      const res = await this.fetch(url, { method: "POST", headers, body: body.text });
      if (res.status === 429) {
        await res.body?.cancel().catch(() => undefined);
        const waitMs = retryAfterMs(res.headers.get("retry-after"));
        if (attempt >= this.maxRetries || waitMs > this.maxRetryAfterMs) throw new SlackApiError(method, "ratelimited", 429);
        await this.sleep(waitMs);
        continue;
      }
      if (!res.ok) {
        await res.body?.cancel().catch(() => undefined);
        throw new SlackApiError(method, `http_${res.status}`, res.status);
      }
      // A response_url is not a Web API method and need not answer JSON: a
      // 2xx is success.
      if (!withToken) {
        await res.body?.cancel().catch(() => undefined);
        return { data: { ok: true }, headers: res.headers };
      }
      const data = (await res.json().catch(() => ({}))) as Record<string, unknown>;
      if (data.ok !== true) throw new SlackApiError(method, safeCode(data.error), res.status);
      return { data: data as SlackResponse, headers: res.headers };
    }
  }
}

/** retryAfterMs reads Retry-After as whole seconds; a missing or odd value waits one second. */
export function retryAfterMs(v: string | null): number {
  const n = v === null ? NaN : Number(v.trim());
  return Number.isFinite(n) && n >= 0 ? Math.ceil(n) * 1000 : 1000;
}
