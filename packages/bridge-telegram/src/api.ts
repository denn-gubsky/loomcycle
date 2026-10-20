// The Bot API calls the bridge makes, over fetch.
//
// The bot token is part of every request URL (/bot<token>/<method>), so no
// URL leaves this file: not in a log line, not in an error. A failed call is
// reported by method name and Telegram's error code only. Telegram's
// description is kept for the caller to match on ("can't parse entities"),
// never logged, since it can quote message text back.

/** What Telegram answers a failed call with. */
export class TelegramError extends Error {
  constructor(
    readonly method: string,
    /** Telegram's error_code, or the HTTP status; 0 when the call never got an answer. */
    readonly code: number,
    readonly description: string,
    /** Seconds to wait, on a 429. */
    readonly retryAfter?: number,
    /** The new id of a group that became a supergroup. */
    readonly migrateToChatId?: number,
  ) {
    super(`telegram ${method} failed (${code})`);
    this.name = "TelegramError";
  }
}

export interface TelegramApiOptions {
  token: string;
  baseUrl: string;
  fetch?: typeof fetch;
  sleep?: (ms: number) => Promise<void>;
  /** Abort a call that takes longer than this. */
  timeoutMs?: number;
}

export interface CallOptions {
  /** Wait out a 429 and retry. Off for calls that are cheap to drop (typing, a
   *  streamed draft): their caller backs off instead. */
  retry?: boolean;
}

/** A 429 asking for longer than this is passed to the caller, not waited out:
 *  a chat turn should not hang for minutes behind a flood limit. */
export const MAX_RETRY_AFTER_SECONDS = 30;
const MAX_ATTEMPTS = 3;

interface Envelope {
  ok?: boolean;
  result?: unknown;
  description?: string;
  error_code?: number;
  parameters?: { retry_after?: number; migrate_to_chat_id?: number };
}

export class TelegramApi {
  private readonly fetch: typeof fetch;
  private readonly sleep: (ms: number) => Promise<void>;
  private readonly timeoutMs: number;

  constructor(private readonly opts: TelegramApiOptions) {
    this.fetch = opts.fetch ?? globalThis.fetch;
    this.sleep = opts.sleep ?? ((ms) => new Promise((r) => setTimeout(r, ms)));
    this.timeoutMs = opts.timeoutMs ?? 30_000;
  }

  async call<T = unknown>(method: string, params: Record<string, unknown> = {}, o: CallOptions = {}): Promise<T> {
    const retry = o.retry ?? true;
    for (let attempt = 1; ; attempt++) {
      try {
        return (await this.once(method, params)) as T;
      } catch (err) {
        const wait = err instanceof TelegramError && err.code === 429 ? err.retryAfter : undefined;
        if (!retry || wait === undefined || wait > MAX_RETRY_AFTER_SECONDS || attempt >= MAX_ATTEMPTS) throw err;
        await this.sleep(wait * 1000);
      }
    }
  }

  private async once(method: string, params: Record<string, unknown>): Promise<unknown> {
    let res: Response;
    try {
      res = await this.fetch(`${this.opts.baseUrl}/bot${this.opts.token}/${method}`, {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify(params),
        signal: AbortSignal.timeout(this.timeoutMs),
      });
    } catch {
      // fetch's own error can name the address it called; drop it.
      throw new TelegramError(method, 0, "no response");
    }
    let env: Envelope;
    try {
      env = (await res.json()) as Envelope;
    } catch {
      throw new TelegramError(method, res.status, "unreadable response");
    }
    if (env.ok === true) return env.result;
    const retryAfter = env.parameters?.retry_after;
    throw new TelegramError(
      method,
      env.error_code ?? res.status,
      env.description ?? "",
      typeof retryAfter === "number" ? retryAfter : undefined,
      env.parameters?.migrate_to_chat_id,
    );
  }
}

/** Telegram refused the HTML: an unbalanced tag, an unknown entity. */
export function isParseError(err: unknown): boolean {
  return err instanceof TelegramError && err.code === 400 && /can't parse entities/i.test(err.description);
}

/** An edit that would leave the message as it was. */
export function isNotModified(err: unknown): boolean {
  return err instanceof TelegramError && err.code === 400 && /message is not modified/i.test(err.description);
}
