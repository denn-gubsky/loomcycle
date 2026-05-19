/**
 * LoomcycleClient — the single public class exported by
 * @loomcycle/client. Speaks HTTP+SSE to a running loomcycle sidecar.
 *
 * PR 5a foundation: hosts the existing runStreaming() with
 * byte-identical behavior. PR 5b adds the remaining 21 methods
 * (continueSession + agent metadata + pause/snapshot + memory +
 * interrupts) on top.
 *
 * Construction:
 *
 *   const client = new LoomcycleClient({
 *     baseUrl: "http://127.0.0.1:8787",  // or process.env.LOOMCYCLE_BASE_URL
 *     authToken: "...",                  // or process.env.LOOMCYCLE_AUTH_TOKEN
 *   });
 *
 * Streaming methods return AsyncIterable<AgentEvent>; non-streaming
 * methods return Promise<T>. Non-2xx responses throw typed errors
 * from errors.ts via fetch-helpers.ts:raiseFromResponse.
 */

import type { _FetchContext } from "./fetch-helpers.js";
import { raiseFromResponse } from "./fetch-helpers.js";
import { parseSSE } from "./stream.js";
import type {
  AgentEvent,
  ClientOptions,
  RunOptions,
} from "./types.js";

export class LoomcycleClient {
  private ctx: _FetchContext;

  constructor(opts: ClientOptions = {}) {
    this.ctx = {
      baseUrl: (opts.baseUrl ?? "http://127.0.0.1:8787").replace(/\/$/, ""),
      authToken: opts.authToken,
      fetchImpl: opts.fetch ?? fetch,
    };
  }

  /**
   * Run an agent and stream events. Returns AsyncIterable<AgentEvent>;
   * the iterator completes when the server closes the SSE stream.
   *
   * Errors during the run surface as `{ type: "error", error }` events;
   * only transport / HTTP-level failures throw — and those throw typed
   * errors from errors.ts (e.g. AuthError for 401, BackpressureError
   * for 429).
   *
   * The wire shape is unchanged from v0.1.0-alpha.0: same request
   * body, same SSE frame parsing. jobs-search-agent's existing
   * runStreaming usage continues to work without changes.
   */
  async *runStreaming(opts: RunOptions): AsyncIterable<AgentEvent> {
    const headers: Record<string, string> = {
      "Content-Type": "application/json",
      // Accept BOTH text/event-stream (the success path) AND
      // application/json (the error path — non-2xx responses come
      // back as JSON so raiseFromResponse can extract typed errors).
      // Per the Streamable HTTP spec; strict reverse proxies in
      // front of the sidecar 406 otherwise. Same rationale as the
      // v0.8.x MCP HTTP-transport hardening note in CLAUDE.md.
      Accept: "text/event-stream, application/json",
    };
    if (this.ctx.authToken) headers.Authorization = `Bearer ${this.ctx.authToken}`;

    const resp = await this.ctx.fetchImpl(`${this.ctx.baseUrl}/v1/runs`, {
      method: "POST",
      headers,
      body: JSON.stringify({
        agent: opts.agent,
        segments: opts.segments,
        allowed_tools: opts.allowedTools,
      }),
      signal: opts.signal,
    });

    if (!resp.ok) {
      await raiseFromResponse(resp);
    }
    if (!resp.body) {
      // Defensive — the spec guarantees a body on streaming endpoints
      // but typing is conservative.
      throw new Error("loomcycle: response has no body");
    }

    yield* parseSSE(resp.body.getReader());
  }
}
