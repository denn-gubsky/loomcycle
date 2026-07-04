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
