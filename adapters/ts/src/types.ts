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
  | "error"
  // v0.8.x extras already on the wire from internal/providers/provider.go.
  // Adding them here as additive options — consumers that only switch on
  // the base seven keep working, while consumers that already render
  // retries/host-widening/etc. can type-narrow.
  | "retry"
  | "host_widened"
  // v0.4 side-channel SSE frames emitted via sse.sendRaw (the JSON payload
  // doesn't carry `type` server-side; parseSSE backfills it from the SSE
  // event-name so the consumer sees a well-formed AgentEvent).
  | "session"
  | "agent";

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
  /** Forward-compat: a future wire bump may include the provider-billed
   *  USD cost alongside token counts. Today the sidecar never populates
   *  this; the field stays optional so consumers can plumb it without a
   *  wire change. */
  cost_usd?: number;
}

/** RetryInfo accompanies an `event: retry` frame (EventRetry in the Go
 *  server). Surfaced live during the retry sleep — useful for "waiting on
 *  rate limit" UI. The agent loop is unaffected; the retry is invisible
 *  to it. Wire-stable; mirrors providers.RetryInfo. */
export interface RetryInfo {
  provider: string;
  attempt: number;
  wait_ms: number;
  /** One of providers.RetryReason* constants — "retry-after header" or
   *  "exponential backoff" today. Stable wire string; do not parse. */
  reason: string;
}

/** HostWidening accompanies an `event: host_widened` frame (v0.8.17+).
 *  Emitted once per dispatched tool call whose Pre-hook allow_hosts grant
 *  fired. Operators audit confused-deputy patterns by comparing `url`'s
 *  host to `hosts_added`. Wire-stable; mirrors providers.HostWideningEventInfo. */
export interface HostWidening {
  tool_call_id: string;
  tool_name: string;
  url: string;
  hook_owner: string;
  hook_name: string;
  hosts_added: string[];
}

export interface AgentEvent {
  type: EventType;
  text?: string;
  tool_use?: ToolUse;
  usage?: Usage;
  error?: string;
  /** is_error flags a tool_result whose execution failed. Surviving the
   *  persist+replay round-trip matters because a continuation that lost
   *  the flag would re-feed the model a successful-looking result. */
  is_error?: boolean;
  stop_reason?: string;
  /** Retry payload on `event: retry`. Nil on all other event types. */
  retry?: RetryInfo;
  /** Host-widening payload on `event: host_widened`. Nil on all other
   *  event types. */
  host_widening?: HostWidening;
  // v0.4 `event: agent` side-channel announces the run's tracking IDs
  // immediately after the `event: session` frame. parent_agent_id is null
  // for top-level runs.
  agent_id?: string;
  run_id?: string;
  session_id?: string;
  parent_agent_id?: string | null;
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
  /** Per-request URL allowlist (v0.3.3+). Three-state on the wire:
   *  - omitted / `undefined` — no narrowing (operator's static list applies).
   *  - `null` — same as omitted (pass-through; convenience for callers
   *    that thread a possibly-unset slice).
   *  - `[]` — deny all (every network call refuses).
   *  - `["host1.com", ...]` — intersection with the operator's list
   *    (caller can shrink, never widen). */
  allowedHosts?: string[] | null;
  /** Brave-side filtering when `allowedHosts` is set:
   *  - "drop" (default) — Brave results outside the intersected list
   *    are omitted; the model only sees URLs it can follow up with WebFetch.
   *  - "keep" — Brave's full result set passes through; caller filters
   *    downstream.
   *  Ignored when `allowedHosts` is unset. */
  webSearchFilter?: "drop" | "keep";
  /** Bind the run to an existing session (v0.x). When set, the new run is
   *  appended to that session (transcript is NOT replayed by /v1/runs —
   *  use continueSession for replay semantics). When empty, a fresh
   *  session is created and announced as the first SSE frame. */
  sessionId?: string;
  tenantId?: string;
  /** Caller-supplied user binding (v0.4+). Records the run under this
   *  user_id for cancel/list endpoints; sub-agents inherit it. Charset:
   *  [A-Za-z0-9_-]{1,128}. */
  userId?: string;
  /** Caller-supplied tracking handle (v0.4+). When omitted, the server
   *  generates one and announces it in `event: agent`. Addresses the run
   *  for status/cancel via /v1/agents/{agent_id}. Charset:
   *  [A-Za-z0-9_-]{1,128}. */
  agentId?: string;
  /** Per-user tier name (v0.8.2+). Maps to `user_tiers.{name}` in the
   *  sidecar config (provider_priority + optional per-agent overlay).
   *  Server 400s on unknown tier. When omitted, falls through to
   *  `user_tiers.default`. */
  userTier?: string;
  /** Per-run MCP bearer token (v0.8.x+). Substituted into MCP HTTP header
   *  values containing `${run.user_bearer}` at outbound request-build time.
   *  Charset: [A-Za-z0-9._\-+/=]{16,512}. Empty is backwards-compatible
   *  (static-bearer setups unaffected). Sub-agents inherit identically.
   *  Never persisted; never logged in full. */
  userBearer?: string;
  signal?: AbortSignal;
}

