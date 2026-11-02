import { LoomcycleClient } from "@loomcycle/client";

// Connection is what a host hands the map: a base URL, an optional bearer, and
// an optional fetch override (the Web UI passes its same-origin cookie fetch).
// The same shape as @loomcycle/explorer's, owned here so the two packages never
// have to agree on one @loomcycle/client copy.
export interface Connection {
  /** loomcycle base URL. "" means same-origin. */
  baseUrl: string;
  /** Bearer token. Omit in open mode or when a custom `fetch` carries auth. */
  token?: string;
  fetch?: typeof fetch;
}

/** The fetch is always wrapped: the SDK calls its stored fetch as a method,
 *  and the browser's native fetch rejects a non-global receiver. */
export function createLoomcycleClient(c: Connection): LoomcycleClient {
  const impl = c.fetch;
  return new LoomcycleClient({
    baseUrl: c.baseUrl,
    authToken: c.token || undefined,
    fetch: (input, init) => (impl ? impl(input, init) : fetch(input, init)),
  });
}
