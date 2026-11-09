import { describe, expect, it } from "vitest";

import { downloadPinned, PinnedDownloadError, type PinnedDownloadOptions } from "../src/download.js";

const FILES = "https://files.example.com";
const LINK = `${FILES}/f/1?sig=secret-signature`;

/** A scripted host: each URL answers once with what route gives; every
 *  request is kept with its options. */
function host(routes: Record<string, () => Response>) {
  const requests: Array<{ url: string; init: RequestInit | undefined }> = [];
  const fetchFn = (async (input: string | URL | Request, init?: RequestInit) => {
    const url = typeof input === "string" ? input : input instanceof URL ? input.href : input.url;
    requests.push({ url, init });
    const r = routes[url];
    return r ? r() : new Response("not found", { status: 404 });
  }) as typeof fetch;
  return { requests, fetchFn };
}

function opts(fetchFn: typeof fetch, over: Partial<PinnedDownloadOptions> = {}): PinnedDownloadOptions {
  return {
    allowed: (u) => u.protocol === "https:" && u.hostname === "files.example.com",
    headers: { authorization: "Bearer tok" },
    maxBytes: 100,
    signal: AbortSignal.timeout(5_000),
    fetch: fetchFn,
    ...over,
  };
}

const redirect = (to: string) => () => new Response(null, { status: 302, headers: { location: to } });

async function refusal(p: Promise<unknown>): Promise<PinnedDownloadError> {
  try {
    await p;
  } catch (err) {
    expect(err).toBeInstanceOf(PinnedDownloadError);
    // A link's query is a credential: never in the message.
    expect((err as Error).message).not.toContain("secret");
    return err as PinnedDownloadError;
  }
  throw new Error("expected a refusal");
}

describe("downloadPinned", () => {
  it("fetches an allowed URL with the headers, without following redirects itself, and returns its type", async () => {
    const h = host({ [LINK]: () => new Response(new Uint8Array([1, 2, 3]), { headers: { "content-type": "audio/ogg" } }) });
    expect(await downloadPinned(LINK, opts(h.fetchFn))).toEqual({ bytes: new Uint8Array([1, 2, 3]), contentType: "audio/ogg" });
    expect(h.requests[0]!.init).toMatchObject({ method: "GET", headers: { authorization: "Bearer tok" }, redirect: "manual" });
  });

  it("follows a redirect within the allowed hosts, re-checking each hop", async () => {
    const next = `${FILES}/f/2`;
    const h = host({ [LINK]: redirect("/f/2"), [next]: () => new Response(new Uint8Array([9])) });
    expect((await downloadPinned(LINK, opts(h.fetchFn))).bytes).toEqual(new Uint8Array([9]));
    expect(h.requests.map((r) => r.url)).toEqual([LINK, next]);
  });

  it("never sends a request, headers and all, to a host it was redirected to that is not allowed", async () => {
    const h = host({ [LINK]: redirect("https://evil.example.net/steal") });
    const err = await refusal(downloadPinned(LINK, opts(h.fetchFn)));
    expect(err.reason).toBe("host");
    expect(err.detail).toEqual({ host: "evil.example.net", redirected: true });
    expect(h.requests).toHaveLength(1);
  });

  it("refuses a first URL that is not allowed, or not a URL, without fetching", async () => {
    const h = host({});
    expect((await refusal(downloadPinned("http://files.example.com/f", opts(h.fetchFn)))).detail).toEqual({ host: "files.example.com", redirected: false });
    expect((await refusal(downloadPinned("not a url", opts(h.fetchFn)))).reason).toBe("url");
    expect(h.requests).toHaveLength(0);
  });

  it("refuses a redirect without a Location, and one past maxRedirects", async () => {
    const bare = host({ [LINK]: () => new Response(null, { status: 302 }) });
    expect((await refusal(downloadPinned(LINK, opts(bare.fetchFn)))).reason).toBe("redirect");
    const loop = host({ [LINK]: redirect(LINK) });
    expect((await refusal(downloadPinned(LINK, opts(loop.fetchFn, { maxRedirects: 2 })))).reason).toBe("redirect");
    expect(loop.requests).toHaveLength(3);
  });

  it("refuses an error status and a content type accept refuses", async () => {
    const h = host({ [LINK]: () => new Response("no", { status: 403 }) });
    expect((await refusal(downloadPinned(LINK, opts(h.fetchFn)))).detail).toEqual({ status: 403 });
    const page = host({ [LINK]: () => new Response("<html>", { headers: { "content-type": "text/html; charset=utf-8" } }) });
    const accept = (ct: string) => !ct.toLowerCase().startsWith("text/html");
    expect((await refusal(downloadPinned(LINK, opts(page.fetchFn, { accept })))).reason).toBe("type");
  });

  it("refuses a declared length over the cap before reading, and stops reading a body that grows past it", async () => {
    const declared = host({ [LINK]: () => new Response(new Uint8Array(10), { headers: { "content-length": "5000" } }) });
    expect((await refusal(downloadPinned(LINK, opts(declared.fetchFn)))).reason).toBe("size");
    let pulled = 0;
    const endless = host({
      [LINK]: () =>
        new Response(
          new ReadableStream<Uint8Array>({
            pull(c) {
              pulled++;
              if (pulled > 1000) c.close();
              else c.enqueue(new Uint8Array(10));
            },
          }),
        ),
    });
    expect((await refusal(downloadPinned(LINK, opts(endless.fetchFn)))).reason).toBe("size");
    expect(pulled).toBeLessThan(20);
  });
});
