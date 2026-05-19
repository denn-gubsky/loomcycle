/**
 * Wire-shape types for the loomcycle HTTP+SSE surface. Field names use
 * snake_case to match the Go server's JSON output (no client-side
 * conversion — what's on the wire is what you get).
 *
 * Public API method *parameters* use camelCase (JS norm); see
 * `client.ts` for the input shapes (RunOptions, CreateSnapshotOptions,
 * etc.) — those are translated to snake_case in the request body.
 */

// ---- Run lifecycle ----

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

export interface ContinueOptions {
  /** Required — the session to continue. */
  sessionId: string;
  segments: PromptSegment[];
  allowedTools?: string[];
  /** Pin the continuation to a specific running agent_id.
   *  Optional — when set, the server validates that the agent is
   *  live for this session before accepting; rejects with
   *  AgentNotFoundError / SessionBusyError otherwise. Mirrors
   *  Python adapter's ContinueOptions.agent_id (the Python field
   *  is snake_case; the wire field server-side is `agent_id`). */
  agentId?: string;
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

// ---- Agent metadata ----

export type AgentStatus = "running" | "completed" | "failed" | "cancelled";

export interface AgentUsage {
  input_tokens?: number;
  output_tokens?: number;
  cache_creation_tokens?: number;
  cache_read_tokens?: number;
  model?: string;
}

export interface Agent {
  agent_id: string;
  run_id: string;
  session_id: string;
  agent: string;
  parent_agent_id: string | null;
  user_id: string;
  status: AgentStatus;
  started_at: string;
  completed_at: string | null;
  stop_reason: string | null;
  error: string | null;
  usage: AgentUsage;
  last_heartbeat_at: string | null;
  live: boolean;
}

export interface ListAgentsResponse {
  agents: Agent[];
}

export interface CancelAgentResult {
  /** Number of agents marked cancelled (root + descendants reached
   *  via parent_agent_id cascade). 0 when the agent had already
   *  terminated; the call still succeeds (idempotent contract). */
  cancelledCount: number;
}

// ---- Transcript ----

/** TranscriptEvent — one persisted store.Event from
 *  GET /v1/sessions/{id}/transcript. The server wraps each
 *  providers.Event in {seq, run_id, ts_ns, type, event:{...}}. */
export interface TranscriptEvent {
  seq: number;
  run_id: string;
  ts_ns: number;
  type: string;
  event: unknown;
}

export interface TranscriptResponse {
  session: {
    id: string;
    user_id: string;
    agent: string;
    created_at: string;
  };
  events: TranscriptEvent[];
}

// ---- Health ----

export interface HealthResponse {
  ok: boolean;
  commit?: string;
  built?: string;
  uptime_seconds?: number;
  version?: string;
}

// ---- Admin: users list ----

export interface UserSummary {
  user_id: string;
  running_count: number;
  total_count: number;
  last_started_at: string;
}

export interface ListUsersResponse {
  users: UserSummary[];
}

// ---- Pause / Resume / State (v0.8.17 wire shape, v0.8.18 real impls) ----

export type RuntimeStateStatus = "running" | "pausing" | "paused";

export interface PauseResult {
  state: string; // "paused"
  duration_ms: number;
  force_cancelled_count: number;
  paused_runs_count: number;
  warnings?: string[];
}

export interface ResumeResult {
  state: string; // "running"
  resumed_runs_count: number;
  warnings?: string[];
}

export interface RuntimeStateResponse {
  state: RuntimeStateStatus;
  paused_runs_count: number;
}

// ---- Snapshot ----

export interface SnapshotDescriptor {
  id: string;
  created_at: string;
  label?: string;
  schema_version: number;
  byte_size: number;
}

export interface SnapshotListResponse {
  entries: SnapshotDescriptor[];
}

export interface SnapshotCreateResponse {
  id: string;
  created_at: string;
  label?: string;
  schema_version: number;
  byte_size: number;
}

/** Full envelope returned by GET /v1/_snapshots/{id}. json_content
 *  carries the canonical envelope as a parsed JSON object — pipe
 *  it directly into restoreSnapshot's `json` field. */
export interface SnapshotEnvelope {
  id: string;
  created_at: string;
  label?: string;
  schema_version: number;
  byte_size: number;
  json_content: unknown;
}

export interface CreateSnapshotOptions {
  /** Free-text marker stored on the snapshots.label column. */
  label?: string;
  /** Capture the optional interaction_history section (large; opt-in). */
  includeHistory?: boolean;
  /** RFC3339 timestamp; only honoured when `includeHistory` is true. */
  includeHistorySince?: string;
  /** Override the operator's LOOMCYCLE_SNAPSHOT_MAX_BYTES cap. 0 = default. */
  maxBytes?: number;
}

export interface SnapshotRestoreResponse {
  agent_defs_restored?: number;
  agent_def_active_restored?: number;
  memory_restored?: number;
  channel_messages_restored?: number;
  channel_cursors_restored?: number;
  evaluations_restored?: number;
  paused_runs_restored?: number;
  synthesized_sessions?: number;
  transcript_events_restored?: number;
  interaction_history_restored?: number;
  warnings?: string[];
}

// ---- Memory admin ----

export interface MemoryScopeKind {
  name: string;
  description: string;
}

export interface MemoryScopesResponse {
  scopes: MemoryScopeKind[];
}

export interface MemoryScopeIDSummary {
  scope_id: string;
  key_count: number;
  bytes: number;
  updated_at: string;
}

export interface MemoryScopeIDsResponse {
  scope: string;
  scope_ids: MemoryScopeIDSummary[];
}

export interface MemoryEntry {
  key: string;
  value: unknown;
  expires_at?: string;
  created_at: string;
  updated_at: string;
}

export interface MemoryEntriesResponse {
  scope: string;
  scope_id: string;
  entries: MemoryEntry[];
  truncated: boolean;
}

export interface MemoryEntryResponse {
  scope: string;
  scope_id: string;
  entry: MemoryEntry;
}

// ---- Interruption ----

export type InterruptStatus = "pending" | "answered" | "cancelled" | "expired";

export interface InterruptRow {
  interrupt_id: string;
  run_id: string;
  kind: string;
  status: InterruptStatus;
  question?: string;
  options?: string[];
  context_data?: string;
  priority: string;
  answer?: string;
  created_at: string;
  expires_at?: string;
  resolved_at?: string;
  resolved_by?: string;
  user_id?: string;
  agent_id?: string;
  agent_name?: string;
}

export interface InterruptListResponse {
  interrupts: InterruptRow[];
  total: number;
}

export interface ResolveInterruptOptions {
  /** The human's answer. When the original ask declared options,
   *  MUST be one of them (server-side validated). */
  answer: string;
  /** Audit attribution for who resolved it (free-form). Defaults
   *  server-side to "client" when omitted. */
  resolvedBy?: string;
  /** Discriminator. v0.8.16 supports only "question"; reserved
   *  for v0.9.x future kinds. */
  kind?: string;
}

// ---- Hook management (hooks-connector series, PR C) ----
//
// Hook *registration* is a client concern surfaced here. Hook
// *callback delivery* is server-side: loomcycle POSTs PreHookCall /
// PostHookCall payloads to the consumer's registered callback_url —
// that endpoint is whatever web framework JobEmber / the consumer
// runs. The TS adapter exports the PreHookCall / PostHookCall /
// PreHookResult / PostHookResult shapes so consumers can type their
// receiver code identically to the server's wire emit, but the adapter
// itself never runs them — it only manages the registration.

export type HookPhase = "pre" | "post";

export type HookFailMode = "open" | "closed";

/** Hook is the full descriptor returned by listHooks. The id +
 *  registered_at are loomcycle-assigned; the rest mirrors what was
 *  POSTed to registerHook. Field names use snake_case to match the
 *  Go server's JSON output. */
export interface Hook {
  id: string;
  owner: string;
  name: string;
  phase: HookPhase;
  agents: string[];
  tools: string[];
  callback_url: string;
  fail_mode: HookFailMode;
  timeout_ms: number;
  registered_at: string; // ISO 8601
}

/** RegisterHookOptions uses camelCase (JS norm for method parameters)
 *  and is translated to snake_case in the request body — same split as
 *  RunOptions / CreateSnapshotOptions. */
export interface RegisterHookOptions {
  /** App UID; (owner, name) is the identity tuple. Re-registering the
   *  same pair replaces the prior entry with a fresh id. */
  owner: string;
  name: string;
  phase: HookPhase;
  /** Agent name globs (exact match or trailing-* prefix). Empty list
   *  matches every agent (equivalent to ["*"]). */
  agents?: string[];
  /** Tool name globs (same syntax). Empty matches every tool. */
  tools?: string[];
  /** http:// or https:// URL loomcycle POSTs PreHookCall /
   *  PostHookCall payloads to. */
  callbackUrl: string;
  /** "open" (default) — webhook errors pass through. "closed" — webhook
   *  errors fail the tool call with IsError=true. */
  failMode?: HookFailMode;
  /** Per-call timeout. 0 / omitted = registry default (5 s). */
  timeoutMs?: number;
}

export interface RegisterHookResponse {
  /** Loomcycle-assigned id. Use it on deleteHook. */
  id: string;
}

export interface ListHooksResponse {
  hooks: Hook[];
}

// ---- Hook callback payloads ----
//
// These describe what the consumer's callback_url endpoint RECEIVES
// from loomcycle. The adapter doesn't post these — the operator's
// hooks.Dispatcher does. Exposed here so consumers can type their
// receiver code (e.g. an Express handler, a Next.js route) against
// the same shapes the server emits.

export interface HookToolCall {
  id: string;
  name: string;
  /** Raw JSON the model produced; consumers parse as needed. */
  input: unknown;
}

export interface HookToolResult {
  text: string;
  is_error?: boolean;
}

export interface PreHookCall {
  phase: "pre";
  owner: string;
  hook_name: string;
  agent: string;
  user_id?: string;
  agent_id?: string;
  tool_call: HookToolCall;
}

/** Response a Pre webhook returns. All fields optional; empty body
 *  means "pass through unchanged". See the server-side docs for the
 *  allow_hosts confused-deputy hazard before populating it. */
export interface PreHookResult {
  /** Rewrite the tool input. Tool runs with this instead of the
   *  model's original payload. */
  input?: unknown;
  /** Short-circuit the call: model sees this synthetic result. When
   *  set, allow_hosts is dropped (deny wins; we do not let a denied
   *  hook contribute hostnames to peers in the chain). */
  deny?: HookToolResult;
  /** Per-call host approvals. Only takes effect when the registering
   *  owner is in the operator yaml's hooks.permit_host_widen.owners.
   *  Read the SECURITY note in internal/hooks/types.go before using —
   *  this is a confused-deputy attack surface. */
  allow_hosts?: string[];
}

export interface PostHookCall {
  phase: "post";
  owner: string;
  hook_name: string;
  agent: string;
  user_id?: string;
  agent_id?: string;
  tool_call: HookToolCall;
  tool_result: HookToolResult;
}

/** Response a Post webhook returns. When result is omitted the tool
 *  result passes through unchanged. */
export interface PostHookResult {
  result?: HookToolResult;
}