export interface ContinueOptions {
  /** Required — the session to continue. */
  sessionId: string;
  segments: PromptSegment[];
  allowedTools?: string[];
  /** Per-call URL allowlist. Same three-state semantics as
   *  RunOptions.allowedHosts — continuations re-supply the list each
   *  time rather than inheriting from the seed run. */
  allowedHosts?: string[] | null;
  /** Brave-side filtering when allowedHosts is set. See RunOptions. */
  webSearchFilter?: "drop" | "keep";
  /** Pin the continuation to a specific running agent_id.
   *  Optional — when set, the server validates that the agent is
   *  live for this session before accepting; rejects with
   *  AgentNotFoundError / SessionBusyError otherwise. Mirrors
   *  Python adapter's ContinueOptions.agent_id (the Python field
   *  is snake_case; the wire field server-side is `agent_id`). */
  agentId?: string;
  /** Per-call user tier (v0.8.2+). Unlike user_id (session-bound),
   *  user_tier is per-request so a user upgrading mid-session sees
   *  the new tier applied to the next continuation. */
  userTier?: string;
  /** Per-call MCP bearer (v0.8.x+). Per-request (not session-bound)
   *  so different continuations in the same session may carry
   *  different end-user tokens. */
  userBearer?: string;
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
 *  providers.Event in {seq, run_id, ts_ns, type, event:{...}}.
 *
 *  `payload` is the v0.9.1 sidecar field carrying the typed body of
 *  events that don't fit the providers.Event union (the first-cycle
 *  `system_prompt` + `user_input` transcript events). Narrow on
 *  `type` to pick the right payload interface — see
 *  {@link SystemPromptPayload} and {@link UserInputPayload}. */
export interface TranscriptEvent {
  seq: number;
  run_id: string;
  ts_ns: number;
  type: string;
  event: unknown;
  /** v0.9.1+ sidecar for typed transcript events:
   *    type === "system_prompt" → SystemPromptPayload
   *    type === "user_input"    → UserInputPayload[]
   *  Absent for events the server hands through via `event`. */
  payload?: SystemPromptPayload | UserInputPayload[] | unknown;
}

/** UserInputPayload mirrors the JSON of one `loop.PromptSegment` —
 *  what the caller supplied as `segments` on POST /v1/runs +
 *  /v1/sessions/{id}/messages. The transcript event's `payload`
 *  field carries the FULL array (`UserInputPayload[]`) because one
 *  call may include multiple segments (system + user prepends, etc.). */
export interface UserInputPayload {
  role: string; // "system" | "user"
  content: Array<{ type: string; text?: string; cacheable?: boolean }>;
}

/** SystemPromptPayload mirrors the v0.9.1 system_prompt transcript
 *  event payload — the resolved system prompt + provenance metadata
 *  so operators can see WHICH AgentDef + WHICH SkillDef rows fed in. */
