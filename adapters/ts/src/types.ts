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
