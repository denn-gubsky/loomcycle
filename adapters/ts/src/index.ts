/**
 * @loomcycle/client — minimal TypeScript client for the loomcycle
 * sidecar. Speaks JSON to POST /v1/runs and parses SSE frames into typed
 * AgentEvents.
 *
 * Designed to be a near-1:1 wrapper of the existing PromptSegment / AgentEvent
 * shape used in jobs-search-agent so existing callers can drop in.
 */

export type EventType =
  | "started"
  | "text"
  | "tool_call"
  | "tool_result"
  | "usage"
  | "done"
  | "error";

export interface ToolUse {
  id: string;
  name: string;
  input: unknown;
}

export interface Usage {
  input_tokens: number;
  output_tokens: number;
  cache_creation_input_tokens?: number;
  cache_read_input_tokens?: number;
  model?: string;
}

export interface AgentEvent {
  type: EventType;
  text?: string;
  tool_use?: ToolUse;
  usage?: Usage;
  error?: string;
  stop_reason?: string;
}

export type PromptContent =
  | { type: "trusted-text"; text: string; cacheable?: boolean }
  | { type: "untrusted-block"; kind: string; text: string };

export interface PromptSegment {
  role: "system" | "user";
  content: PromptContent[];
}

export interface RunOptions {
  agent: string;
  segments: PromptSegment[];
  allowedTools?: string[];
  signal?: AbortSignal;
}

export interface ClientOptions {
  /** Base URL of the sidecar, e.g. "http://127.0.0.1:8787". */
  baseUrl?: string;
  /** Bearer token for LOOMCYCLE_AUTH_TOKEN. Optional in dev mode. */
  authToken?: string;
  /** Custom fetch implementation; defaults to global fetch. */
  fetch?: typeof fetch;
}

export class LoomcycleClient {
  private baseUrl: string;
  private authToken?: string;
  private fetchImpl: typeof fetch;

  constructor(opts: ClientOptions = {}) {
    this.baseUrl = (opts.baseUrl ?? "http://127.0.0.1:8787").replace(/\/$/, "");
    this.authToken = opts.authToken;
    this.fetchImpl = opts.fetch ?? fetch;
  }

  /**
   * Run an agent and stream events. Returns an AsyncIterable<AgentEvent>;
   * the iterator completes when the server closes the SSE stream.
   *
   * Errors during the run surface as { type: "error", error } events; only
   * transport / HTTP-level failures throw.
   */
  async *runStreaming(opts: RunOptions): AsyncIterable<AgentEvent> {
    const headers: Record<string, string> = {
      "Content-Type": "application/json",
      Accept: "text/event-stream",
    };
    if (this.authToken) headers.Authorization = `Bearer ${this.authToken}`;

    const resp = await this.fetchImpl(`${this.baseUrl}/v1/runs`, {
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
      const body = await resp.text();
      throw new Error(`loomcycle ${resp.status}: ${body.slice(0, 200)}`);
    }
    if (!resp.body) {
      throw new Error("loomcycle: response has no body");
    }

    yield* parseSSE(resp.body.getReader());
  }
}

/**
 * parseSSE turns a chunked byte stream into typed AgentEvents.
 *
 * SSE framing (subset): "event: <name>\ndata: <json>\n\n". We only emit a
 * frame when both event + data have been seen since the last blank line.
 */
async function* parseSSE(
  reader: ReadableStreamDefaultReader<Uint8Array>,
): AsyncIterable<AgentEvent> {
  const decoder = new TextDecoder("utf-8");
  let buf = "";
  let event = "";
  let data = "";

  const flush = (): AgentEvent | null => {
    if (!event && !data) return null;
    if (!data) {
      event = "";
      return null;
    }
    try {
      const parsed = JSON.parse(data) as AgentEvent;
      event = "";
      data = "";
      return parsed;
    } catch {
      event = "";
      data = "";
      return null;
    }
  };

  while (true) {
    const { value, done } = await reader.read();
    if (done) break;
    buf += decoder.decode(value, { stream: true });

    let idx;
    while ((idx = buf.indexOf("\n")) !== -1) {
      const line = buf.slice(0, idx).replace(/\r$/, "");
      buf = buf.slice(idx + 1);

      if (line === "") {
        const ev = flush();
        if (ev) yield ev;
        continue;
      }
      if (line.startsWith("event:")) event = line.slice("event:".length).trim();
      else if (line.startsWith("data:")) data = line.slice("data:".length).trim();
    }
  }
  // Trailing frame without a final blank line.
  const ev = flush();
  if (ev) yield ev;
}