export interface SystemPromptPayload {
  system_prompt: string;
  /** Empty for yaml-only agents (no AgentDef row). Pinned for
   *  sub-runs spawned via the Agent tool with a def_id. */
  agent_def_id?: string;
  /** skillName → active SkillDef def_id. Only present for skills
   *  whose DB-active row supplied the body; static-fallback skills
   *  are absent. */
  skill_def_ids?: Record<string, string>;
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

// ---- v0.8.22 substrate admin (AgentDef + SkillDef) ----

/** Input shape for {@link LoomcycleClient.agentDef} and
 *  {@link LoomcycleClient.skillDef}. Mirrors the in-process tool
 *  input — `op` discriminates create / fork / get / list / promote
 *  / retire and the remaining fields are op-specific.
 *
 *  Typed loosely because the in-process tool owns the full schema;
 *  the adapter doesn't re-validate. Use the optional `extra` index
 *  signature for forward-compat fields. */
export type SubstrateToolInput = {
  op: "create" | "fork" | "get" | "list" | "promote" | "retire";
  name?: string;
  def_id?: string;
  parent_def_id?: string;
  overlay?: Record<string, unknown>;
  description?: string;
  promote?: boolean;
  retired?: boolean;
  [extra: string]: unknown;
};

/** Response shape for {@link LoomcycleClient.agentDef} and
 *  {@link LoomcycleClient.skillDef}. `unknown` because the shape
 *  varies per op — create/fork return a row envelope, list returns
 *  `{name, versions: [...]}`, promote/retire return summary shapes.
 *  Callers narrow as needed. */
export type SubstrateToolResponse = unknown;

/** Response a Post webhook returns. When result is omitted the tool
 *  result passes through unchanged. */
export interface PostHookResult {
  result?: HookToolResult;
}

// ---- v0.9.x n8n RFC Phase 0 ----

/** Aggregate stats for one operator-declared channel. Returned by
 *  {@link LoomcycleClient.listChannels}. */
export interface ChannelDescriptor {
  name: string;
  scope?: string;
  semantic?: string;
  publisher?: string;
  period?: string;
  default_ttl?: number;
  max_messages?: number;
  message_count: number;
  /** RFC3339 — empty when count == 0. */
  oldest_visible_at?: string;
  newest_visible_at?: string;
}

/** Response shape for {@link LoomcycleClient.listChannels}. */
export interface ListChannelsResponse {
  channels: ChannelDescriptor[];
}

/** One run state transition emitted by
 *  {@link LoomcycleClient.streamUserRunStates}. The TS field is RFC3339. */
export interface RunStateEvent {
  run_id: string;
  agent_id: string;
  agent: string;
  user_id: string;
  parent_agent_id?: string;
  status: string;
  stop_reason?: string;
  error?: string;
  ts: string;
}

/** Initial stream_open frame emitted before the first run_state. */
export interface RunStateStreamOpen {
  user_id: string;
  filter_status: string[] | null;
  filter_agent: string;
  keepalive_interval: number;
}

/** Yielded by {@link LoomcycleClient.streamUserRunStates}.
 *
 *  The first item is always `{ kind: "open", payload: RunStateStreamOpen }`.
 *  Subsequent items are `{ kind: "event", payload: RunStateEvent }`.
 *
 *  Consumers branch on `kind`; the `open` frame is useful for confirming
 *  the connection before any real events flow. */
export type RunStateStreamItem =
  | { kind: "open"; payload: RunStateStreamOpen }
  | { kind: "event"; payload: RunStateEvent };

/** Optional filter for {@link LoomcycleClient.streamUserRunStates}. */
export interface StreamUserRunStatesOptions {
  /** Subset of states to receive. Empty means all states. */
  statuses?: string[];
  /** Filter to one agent name. Empty means any. */
  agent?: string;
  signal?: AbortSignal;
}

// ---- v0.9.x content_sha256 verify op (AgentDef + SkillDef) ----

/** Response shape for `AgentDef set/fork/get/list` rows. Mirrors what
 *  the server-side rowResponseMap emits. The `content_sha256` field is
 *  the deterministic SHA-256 of the agent's content-bearing fields,
 *  prefixed `sha256:` (Docker image-digest convention). Empty on rows
 *  that pre-date v0.9.x and haven't been backfilled yet. */
export interface AgentDefRowResponse {
  def_id: string;
  name: string;
  version: number;
  parent_def_id?: string;
  description?: string;
  created_at: string;
  created_by_agent_id?: string;
  retired: boolean;
  bootstrapped_from_static: boolean;
  /** "sha256:" + 64 hex chars; empty for not-yet-backfilled rows. */
  content_sha256?: string;
  /** Only populated on `set` / `fork` responses (was the new row
   *  auto-promoted to active?). Absent on get/list. */
  promoted?: boolean;
}

/** Response shape for `AgentDef verify`. Answers "is the supplied
 *  content_sha256 the active deployed version of this name?"
 *
 *  - `matches: true`  — caller's local hash matches the deployed
 *                       active version; no push needed.
 *  - `matches: false` — bundle is out of sync; the operator should
 *                       push a new version via `agentDef({op: "set",
 *                       overlay: ...})`.
 *  - `deployed: false` — no active row exists for this name (no
 *                       deployment yet). matches is always false. */
export interface AgentDefVerifyResult {
  matches: boolean;
  /** Deployed active row's hash; empty when not deployed. */
  current_sha256: string;
  /** Deployed active row's def_id; empty when not deployed. */
  current_def_id: string;
  /** Deployed active row's version; 0 when not deployed. */
  version: number;
  name: string;
  /** True if an active row exists for this name. */
  deployed: boolean;
}

/** Response shape for `SkillDef verify`. Same semantics as
 *  AgentDefVerifyResult; the per-skill content basis is just
 *  smaller (name + description + body + allowed_tools). */
export interface SkillDefVerifyResult {
  matches: boolean;
  current_sha256: string;
  current_def_id: string;
  version: number;
  name: string;
  deployed: boolean;
}
