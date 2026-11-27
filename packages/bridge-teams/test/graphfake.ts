// A fake Microsoft Graph and SharePoint for the Graph fallback's tests (RFC
// HF): what a lookup reads and a download fetches, with every request
// recorded with the Authorization it carried.

export const TENANT = "11111111-1111-1111-1111-111111111111";
export const ALICE = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa";
export const MALLORY = "cccccccc-cccc-cccc-cccc-cccccccccccc";
export const APP_ID = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb";
export const TOKEN = "graph-token-secret";
export const GRAPH = "https://graph.microsoft.com";
export const CHAT = "19:alice_bot@unq.gbl.spaces";
export const CONTENT_URL = "https://contoso-my.sharepoint.com/personal/alice/Documents/Microsoft Teams Chat Files/a.pdf";
export const SP_LINK = "https://contoso-my.sharepoint.com/personal/alice/_layouts/15/download.aspx?UniqueId=1&tempauth=secret-tempauth";
export const HOSTS = [".sharepoint.com"];
export const PDF = new TextEncoder().encode("%PDF-1.7\n1 0 obj\n");

export const reference = (name = "a.pdf", contentUrl = CONTENT_URL) => ({ id: "att-1", contentType: "reference", contentUrl, name });

/** What the fake Graph and SharePoint hold. */
export interface World {
  installs: Array<{ id: string }>;
  chat: { id: string; chatType: string };
  message: { from: { user: { id: string } | null } | null; attachments: unknown[] };
  /** How many reads of the message answer 404 first. */
  misses: number;
  /** The drive the shared file is in; each user's own is drive-<object id>. */
  itemDrive: string;
  /** Where the item's content redirects. */
  redirect?: string;
  /** With no redirect: the bytes Graph serves itself. */
  direct?: Uint8Array;
  /** What SharePoint serves, by URL. */
  files: Map<string, { body: Uint8Array; headers?: Record<string, string> }>;
  /** The message read never answers (until aborted). */
  hang?: boolean;
}

export function world(over: Partial<World> = {}): World {
  return {
    installs: [{ id: "inst-1" }],
    chat: { id: CHAT, chatType: "oneOnOne" },
    message: { from: { user: { id: ALICE } }, attachments: [reference()] },
    misses: 0,
    itemDrive: `drive-${ALICE}`,
    redirect: SP_LINK,
    files: new Map([[SP_LINK, { body: PDF }]]),
    ...over,
  };
}

export type Req = { url: string; auth: string | null; redirect?: RequestInit["redirect"] };

/** onGraph says a request went to the Graph host, by its parsed origin. */
export const onGraph = (r: Req) => new URL(r.url).origin === GRAPH;

/** fakeGraph answers Graph's calls and SharePoint's downloads from w, and
 *  records every request with the Authorization it carried. */
export function fakeGraph(w: World) {
  const requests: Req[] = [];
  const json = (v: unknown) => new Response(JSON.stringify(v), { status: 200, headers: { "content-type": "application/json" } });
  const fetchFn = (async (input: string | URL | Request, init?: RequestInit) => {
    const href = typeof input === "string" ? input : input instanceof URL ? input.href : input.url;
    requests.push({ url: href, auth: new Headers(init?.headers).get("authorization"), redirect: init?.redirect });
    const url = new URL(href);
    if (url.origin === GRAPH) {
      const path = decodeURIComponent(url.pathname);
      if (path.endsWith("/teamwork/installedApps")) return json({ value: w.installs });
      if (path.endsWith("/chat")) return json(w.chat);
      if (path.startsWith("/v1.0/chats/")) {
        if (w.hang) {
          return new Promise<Response>((_, reject) => init?.signal?.addEventListener("abort", () => reject(init.signal!.reason)));
        }
        if (w.misses > 0) {
          w.misses--;
          return new Response("{}", { status: 404 });
        }
        return json(w.message);
      }
      if (path.startsWith("/v1.0/shares/") && path.endsWith("/driveItem")) return json({ id: "item-1", name: "a.pdf", parentReference: { driveId: w.itemDrive } });
      const drive = /^\/v1\.0\/users\/([^/]+)\/drive$/.exec(path);
      if (drive) return json({ id: `drive-${drive[1]}` });
      if (path === `/v1.0/drives/${w.itemDrive}/items/item-1/content`) {
        if (w.redirect) return new Response(null, { status: 302, headers: { location: w.redirect } });
        if (w.direct) return new Response(w.direct, { status: 200, headers: { "content-type": "application/pdf" } });
      }
      return new Response("{}", { status: 404 });
    }
    const f = w.files.get(href);
    if (!f) return new Response("not found", { status: 404 });
    return new Response(f.body, { status: 200, headers: f.headers ?? { "content-type": "application/octet-stream" } });
  }) as typeof fetch;
  return { requests, fetchFn };
}
