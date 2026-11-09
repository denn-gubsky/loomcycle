// downloadPinned: the one way a bridge fetches a file a platform names (a
// voice clip, an audio file). The URL comes from an inbound request, so it is
// only as trustworthy as that request, and a redirect may point anywhere:
// every hop is checked against the platform's hosts before it is fetched, a
// credential goes only with a request to an allowed host, and the body is
// read under a byte cap while it streams. Errors carry a reason, never the
// URL, whose query is often a credential while it lives.

export type PinnedRefusal =
  /** The link is not a URL. */
  | "url"
  /** A URL, first or redirected to, that `allowed` refused. */
  | "host"
  /** A redirect without a usable Location, or one too many. */
  | "redirect"
  /** An answer that is neither a redirect nor 2xx. */
  | "status"
  /** A content type `accept` refused (a sign-in page, say). */
  | "type"
  /** A body over maxBytes, declared or read. */
  | "size";

export class PinnedDownloadError extends Error {
  constructor(
    readonly reason: PinnedRefusal,
    /** host: the host refused; redirected: it was reached by a redirect;
     *  status: the HTTP status. Never the URL. */
    readonly detail: { host?: string; status?: number; redirected?: boolean } = {},
  ) {
    super(`download refused: ${reason}`);
    this.name = "PinnedDownloadError";
  }
}

export interface PinnedDownloadOptions {
  /** Whether a URL may be fetched. Asked before every request, the first and
   *  each redirect's, so `headers` never reach any other host. */
  allowed(url: URL): boolean;
  /** Sent with every request (e.g. a bearer token for the files host). */
  headers?: Record<string, string>;
  /** Whether to read a body served as this content type (the header as
   *  given, "" when absent). Default: any. */
  accept?(contentType: string): boolean;
  maxBytes: number;
  signal: AbortSignal;
  fetch?: typeof fetch;
  /** Redirects followed; default 3. */
  maxRedirects?: number;
}

const DEFAULT_MAX_REDIRECTS = 3;

/** downloadPinned fetches link under the rules above and returns the body
 *  with the content type it was served as. */
export async function downloadPinned(link: string, o: PinnedDownloadOptions): Promise<{ bytes: Uint8Array; contentType: string }> {
  const doFetch = o.fetch ?? globalThis.fetch;
  const maxRedirects = o.maxRedirects ?? DEFAULT_MAX_REDIRECTS;
  let url: URL;
  try {
    url = new URL(link);
  } catch {
    throw new PinnedDownloadError("url");
  }
  for (let hop = 0; ; hop++) {
    if (!o.allowed(url)) throw new PinnedDownloadError("host", { host: url.hostname, redirected: hop > 0 });
    const res = await doFetch(url.href, { method: "GET", ...(o.headers ? { headers: o.headers } : {}), redirect: "manual", signal: o.signal });
    if (res.status >= 300 && res.status < 400) {
      await res.body?.cancel().catch(() => undefined);
      const loc = res.headers.get("location");
      if (!loc || hop >= maxRedirects) throw new PinnedDownloadError("redirect");
      try {
        url = new URL(loc, url);
      } catch {
        throw new PinnedDownloadError("redirect");
      }
      continue;
    }
    if (!res.ok) {
      await res.body?.cancel().catch(() => undefined);
      throw new PinnedDownloadError("status", { status: res.status });
    }
    const contentType = res.headers.get("content-type") ?? "";
    if (o.accept && !o.accept(contentType)) {
      await res.body?.cancel().catch(() => undefined);
      throw new PinnedDownloadError("type");
    }
    return { bytes: await readCapped(res, o.maxBytes), contentType };
  }
}

/** readCapped reads a body, failing as soon as it passes maxBytes rather than
 *  after: a lying or absent Content-Length never lets more into memory. */
async function readCapped(res: Response, maxBytes: number): Promise<Uint8Array> {
  const declared = Number(res.headers.get("content-length") ?? NaN);
  if (Number.isFinite(declared) && declared > maxBytes) {
    await res.body?.cancel().catch(() => undefined);
    throw new PinnedDownloadError("size");
  }
  if (!res.body) return new Uint8Array();
  const reader = res.body.getReader();
  const parts: Uint8Array[] = [];
  let total = 0;
  for (;;) {
    const { done, value } = await reader.read();
    if (done) break;
    total += value.byteLength;
    if (total > maxBytes) {
      await reader.cancel().catch(() => undefined);
      throw new PinnedDownloadError("size");
    }
    parts.push(value);
  }
  const out = new Uint8Array(total);
  let at = 0;
  for (const p of parts) {
    out.set(p, at);
    at += p.byteLength;
  }
  return out;
}
