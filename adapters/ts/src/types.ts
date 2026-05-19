/**
 * Wire-shape types for the loomcycle HTTP+SSE surface. Field names use
 * snake_case to match the Go server's JSON output (no client-side
 * conversion — what's on the wire is what you get).
 *
 * Public API method *parameters* use camelCase (JS norm); see
 * `client.ts` for the input shapes (RunOptions, CreateSnapshotOptions,
 * etc.) — those are translated to snake_case in the request body.
 *
 * PR 5a foundation: Run lifecycle + Prompt + Event types only. PR 5b
 * adds the agent / transcript / pause / snapshot / memory / interrupt
 * types when the methods that need them land.
 */

// ---- Run lifecycle (existing, unchanged) ----

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

// ---- Client input shapes ----

export interface RunOptions {
  agent: string;
  segments: PromptSegment[];
  allowedTools?: string[];
  signal?: AbortSignal;
}

export interface ClientOptions {
  /** Base URL of the sidecar, e.g. "http://127.0.0.1:8787". Defaults to
   *  http://127.0.0.1:8787; in production, callers pass the deployed
   *  URL (LOOMCYCLE_BASE_URL equivalent). */
  baseUrl?: string;

  /** Bearer token for the Authorization header. Optional in
   *  open-mode deployments (LOOMCYCLE_AUTH_TOKEN unset on the server).
   *  When present, attached as `Authorization: Bearer <token>` on
   *  every request. */
  authToken?: string;

  /** Custom fetch implementation; defaults to global `fetch`.
   *  Useful for testing (vi.fn()) or for runtimes that ship their
   *  own fetch (e.g. node-fetch on older Node). */
  fetch?: typeof fetch;
}
