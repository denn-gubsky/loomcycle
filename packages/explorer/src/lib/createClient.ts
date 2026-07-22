import { LoomcycleClient } from "@loomcycle/client";

// Connection the host hands to <PathExplorer> / <DocumentViewer>. `fetch` is an
// optional override so an embedding app can route requests however it needs
// (proxying, header injection, instrumentation). External consumers just pass
// baseUrl + token and hit the runtime directly.
export interface Connection {
  /** loomcycle base URL. "" means same-origin. */
  baseUrl: string;
  /** Bearer token. Omit when the runtime runs in open mode (or when the host
   *  authenticates via a same-origin cookie through a custom `fetch`). */
  token?: string;
  /** Optional fetch override (proxying, header injection, instrumentation). */
  fetch?: typeof fetch;
}

/** Build a loomcycle client from a Connection. The fetch is always wrapped: the
 *  SDK calls its stored fetch as a method (`ctx.fetchImpl(url)`), and the
 *  browser's native fetch rejects a non-global receiver with "Illegal
 *  invocation" — so we hand it a plain function that calls global fetch. */
export function createLoomcycleClient(c: Connection): LoomcycleClient {
  const impl = c.fetch;
  return new LoomcycleClient({
    baseUrl: c.baseUrl,
    authToken: c.token || undefined,
    fetch: (input, init) => (impl ? impl(input, init) : fetch(input, init)),
  });
}

/** AssetFetch does a raw authenticated GET against a relative loomcycle path and
 *  returns the Response — for BINARY endpoints (RFC BO image assets) the JSON
 *  client can't model. It mirrors the Connection's transport: the custom `fetch`
 *  (e.g. the Web UI's same-origin cookie fetch) carries cookie auth, and a bearer
 *  token is added as an Authorization header when set. */
export type AssetFetch = (path: string) => Promise<Response>;

/** Build the AssetFetch from a Connection — the SAME auth transport the JSON
 *  client uses, so an image asset loads under both cookie (web) and bearer
 *  (standalone package) auth. */
export function assetFetchFromConnection(c: Connection): AssetFetch {
  const impl = c.fetch;
  const doFetch: typeof fetch = (input, init) => (impl ? impl(input, init) : fetch(input, init));
  return (path) => {
    const headers: Record<string, string> = {};
    if (c.token) headers.Authorization = `Bearer ${c.token}`;
    return doFetch(c.baseUrl + path, { headers });
  };
}
