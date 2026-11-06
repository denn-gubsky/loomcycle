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
    return this.withRetry<T>(method, { headers: { "content-type": "application/json" }, body: JSON.stringify(params) }, o);
  }

  /** callForm is call with a multipart body, for a method that uploads a
   *  file (sendVoice). Nested values must already be JSON strings, as the
   *  Bot API reads them from a form. */
  async callForm<T = unknown>(method: string, form: FormData, o: CallOptions = {}): Promise<T> {
    // fetch sets the multipart content type, boundary included; a FormData
    // body is serialised afresh on each attempt, so a retry resends it whole.
    return this.withRetry<T>(method, { body: form }, o);
  }

  private async withRetry<T>(method: string, init: RequestBody, o: CallOptions): Promise<T> {
    const retry = o.retry ?? true;
    for (let attempt = 1; ; attempt++) {
      try {
        return (await this.once(method, init)) as T;
      } catch (err) {
        const wait = err instanceof TelegramError && err.code === 429 ? err.retryAfter : undefined;
        if (!retry || wait === undefined || wait > MAX_RETRY_AFTER_SECONDS || attempt >= MAX_ATTEMPTS) throw err;
        await this.sleep(wait * 1000);
      }
    }
  }

  private async once(method: string, init: RequestBody): Promise<unknown> {
    let res: Response;
    try {
      res = await this.fetch(`${this.opts.baseUrl}/bot${this.opts.token}/${method}`, {
        method: "POST",
        ...(init.headers ? { headers: init.headers } : {}),
        body: init.body,
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

  /** downloadFile fetches a file getFile named, from the Bot API's own file
   *  host (<baseUrl>/file/bot<token>/<file_path>), reading at most maxBytes.
   *  The bot token is in that URL, so: the path is checked to be a plain
   *  relative one, a redirect is refused rather than followed (it would
   *  carry the request elsewhere), and an oversized body is cut off while
   *  it streams in, not after. A failure is a TelegramError for the method
   *  "file", which names no URL. */
  async downloadFile(filePath: string, maxBytes: number, signal?: AbortSignal): Promise<Uint8Array> {
    if (!isPlainFilePath(filePath)) throw new TelegramError(FILE, 0, "bad file path");
    // One signal for the caller's deadline and the byte cap. Linked by hand:
    // AbortSignal.any needs Node 20.3, and the package supports 20.
    const cap = new AbortController();
    const outer = signal ?? AbortSignal.timeout(this.timeoutMs);
    if (outer.aborted) cap.abort();
    else outer.addEventListener("abort", () => cap.abort(), { once: true });
    const expected = new URL(this.opts.baseUrl).origin;
    let res: Response;
    try {
      res = await this.fetch(`${this.opts.baseUrl}/file/bot${this.opts.token}/${filePath}`, {
        method: "GET",
        redirect: "manual",
        signal: cap.signal,
      });
    } catch {
      // fetch's own error can name the address it called; drop it.
      throw new TelegramError(FILE, 0, "no response");
    }
    const drop = () => void res.body?.cancel().catch(() => undefined);
    // "manual" hands a redirect back unfollowed: a 3xx, or an opaque
    // response in a browser-like fetch.
    if (res.type === "opaqueredirect" || (res.status >= 300 && res.status < 400)) {
      drop();
      throw new TelegramError(FILE, res.status, "redirect refused");
    }
    if (res.url && new URL(res.url).origin !== expected) {
      drop();
      throw new TelegramError(FILE, res.status, "answered from another host");
    }
    if (!res.ok) {
      drop();
      throw new TelegramError(FILE, res.status, "");
    }
    const declared = Number(res.headers.get("content-length") ?? NaN);
    if (Number.isFinite(declared) && declared > maxBytes) {
      drop();
      throw new TelegramError(FILE, 413, "file too large");
    }
    if (!res.body) return new Uint8Array();
    const chunks: Uint8Array[] = [];
    let total = 0;
    const reader = res.body.getReader();
    try {
      for (;;) {
        const { done, value } = await reader.read();
        if (done) break;
        total += value.byteLength;
        if (total > maxBytes) {
          cap.abort();
          await reader.cancel().catch(() => undefined);
          throw new TelegramError(FILE, 413, "file too large");
        }
        chunks.push(value);
      }
    } catch (err) {
      if (err instanceof TelegramError) throw err;
      throw new TelegramError(FILE, 0, "download interrupted");
    }
    const out = new Uint8Array(total);
    let at = 0;
    for (const c of chunks) {
      out.set(c, at);
      at += c.byteLength;
    }
    return out;
  }
}

interface RequestBody {
  headers?: Record<string, string>;
  body: string | FormData;
}

/** The method name a download failure is reported under. */
const FILE = "file";

/** isPlainFilePath accepts the relative paths getFile gives ("voice/file_3.oga"):
 *  segments of letters, digits, "_", "-" and ".", none starting with a dot,
 *  so neither ".." nor a scheme, query or absolute path can steer the URL. */
function isPlainFilePath(p: string): boolean {
  return p.length > 0 && p.length <= 512 && p.split("/").every((seg) => /^[A-Za-z0-9_-][A-Za-z0-9_.-]*$/.test(seg));
}

/** Telegram refused the HTML: an unbalanced tag, an unknown entity. */
export function isParseError(err: unknown): boolean {
  return err instanceof TelegramError && err.code === 400 && /can't parse entities/i.test(err.description);
}

/** An edit that would leave the message as it was. */
export function isNotModified(err: unknown): boolean {
  return err instanceof TelegramError && err.code === 400 && /message is not modified/i.test(err.description);
}
