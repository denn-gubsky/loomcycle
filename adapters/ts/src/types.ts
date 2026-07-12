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
  | "agent"
  // RFC AI interactive session frames. `awaiting_input` = a persistent run
  // parked at end_turn (idle, ready for steering). `steer` = an operator
  // turn drained into the conversation, OR (on a streamRunByID re-attach) a
  // replayed prior operator turn (user_input.source === "replay").
  // `context_compaction` marks a context summarization.
  | "awaiting_input"
  | "steer"
  | "context_compaction"
  // RFC AW per-scope token budgets. `limit` = a server-generated token-budget
  // crossing (a soft warning at run start, or a soft crossing mid-run). The
  // structured payload rides `AgentEvent.limit`.
  | "limit"
  // v0.9.x — client-synthesized lifecycle events emitted ONLY when the
  // streaming caller passes `debug: true`. Never originate from the
  // server. The leading underscore signals "synthetic, not on the wire."
  // Consumers that opt out (default) never see this `type`.
  | "_meta";

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
  /** Serving model's context-window ceiling, stamped by the loop on each
   *  per-iteration usage event from Provider.Capabilities(). 0/absent =
   *  unknown (e.g. Ollama). Lets a client render a "context used / max"
   *  gauge without a hard-coded per-model table. Additive + optional. */
  max_context_tokens?: number;
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

/** LimitInfo accompanies an `event: limit` frame (RFC AW per-scope token
 *  budgets). Names which scope tripped, how hard (`soft` warns + the run
 *  continues; `hard` means the NEXT run is refused at admission), and where the
 *  scope stands against its ceiling — so a UI can render "tenant acme at 1.2M /
 *  1M tokens this month" without a follow-up fetch. Wire-stable; mirrors
 *  providers.LimitInfo. */
export interface LimitInfo {
  /** Which axis tripped: "operator" | "tenant" | "user". */
  scope: string;
  /** The tripped scope's id — tenant id (scope=tenant), user subject
   *  (scope=user), "" (operator-global). */
  scope_id?: string;
  /** "soft" (warn, run continues) | "hard" (next run refused at admission). */
  severity: string;
  /** Budget window; "month" (calendar month, UTC) in Phase 1. */
  window: string;
  /** The scope's month-to-date token total at the crossing. */
  used: number;
  /** The tier that was crossed (the soft or hard ceiling). */
  limit: number;
  /** Human-readable banner string. Optional. */
  message?: string;
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
  /** Payload on `event: awaiting_input` (RFC AI) — a persistent interactive
   *  run parked at end_turn. `since_turn` is the iteration it parked after. */
  awaiting_input?: { since_turn?: number };
  /** Payload on `event: steer` (RFC AI) — the operator's drained turn. On a
   *  re-attach replay, `source` is `"replay"`. Nil on all other event types. */
  user_input?: { text?: string; source?: string; seen_at?: string };
  /** Payload on `event: limit` (RFC AW) — a per-scope token-budget crossing.
   *  Nil on all other event types. */
  limit?: LimitInfo;
  // v0.4 `event: agent` side-channel announces the run's tracking IDs
  // immediately after the `event: session` frame. parent_agent_id is null
  // for top-level runs.
  agent_id?: string;
  run_id?: string;
  session_id?: string;
  parent_agent_id?: string | null;
  // v0.12.x — opaque caller-tracking lineage on the `event: agent` frame
  // (and inherited by sub-agents). Present only when the run carried it.
  parent_context?: ParentContext;
  // v0.9.x — client-synthesized observability fields, populated only on
  // events of `type: "_meta"`. `meta_subtype` is always set on a
  // _meta event (distinguishes open from close). `meta_reason` is set
  // ONLY on stream_close frames — carrying the cause ("eof" on clean
  // close, "AbortError"/"AuthError"/etc on a typed-error throw). The
  // stream_open frame leaves meta_reason undefined — the frame itself
  // is the signal that the stream just opened.
  meta_subtype?: "stream_open" | "stream_close";
  meta_reason?: string;
}

export type PromptContent =
  | { type: "trusted-text"; text: string; cacheable?: boolean }
  | { type: "untrusted-block"; kind: string; text: string }
  // Image input (RFC AT), valid only in a user segment. `data` is the
  // base64-encoded image bytes with NO "data:" prefix; there is deliberately
  // no URL form (SSRF). The model must be vision-capable or the run errors
  // before the call.
  | { type: "image"; media_type: ImageMediaType; data: string };

/** Whitelisted image media types accepted on an `image` content block (RFC AT). */
export type ImageMediaType =
  | "image/png"
  | "image/jpeg"
  | "image/gif"
  | "image/webp";

export interface PromptSegment {
  role: "system" | "user";
  content: PromptContent[];
}

export interface RunOptions {
  agent: string;
  segments: PromptSegment[];
  tools?: string[];
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
  /** Per-tool named credentials map (v1.x RFC F). Per-MCP-server bearers
   *  keyed by operator-chosen name (convention: the `mcp_servers.<name>`
   *  yaml key). Substituted into MCP HTTP header values containing
   *  `${run.credentials.<name>}` at outbound request-build time. Keys
   *  match `[a-zA-Z0-9_-]{1,64}`; values arbitrary strings. Sub-agents
   *  inherit the whole map. Coexists with `userBearer` — the legacy
   *  field auto-promotes to `userCredentials.default` for back-compat
   *  with v0.8.x flows. Never persisted; never logged. */
  userCredentials?: Record<string, string>;
  /** Opaque caller-tracking lineage (v0.12.x). Carried verbatim by the
   *  runtime, inherited UNCHANGED by every sub-agent the Agent tool
   *  spawns, persisted on each run row, and echoed back on the per-agent
   *  report surfaces (agent status, run-state stream, the `event: agent`
   *  frame) — so you can attribute a child sub-agent's usage to the
   *  user-initiated request that spawned the whole tree. Not a secret.
   *  Omitted = no tracking context. */
  parentContext?: ParentContext;
  /** Optional NON-SECRET structured metadata passed to the agent (repo
   *  name, review policy, preferred skills, …) — symmetric with the
   *  WebHook/Schedule trigger paths. As a first-party (bearer-authed)
   *  caller this is TRUSTED: a code-js agent reads it as `input.metadata`;
   *  an LLM agent receives it as a trusted prompt block. NOT for secrets —
   *  use {@link RunOptions.userCredentials} for tokens. Per-call, not session
   *  state: a continuation does not inherit it — re-send on continue(). */
  metadata?: Record<string, unknown>;
  /** Optional ad-hoc per-run wall-clock budget (seconds) for a CODE-JS agent,
   *  overriding the agent's `run_timeout_seconds` and the sidecar's global
   *  default (precedence: per-run > per-agent > global). Use it for a fan-out
   *  orchestrator that blocks in Agent.parallel_spawn awaiting LLM children —
   *  its budget spans that wait, so the CPU-oriented default is often too low.
   *  Ignored by LLM agents. 0 / omitted = inherit. */
  runTimeoutSeconds?: number;
  /** Per-run LLM sampling override (v0.28.0), merged PER FIELD over the
   *  agent's own sampling (this wins; unset fields inherit). Omitted =
   *  inherit entirely. */
  sampling?: SamplingOptions;
  /** Per-run context-compaction override (v0.32.0), merged PER FIELD over
   *  the agent's own compaction block (this wins; unset fields inherit).
   *  Omitted = inherit entirely. Trigger compaction mid-run with
   *  {@link LoomcycleClient.compactRun}. */
  compaction?: CompactionOptions;
  /** RFC AI — start a PERSISTENT interactive run that parks at end_turn
   *  awaiting operator steering instead of terminating. The stream emits an
   *  `awaiting_input` frame when it parks; drive it with
   *  {@link LoomcycleClient.sendRunInput}, re-attach with
   *  {@link LoomcycleClient.streamRunByID}, and `cancelAgent` to end it.
   *  Higher-level: {@link LoomcycleClient.interactiveSession}. */
  interactive?: boolean;
  /** Opt-in observability: when true, the iterator emits client-
   *  synthesized `{ type: "_meta", meta_subtype: "stream_open" | "stream_close" }`
   *  events around the real event stream. `meta_reason` carries the
   *  trigger ("eof", "abort", or an error class name). Default is
   *  false — existing consumers see no behaviour change. Useful for
   *  n8n nodes that want to surface "stream re-opened" / "stream
   *  closed" log entries without inferring from event timing. */
  debug?: boolean;
  signal?: AbortSignal;
}

/** Per-run LLM sampling override (v0.28.0). Mirrors config.Sampling — every
 *  field optional; an unset field inherits the agent's value. An explicit
 *  `temperature: 0` is deterministic, NOT "unset". Each provider maps what it
 *  supports (e.g. topK is Anthropic/Gemini/Ollama; frequencyPenalty/
 *  presencePenalty/seed are OpenAI/DeepSeek/Ollama). */
export interface SamplingOptions {
  temperature?: number;
  topP?: number;
  topK?: number;
  frequencyPenalty?: number;
  presencePenalty?: number;
  seed?: number;
  stop?: string[];
}

/** Per-run context-compaction override (v0.32.0). Mirrors config.Compaction —
 *  every field optional; an unset field inherits the agent's value. */
export interface CompactionOptions {
  /** Turn AUTO-compaction on for this run (default off). */
  enabled?: boolean;
  /** Summary aims for ~N% of the compacted span's length (10..50; default 10). */
  targetPercentage?: number;
  /** Keep the last N messages verbatim (default 4; 0 = summarize all). */
  keepLastN?: number;
  /** Pin the first user message (the task) verbatim (default true). */
  keepFirst?: boolean;
  /** Auto-compact when used/window ≥ N% (50..95; default 80; only when
   *  enabled + the provider reports a context window). */
  autocompactAtPct?: number;
  /** Run the summary call on a cheaper/faster model served by the SAME
   *  provider. Omitted = the run's model. */
  model?: string;
}

/** Opaque caller-tracking lineage (v0.12.x) attached to a run and
 *  propagated to all its sub-agents. The runtime stores and echoes
 *  these fields verbatim and never interprets them. All fields
 *  optional; an all-empty object is treated as absent. */
export interface ParentContext {
  /** The consumer's identifier for the user-initiated run at the root of
   *  the spawn tree — echoed on every descendant. */
  root_agent_run_id?: string;
  /** The consumer's logical-operation key for the root request. */
  function_key?: string;
  /** The consumer's tier marker captured at root-run time (distinct from
   *  `userTier`, which is loomcycle's resolver policy). */
  tier_at_run?: string;
}

export interface ContinueOptions {
  /** Required — the session to continue. */
  sessionId: string;
  segments: PromptSegment[];
  tools?: string[];
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
  /** Per-tool named credentials map (v1.x RFC F). See
   *  {@link RunOptions.userCredentials} for the full shape — same
   *  semantics, supplied per-continuation rather than per-fresh-run. */
  userCredentials?: Record<string, string>;
  /** Opaque caller-tracking lineage (v0.12.x). See
   *  {@link RunOptions.parentContext} — same shape; a continuation may
   *  (re)set the lineage for the new run it creates. */
  parentContext?: ParentContext;
  /** Optional NON-SECRET structured metadata for the new run — see
   *  {@link RunOptions.metadata}. Same shape + trust posture. NOT inherited
   *  from the original run (metadata is a per-call input, not session state):
   *  re-send it on the continuation to carry it forward. */
  metadata?: Record<string, unknown>;
  /** Optional ad-hoc per-run code-js wall-clock budget (seconds) for the
   *  continuation's new run — see {@link RunOptions.runTimeoutSeconds}. */
  runTimeoutSeconds?: number;
  /** Per-continuation LLM sampling override — see {@link RunOptions.sampling}. */
  sampling?: SamplingOptions;
  /** Per-continuation context-compaction override — see {@link RunOptions.compaction}. */
  compaction?: CompactionOptions;
  /** RFC AI — park this continuation at end_turn for operator steering. See
   *  {@link RunOptions.interactive}. */
  interactive?: boolean;
  /** Opt-in observability: see {@link RunOptions.debug}. Same shape. */
  debug?: boolean;
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
  // provider is the provider id that actually served the final
  // successful iteration (e.g. "anthropic", "deepseek"). Distinct
  // from model so consumers can tell primary-provider runs from
  // runtime-fallback routed runs. Added in v0.12.7 — pre-migration
  // server rows + pre-call failures omit this field.
  provider?: string;
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
  /** Opaque caller-tracking lineage this run carries (inherited from its
   *  root for sub-agents), v0.12.x. Echoed here alongside `usage` so you
   *  can attribute a child sub-agent's cost to the user-initiated request
   *  in a single fetch. Omitted when the run carried no context. */
  parent_context?: ParentContext;
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

// ---- Fan-out (RFC Y) + compaction ----

/** Options for {@link LoomcycleClient.spawnRunBatch} — the RFC Y external
 *  fan-out. Each spawn is a fresh-run {@link RunOptions} (its `sessionId` is
 *  ignored — batch children never continue a session; `signal`/`debug` are
 *  per-call client concerns and not sent). Capped at 32; the server rejects an
 *  over-cap batch. */
export interface RunBatchOptions {
  spawns: RunOptions[];
  /** "join" (default) — block until all children settle, returning the
   *  combined envelope. "detach" (async run handles) is reserved for a future
   *  release and rejected by the server today. */
  mode?: "join";
  /** Optional join deadline (ms): a child still running when it elapses is
   *  cancelled and reported with a cancelled status in-envelope. */
  timeoutMs?: number;
  signal?: AbortSignal;
}

/** One child run's outcome in a batch. Mirrors the server's SpawnResult wire
 *  shape; a per-child failure is reported via `status` + `error`, never as a
 *  thrown error (the batch as a whole still resolves). */
export interface SpawnRunResult {
  agent_id: string;
  run_id: string;
  session_id: string;
  status: AgentStatus;
  stop_reason?: string;
  final_text?: string;
  usage?: AgentUsage;
  error?: string;
}

/** Result of {@link LoomcycleClient.spawnRunBatch} — `results` is index-aligned
 *  with the request's `spawns`. */
export interface RunBatchResult {
  results: SpawnRunResult[];
  spawned: number;
}

/** Result of {@link LoomcycleClient.compactRun}. `applied` is "live" (pushed to
 *  the running loop), "marker" (persisted for a terminal run's next
 *  continuation), or "noop" (too short to compact). */
export interface CompactRunResult {
  run_id: string;
  compacted: boolean;
  before_tokens: number;
  after_tokens: number;
  applied: "live" | "marker" | "noop";
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

// ---- Whoami / principal (RFC L, v0.17.0) ----

/** GET /v1/_me — the authenticated principal resolved from the bearer.
 *  `open_mode` is true when the server runs without the OperatorTokenDef
 *  substrate (single shared LOOMCYCLE_AUTH_TOKEN); `legacy` is true for a
 *  shared-token principal when the substrate IS enabled. */
export interface WhoamiResponse {
  tenant_id: string;
  subject: string;
  scopes: string[];
  is_admin: boolean;
  legacy: boolean;
  open_mode?: boolean;
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

// ---- Resolver re-probe (issue #88) ----

export interface ResolverModelStatus {
  listed: boolean;
  stalled: boolean;
}

export interface ResolverProviderAvailability {
  excluded: boolean;
  reachable: boolean;
  models: Record<string, ResolverModelStatus>;
  /** RFC3339 timestamp of the last probe for this provider. */
  last_check: string;
  last_error?: string;
}

/** The resolver availability matrix, captured right after a forced
 *  re-probe. Same shape as GET /v1/_resolver. */
export interface ResolverMatrix {
  /** RFC3339 timestamp when this matrix snapshot was assembled. */
  generated_at: string;
  providers: Record<string, ResolverProviderAvailability>;
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
  // `delete` + `purge` are the flat VolumeDef lifecycle ops (RFC AH) — a
  // Volume points at mutable on-disk state, so it has no version chain to
  // retire/promote/fork. `delete` unmaps (keeps files); `purge` removes the
  // row AND the directory tree.
  op:
    | "create"
    | "fork"
    | "get"
    | "list"
    | "promote"
    | "retire"
    | "rediscover"
    | "verify"
    | "delete"
    | "purge";
  name?: string;
  def_id?: string;
  parent_def_id?: string;
  overlay?: Record<string, unknown>;
  description?: string;
  promote?: boolean;
  retired?: boolean;
  // VolumeDef create takes a flat `mode` (ro|rw); the runtime derives the
  // path, so callers never supply one.
  mode?: VolumeMode;
  [extra: string]: unknown;
};

/** Response shape for {@link LoomcycleClient.agentDef} and
 *  {@link LoomcycleClient.skillDef}. `unknown` because the shape
 *  varies per op — create/fork return a row envelope, list returns
 *  `{name, versions: [...]}`, promote/retire return summary shapes.
 *  Callers narrow as needed. */
export type SubstrateToolResponse = unknown;

// ---- RFC AP Agent Teams (TeamDef substrate) ----
//
// TeamDefs are agent-team workflow graphs (states + transitions + per-state
// handler agent). The team methods mirror the HTTP shapes in the Web UI's
// api.ts; the substrate is tenant-confined (the runtime stamps the caller's
// authoritative tenant), so a team is visible only within its own tenant.

/** One team's roll-up in {@link ListTeamsResponse} (GET /v1/_teamdef/names). */
export interface TeamNameSummary {
  name: string;
  tenant_id?: string;
  version_count: number;
  active_def_id?: string;
  latest_version: number;
  last_updated?: string;
  live_version_count?: number;
  active_retired?: boolean;
}

/** Response of {@link LoomcycleClient.listTeams}. `names` is `null` (not `[]`)
 *  when the tenant has no teams — mirrors the server's Go nil-slice encoding. */
export interface ListTeamsResponse {
  names: TeamNameSummary[] | null;
}

/** A rendered team diagram from {@link LoomcycleClient.renderTeamDiagram}
 *  (op=render_diagram). `diagram` is Mermaid `stateDiagram-v2` source. */
export interface TeamDiagram {
  name: string;
  def_id: string;
  format: string;
  diagram: string;
}

/** Result of {@link LoomcycleClient.createTeam} / {@link LoomcycleClient.forkTeam}
 *  (op=create / op=fork) — the newly-written version's identifiers. */
export interface CreatedTeam {
  def_id: string;
  name: string;
  version: number;
}

/** One team version's full record from {@link LoomcycleClient.getTeamDef}
 *  (op=get). `definition` is the stored graph, inlined as an object (the server
 *  stores it as raw JSON), suitable for loading into an editor. */
export interface TeamDefDetail {
  def_id: string;
  name: string;
  version: number;
  retired?: boolean;
  content_sha256?: string;
  definition: unknown;
}

/** Result of {@link LoomcycleClient.runTeam} (op=run) — the walk trace. `status`
 *  is `"completed"` (a terminal state was reached) or `"iteration_cap"` (a
 *  state's cycle cap tripped; `capped_state` + `iteration_count` describe it).
 *  `steps` is the per-state trace. Extra fields are tolerated (the run output
 *  varies by outcome), so callers narrow as needed. */
export interface TeamRunResult {
  name: string;
  def_id: string;
  status: string;
  final_state?: string;
  final_output?: string;
  capped_state?: string;
  max_iterations?: number;
  iteration_count?: number;
  steps: Array<Record<string, unknown>>;
  [extra: string]: unknown;
}

/** Input for {@link LoomcycleClient.path} — the RFC AL Unix-like VFS tool
 *  (POST /v1/_path). Op-discriminated; the server resolves scope + tenant
 *  from the authenticated principal, never the wire. Address Memory entries,
 *  Volume mounts, and Documents by human-readable paths (e.g. /docs/launch). */
export type PathToolInput = {
  op: "resolve" | "ls" | "stat" | "mkdir" | "mv" | "rm";
  /** Absolute path, e.g. /docs/launch. Segments are [a-zA-Z0-9._-]; no "..". */
  path?: string;
  /** Destination path (mv only). */
  to?: string;
  /** Which tree (default agent). user needs a user on the run; tenant is shared. */
  scope?: "agent" | "user" | "tenant";
  /** ls: list descendants. rm: required to remove a path that has descendants. */
  recursive?: boolean;
  /** ls: only entries of this kind (document/volume_mount/memory_entry/directory). */
  kind_filter?: string;
  /** rm: also delete the backing resource (NOT supported in v1). */
  resource_too?: boolean;
  [extra: string]: unknown;
};

/** Input for {@link LoomcycleClient.document} — the RFC AK chunked-graph
 *  Document tool (POST /v1/_document). Op-discriminated (16 ops); requires
 *  SQL Memory on the sidecar. Scope agent/user (tenant deferred). */
export type DocumentToolInput = {
  // Op order mirrors the backend enum (internal/tools/builtin/document.go)
  // exactly so a reader can line the two up 1:1. set_path attaches/re-homes a
  // Path-tree name for an existing document; export_md / import_md render to /
  // build from export_md-shaped Markdown.
  op:
    | "create_document"
    | "get_document"
    | "delete_document"
    | "set_path"
    | "create_chunk"
    | "get_chunk"
    | "update_chunk"
    | "delete_chunk"
    | "move_chunk"
    | "link_chunks"
    | "unlink_chunks"
    | "query_chunks"
    | "define_type"
    | "list_types"
    | "export_md"
    | "import_md";
  scope?: "agent" | "user";
  /** Document id (get/delete_document) or chunk id (get/update/delete/move_chunk). */
  id?: string;
  /** create_document: name the doc in the Path tree; get/delete: address by path. */
  path?: string;
  title?: string;
  document_id?: string;
  parent_id?: string;
  new_parent_id?: string;
  type?: string;
  body?: string;
  fields?: Record<string, unknown>;
  status?: string;
  position?: number;
  /** update_chunk: the chunk's current revision (optimistic concurrency). */
  revision?: number;
  from_id?: string;
  to_id?: string;
  kind?: string;
  /** query_chunks: restrict to documents at/under this Path-tree path. */
  under_path?: string;
  /** query_chunks: raw read-only SELECT (escape hatch; validator-gated). */
  sql?: string;
  limit?: number;
  /** define/list_types: the type name. */
  name?: string;
  /** export_md: embed round-trippable chunk metadata + edges as HTML comments
   *  (default true server-side). false = clean human-facing Markdown. */
  include_metadata?: boolean;
  /** import_md: an export_md-shaped Markdown document (headings = hierarchy;
   *  `<!-- loom: ... -->` metadata; `<!-- loom-edges: ... -->` trailer). Omit
   *  document_id to create a new document; pass it (+ optional parent_id) to
   *  import under an existing chunk. */
  markdown?: string;
  [extra: string]: unknown;
};

/** Response shape for {@link LoomcycleClient.path} and
 *  {@link LoomcycleClient.document}. `unknown` because it varies per op —
 *  callers narrow as needed (e.g. an `ls` returns `{path, entries}`, a
 *  `create_document` returns `{document_id, root_chunk_id, ...}`). */
export type PathToolResponse = unknown;
export type DocumentToolResponse = unknown;

/** Volume access mode — read-only or read-write (RFC AH). */
export type VolumeMode = "ro" | "rw";

/** One row of {@link LoomcycleClient.listVolumes} (`GET /v1/_volumes`). */
export interface PersistentVolumeEntry {
  name: string;
  /** "static" (operator yaml, read-only) or "dynamic" (a tenant VolumeDef). */
  source: "static" | "dynamic";
  /** Host path. Redacted to "" for a non-operator (tenant) caller — the
   *  volume universe is visible to a tenant operator, the host location is
   *  not. Operator-equivalent callers see the real path. */
  path: string;
  mode: VolumeMode;
  /** True for the static volume flagged `default: true` (never for dynamic). */
  default: boolean;
  /** True for the static volume dynamic VolumeDefs are provisioned inside. */
  dynamic_root: boolean;
  /** Set for dynamic rows (the substrate stamps it); absent for static. */
  created_at?: string;
}

export interface PersistentVolumesResponse {
  entries: PersistentVolumeEntry[];
}

/** One row of {@link LoomcycleClient.listEphemeralVolumes}
 *  (`GET /v1/_volumes/ephemeral`). */
export interface EphemeralVolumeEntry {
  name: string;
  root_run_id: string;
  /** Host path — redacted to "" for a non-operator caller (see
   *  {@link PersistentVolumeEntry.path}). */
  path: string;
  mode: VolumeMode;
  created_at: string;
}

export interface EphemeralVolumesResponse {
  entries: EphemeralVolumeEntry[];
}

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
  description?: string;
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
  /** v0.11.5: "yaml" (operator yaml — immutable from this surface),
   *  "runtime" (substrate — CRUD-mutable), "orphan" (no declaration,
   *  only orphan messages). */
  source?: "yaml" | "runtime" | "orphan" | string;
}

/** Response shape for {@link LoomcycleClient.listChannels}. */
export interface ListChannelsResponse {
  channels: ChannelDescriptor[];
}

/** Result of {@link LoomcycleClient.purgeChannel} — the channel name and
 *  the count of buffered messages cleared. */
export interface ChannelPurgeResult {
  name: string;
  purged: number;
}

// ---- v0.9.x Channel CRUD types ----

/** Scope selector for the Channel CRUD methods. `"global"` addresses
 *  the admin surface; `"user"` requires `userId` and addresses the
 *  per-end-user URL family. */
export type ChannelScope = "global" | "user";

/** Options for {@link LoomcycleClient.publishChannel}. `payload` is
 *  the raw JSON value (object, array, string, number) to publish.
 *  `deliverAt` (RFC3339Nano) defers the publish so long-poll
 *  subscribers wake at the visible_at time. */
export interface PublishChannelOptions {
  scope: ChannelScope;
  /** Required when scope === "user". The per-user URL is
   *  /v1/users/{userId}/channels/{channel}/publish. */
  userId?: string;
  payload: unknown;
  /** RFC3339Nano deferred-publish time. Omit for "publish now". */
  deliverAt?: string;
  signal?: AbortSignal;
}

/** Response shape for {@link LoomcycleClient.publishChannel}. */
export interface ChannelPublishResult {
  msg_id: string;
  channel: string;
  /** RFC3339Nano. */
  created_at: string;
  /** RFC3339Nano. Omitted when the publish was immediate. */
  visible_at?: string;
}

/** Options for {@link LoomcycleClient.subscribeChannel}. The call is a
 *  single-round-trip long-poll, NOT an open SSE stream — returns
 *  immediately if messages are present, otherwise waits up to
 *  `waitMs` for a publish. Auto-commits the cursor on a non-empty
 *  batch (at-most-once shape). For at-least-once, use
 *  {@link LoomcycleClient.peekChannel} + explicit ack. */
export interface SubscribeChannelOptions {
  scope: ChannelScope;
  userId?: string;
  /** Cursor to read forward from. Empty/omitted = the committed
   *  cursor. `"cur_0"` = replay from the oldest non-expired row. */
  fromCursor?: string;
  /** Defaults to 10; clamped at 100 by the server. */
  maxMessages?: number;
  /** Long-poll timeout in ms. 0 / omitted = poll once and return.
   *  Capped at the operator's `ChannelsLongPollCapMS` (default 30s). */
  waitMs?: number;
  signal?: AbortSignal;
}

/** One delivered message — same wire shape as the in-band Channel
 *  tool's subscribe response. */
export interface ChannelMessageItem {
  id: string;
  value: unknown;
  /** RFC3339Nano. */
  published_at: string;
}

/** Response shape for {@link LoomcycleClient.subscribeChannel}. */
export interface ChannelSubscribeResult {
  channel: string;
  messages: ChannelMessageItem[];
  /** Cursor to pass on the next subscribe call to continue forward.
   *  Empty when the batch is empty. */
  next_cursor: string;
}

/** Options for {@link LoomcycleClient.peekChannel}. */
export interface PeekChannelOptions {
  scope: ChannelScope;
  userId?: string;
  fromCursor?: string;
  maxMessages?: number;
  signal?: AbortSignal;
}

/** Response shape for {@link LoomcycleClient.peekChannel}. */
export interface ChannelPeekResult {
  channel: string;
  messages: ChannelMessageItem[];
}

/** Options for {@link LoomcycleClient.ackChannel}. */
export interface AckChannelOptions {
  scope: ChannelScope;
  userId?: string;
  cursor: string;
  signal?: AbortSignal;
}

/** Response shape for {@link LoomcycleClient.ackChannel}. */
export interface ChannelAckResult {
  ok: boolean;
}

// --- RFC S client twins: fan-in (await) / fan-out (broadcast) ---

/** Fan-in mode for {@link LoomcycleClient.awaitChannels}: `any` = ≥1
 *  channel has a message; `all` = every channel has ≥1; `at_least` =
 *  total messages across channels ≥ `n`. */
export type ChannelAwaitMode = "any" | "all" | "at_least";

/** Options for {@link LoomcycleClient.awaitChannels} — wait until the
 *  predicate is met across `channels`, or `waitMs` elapses. `scope` +
 *  `userId` apply to EVERY channel in the set. Non-committing. Max 32. */
export interface AwaitChannelsOptions {
  channels: string[];
  scope: ChannelScope;
  /** Required when scope === "user" — the shared scope_id for the set. */
  userId?: string;
  mode?: ChannelAwaitMode;
  /** Required (>0) when mode === "at_least". */
  n?: number;
  fromCursor?: string;
  maxMessages?: number;
  /** Long-poll timeout in ms; capped at the operator's LongPollCapMS. */
  waitMs?: number;
  signal?: AbortSignal;
}

/** One fired channel's accumulated messages + the (non-advanced) cursor. */
export interface ChannelAwaitEntry {
  messages: ChannelMessageItem[];
  next_cursor: string;
}

/** Response shape for {@link LoomcycleClient.awaitChannels}. `timed_out`
 *  is true only when the predicate was unmet within `waitMs` (never an
 *  error). `results` is keyed by channel name. */
export interface ChannelAwaitResult {
  satisfied: boolean;
  timed_out: boolean;
  mode: ChannelAwaitMode;
  fired: string[];
  total_messages: number;
  results: Record<string, ChannelAwaitEntry>;
}

/** Options for {@link LoomcycleClient.broadcastChannels} — publish the
 *  same `payload` to every channel in `channels`. Atomic at the declare
 *  pre-flight (one undeclared channel rejects the whole call). Max 32. */
export interface BroadcastChannelsOptions {
  channels: string[];
  scope: ChannelScope;
  userId?: string;
  payload: unknown;
  /** RFC3339Nano deferred-publish time. Omit for "publish now". */
  deliverAt?: string;
  signal?: AbortSignal;
}

/** One channel's publish outcome. `error` is set (and `msg_id` absent)
 *  when that channel's write failed after the pre-flight passed. */
export interface ChannelBroadcastEntry {
  channel: string;
  msg_id?: string;
  created_at?: string;
  visible_at?: string;
  error?: string;
}

/** Response shape for {@link LoomcycleClient.broadcastChannels}.
 *  `published` + `failed` = the deduped channel count. */
export interface ChannelBroadcastResult {
  published: number;
  failed: number;
  results: ChannelBroadcastEntry[];
}

// ---- v0.11.5 Channel admin CRUD types ----

/** Options for {@link LoomcycleClient.createChannel}. Operator-yaml
 *  channels are immutable from this surface; the server returns
 *  HTTP 409 `channel_yaml_immutable` when `name` matches a yaml-
 *  declared channel. */
export interface CreateChannelOptions {
  name: string;
  description?: string;
  /** "global" | "agent" | "user". Defaults to "global" if omitted. */
  scope?: string;
  /** "queue" | "topic". Defaults to "queue" if omitted. */
  semantic?: string;
  /** Seconds; 0 = no TTL. */
  default_ttl?: number;
  /** 0 = unbounded. */
  max_messages?: number;
  /** Free-form attribution; not enforced by the substrate. */
  publisher?: string;
  /** Free-form retention hint; not enforced by the substrate. */
  period?: string;
  signal?: AbortSignal;
}

/** Options for {@link LoomcycleClient.updateChannel}. Nil fields
 *  leave the corresponding channel attribute unchanged. */
export interface UpdateChannelOptions {
  description?: string;
  default_ttl?: number;
  max_messages?: number;
  /** "queue" | "topic" */
  semantic?: string;
  signal?: AbortSignal;
}

// ---- v0.11.5 Memory entry admin CRUD types ----

/** Options for {@link LoomcycleClient.setMemoryEntry}. `value` is
 *  opaque JSON. Setting `embed: true` triggers a synchronous embed
 *  via the operator-configured embedder; the returned `embedded`
 *  flag + optional `embed_warning` report whether the embedding
 *  landed. */
export interface SetMemoryEntryOptions {
  value: unknown;
  /** When true, also compute + store the embedding (requires
   *  memory.embedder yaml + a vector-capable store backend). */
  embed?: boolean;
  /** Optional TTL in seconds; <= 0 means "no expiry". */
  ttl_seconds?: number;
  signal?: AbortSignal;
}

/** Response shape for {@link LoomcycleClient.setMemoryEntry}. */
export interface SetMemoryEntryResponse {
  scope: string;
  scope_id: string;
  key: string;
  /** true when the embedding was computed AND stored. */
  embedded: boolean;
  /** Non-empty when embed was requested but failed (transient
   *  error, embedder unconfigured, vector backend not available).
   *  The k/v row still landed. */
  embed_warning?: string;
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
  /** Opaque caller-tracking lineage echoed on the transition (v0.12.x),
   *  so a subscriber learns which root request a finishing sub-agent
   *  belongs to. Omitted when the run carried no context. */
  parent_context?: ParentContext;
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
  | { kind: "event"; payload: RunStateEvent }
  // v0.9.x — emitted ONLY when StreamUserRunStatesOptions.debug=true.
  // Never on the wire; synthesized client-side at stream-close time.
  | { kind: "close"; payload: RunStateStreamClose };

/** Optional filter for {@link LoomcycleClient.streamUserRunStates}. */
export interface StreamUserRunStatesOptions {
  /** Subset of states to receive. Empty means all states. */
  statuses?: string[];
  /** Filter to one agent name. Empty means any. */
  agent?: string;
  /** v0.9.x — client-side filter on the run's parent_agent_id.
   *  Useful for "show me only the sub-runs spawned by agent X."
   *  The filter is applied AFTER the SSE frame is parsed, so this
   *  shrinks what your callback sees but doesn't reduce server-side
   *  load. Server-side filtering is a separate (future) request.
   *  Pass the empty string to opt out (default). */
  parentAgentId?: string;
  /** v0.9.x — opt-in observability: when true, the iterator yields a
   *  client-synthesized `{ kind: "close", payload: { reason } }` item
   *  when the stream ends (EOF, abort, or error). `reason` carries
   *  the cause ("eof" on clean close or an error class name like
   *  "AbortError" / "AuthError"). The opening `kind: "open"` frame
   *  that always appears first is server-emitted, not synthetic;
   *  `debug` has no effect on it. Default false leaves behaviour
   *  identical to v0.9.x earlier. */
  debug?: boolean;
  signal?: AbortSignal;
}

/** v0.9.x — close-event payload emitted only under
 *  {@link StreamUserRunStatesOptions.debug}. Synthetic; never on the
 *  wire. */
export interface RunStateStreamClose {
  reason: string;
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
  /** True when create was a content-addressed no-op: the active def already
   *  carried identical content, so no new version was minted. Absent on a
   *  real mint. Drives {@link LoomcycleClient.ensureCodeAgent}'s `changed`. */
  deduplicated?: boolean;
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
 *  smaller (name + description + body + tools). */
export interface SkillDefVerifyResult {
  matches: boolean;
  current_sha256: string;
  current_def_id: string;
  version: number;
  name: string;
  deployed: boolean;
}

// ---- v0.9.x MCPServerDef substrate (dynamic MCP server registration) ----

/** Response shape for `MCPServerDef set/fork/get/list` rows. Mirrors
 *  what the server-side rowResponseMap emits. `discovered_tools` is the
 *  cached tools/list snapshot — refreshed via the `rediscover` op; not
 *  part of the content_sha256 basis. */
export interface MCPServerDefRowResponse {
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
  /** Only populated on `set` / `fork` responses (auto-promoted?). */
  promoted?: boolean;
  /** True when create/rediscover was a content-addressed no-op
   *  (loomcycle ≥ v0.18.0): the active def already carried identical
   *  content (create) or identical discovered_tools (rediscover), so no
   *  new version was minted. Absent (undefined) on a real mint. */
  deduplicated?: boolean;
  /** Number of tools discovered via the upstream's `tools/list`. Present on
   *  `rediscover` responses and — since loomcycle auto-discovers at ingestion
   *  — on a fresh `create`/`fork` that ran the handshake. Absent on a
   *  deduplicated create (the active tool surface was unchanged) and when
   *  `discover:false` was passed. */
  discovered?: number;
}

/** Options for {@link LoomcycleClient.ensureMcpServer}. */
export interface EnsureMcpServerOptions {
  /** Substrate name (not a static cfg.MCPServers name). */
  name: string;
  /** Absolute MCP endpoint, e.g. `http://localhost:3000/api/mcp`. */
  url: string;
  /** Default `"http"`. */
  transport?: "http" | "streamable-http";
  /** Per-request headers, stored verbatim. Keep `${run.*}` / `${LOOMCYCLE_*}`
   *  substitution placeholders LITERAL (don't resolve a token yourself) so
   *  the registration content is stable across restarts — that's what lets
   *  loomcycle's idempotent create dedup the re-registration. */
  headers?: Record<string, string>;
  description?: string;
  /** Force a `tools/list` refresh after registering. Default false.
   *
   *  You usually do NOT need this: registration auto-discovers the upstream's
   *  tools at ingestion, so {@link EnsureMcpServerResult.discoveredToolCount}
   *  is already populated from the `create`. Set this only to force a re-read
   *  when the upstream's tool surface changed but the registration content
   *  (url/headers) did not — a plain re-register would dedup and keep the
   *  cached tools, whereas a rediscover re-runs the handshake. */
  rediscover?: boolean;
}

/** Result of {@link LoomcycleClient.ensureMcpServer}. */
export interface EnsureMcpServerResult {
  name: string;
  defId: string;
  version: number;
  /** True when this call minted a new version (create and/or rediscover);
   *  false when loomcycle deduped it (active def already current). A
   *  consumer re-registering on every boot expects `changed: false` once
   *  the registration content is stable. */
  changed: boolean;
  /** Populated when `rediscover` ran: the number of tools discovered. */
  discoveredToolCount?: number;
}

/** Typed `overlay` for an {@link LoomcycleClient.agentDef} create/fork —
 *  the mutable subset of an agent definition. All fields optional (a fork
 *  overlays only what changes). The `[extra]` tail keeps it forward-compatible
 *  with fields the in-process tool may accept that the adapter doesn't model
 *  yet — the tool owns the authoritative schema; the adapter doesn't re-validate.
 *
 *  `code_body` is the inline code-js orchestrator source (RFC J): set it (with
 *  `provider: "code-js"`) to ingest a code agent through the substrate with NO
 *  host filesystem bind — the symmetry that makes code agents work in
 *  containers / pure-cloud. Requires `LOOMCYCLE_CODE_AGENTS_ENABLED=1` on the
 *  sidecar; create/fork refuses a non-empty `code_body` otherwise. */
/** Per-agent Channel tool ACL (mirrors the sidecar `channels:` agent yaml).
 *  The Channel tool default-denies until publish/subscribe patterns are granted. */
export interface AgentChannelACL {
  publish?: string[];
  subscribe?: string[];
}

/** Per-agent Interruption tool gate (mirrors the sidecar `interruption:` agent
 *  yaml). `enabled: true` is REQUIRED for the Interruption tool to work at all. */
export interface AgentInterruptionACL {
  enabled?: boolean;
  kinds?: string[];
  max_pending?: number;
}

export interface AgentDefOverlay {
  provider?: string;
  model?: string;
  /** Inline code-js source. Empty/absent ⇒ the provider falls back to
   *  `agent_code/<name>/index.js`. Stored verbatim (whitespace is hash-
   *  significant); participates in content_sha256. */
  code_body?: string;
  tier?: string;
  effort?: string;
  max_tokens?: number;
  max_iterations?: number;
  max_concurrent_children?: number;
  system_prompt?: string;
  tools?: string[];
  skills?: string[];
  memory_scopes?: string[];
  memory_quota_bytes?: number;
  memory_backend?: string;
  retry_attempts?: number;
  /** Evaluation tool scope gate, e.g. `["submit_self", "read_any"]`. The
   *  Evaluation tool default-denies until granted. */
  evaluation_scopes?: string[];
  /** Channel tool ACL (default-deny until set). */
  channels?: AgentChannelACL;
  /** Interruption tool gate — `enabled: true` REQUIRED for the tool to work. */
  interruption?: AgentInterruptionACL;
  [extra: string]: unknown;
}

/** Options for {@link LoomcycleClient.ensureCodeAgent}. */
export interface EnsureCodeAgentOptions {
  /** Agent name (substrate; must not collide with a static cfg.Agents name —
   *  use a fork for those). */
  name: string;
  /** The inline code-js orchestrator source (the `function run(input){…}` body).
   *  Keep any `${run.*}` / `${LOOMCYCLE_*}` placeholders LITERAL so the content
   *  is stable across restarts — that's what lets loomcycle dedup the
   *  re-registration. */
  code: string;
  /** The agent's tools ceiling (must be a subset of the caller's). */
  tools?: string[];
  /** Per-user tier policy name (mutually exclusive with `model` in practice). */
  tier?: string;
  /** Pin a concrete model id (overrides tier resolution). */
  model?: string;
  description?: string;
}

/** Result of {@link LoomcycleClient.ensureCodeAgent}. */
export interface EnsureCodeAgentResult {
  name: string;
  defId: string;
  version: number;
  /** True when this call minted a new version; false when loomcycle deduped
   *  it (identical body + config already active). */
  changed: boolean;
}

/** Response shape for `MCPServerDef verify`. Same semantics as
 *  AgentDefVerifyResult / SkillDefVerifyResult — answers "is the
 *  supplied content_sha256 the deployed active version of this name?" */
export interface MCPServerDefVerifyResult {
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

// ---- v0.10.3 Library v2 — unified yaml + substrate enumeration ----
//
// Wraps GET /v1/_library/{agents,skills,mcp-servers} (shipped in v0.9.3).
// Each endpoint merges yaml-static entries with substrate-side dynamic
// entries into one envelope per row, tagged with its source. Adapter
// methods are listLibraryAgents / listLibrarySkills / listLibraryMcpServers
// — typed wrappers around the existing endpoints so external consumers
// (n8n, custom integrations) don't need to drop to raw fetch + path
// strings.
//
// The TypeScript shape mirrors web/src/api.ts:825-844 (the Web UI's
// canonical definitions) plus tightened static_definition typing per
// endpoint flavor — Web UI uses `unknown` because it renders many
// shapes through one component; external adapters get IntelliSense
// over the per-flavor structural type.

/** Static-side agent definition body. Snake_case keys mirror
 *  internal/lookup.SubstrateAgentDef so substrate-side and static-side
 *  definitions share one renderer. Forward-compatible: unknown fields
 *  on newer binaries flow through transparently. */
export interface LibraryAgentDefinition {
  provider?: string;
  model?: string;
  tier?: string;
  effort?: string;
  max_tokens?: number;
  max_iterations?: number;
  system_prompt?: string;
  system_prompt_base?: string;
  tools?: string[];
  skills?: string[];
  providers?: string[];
  /** RFC BB: per-agent web-search fallback list — the ordered providers the
   *  WebSearch tool tries (empty = the global search_priority default). */
  search_providers?: string[];
  /** Per-tier candidate list. Server-side opaque shape — kept as
   *  Record<string, unknown> for forward-compat. */
  models?: Record<string, unknown>;
  memory_scopes?: string[];
  memory_quota_bytes?: number;
}

/** Static-side skill definition body. */
export interface LibrarySkillDefinition {
  body?: string;
  description?: string;
  tools?: string[];
}

/** Static-side MCP server definition body. Mirrors
 *  internal/api/http.marshalStaticMCPServer (transport + url + headers
 *  for http/streamable-http; command/args/env/pool_size for stdio;
 *  tools narrowing; discovered_tools cached from the pool
 *  inspector when ready). */
export interface LibraryMcpServerDefinition {
  transport?: "http" | "streamable-http" | "stdio";
  url?: string;
  headers?: Record<string, string>;
  command?: string;
  args?: string[];
  env?: Record<string, string>;
  pool_size?: number;
  tools?: string[];
  /** Substrate-mirror shape of the pool's PeekTools snapshot.
   *  Omitted when the pool inspector returns nil (init pending or
   *  failed) — re-check after pool init completes. */
  discovered_tools?: unknown;
}

/** One row of the library envelope. T is the per-endpoint definition
 *  type — LibraryAgentDefinition for /v1/_library/agents, etc. */
export interface LibraryEntry<T = unknown> {
  name: string;
  source: "static-only" | "dynamic-only" | "both";
  in_static: boolean;
  in_substrate: boolean;
  version_count: number;
  active_def_id?: string;
  latest_version?: number;
  last_updated?: string;
  /** Static-side definition body. Omitted when in_static is false
   *  (dynamic-only entries have no static body — query the substrate
   *  via AgentDef/SkillDef/MCPServerDef tools to fetch the active
   *  version's payload). */
  static_definition?: T;
}

/** Envelope returned by all three GET /v1/_library/* endpoints. */
export interface LibraryListResponse<T = unknown> {
  entries: LibraryEntry<T>[];
}

// ---- v0.11.0 LLM Gateway (`POST /v1/_llm/chat`) ----
//
// Direct LLM call surface that bypasses the agent loop. Consumers
// (n8n's LoomCycleChatModel cluster sub-node first; any LangChain-
// compatible adapter in principle) hit the gateway when they only
// need provider routing + auth + retry — no tools, no memory, no
// agent semantics. The on-the-wire shape is LangChain-friendly on
// the request side (flat content strings + tool_call_id correlation)
// and Anthropic-style on the response side (content-block arrays
// with type-discriminated unions).

/** One message in the gateway conversation. Mirrors LangChain's
 *  BaseMessage shape so consumers map without re-shaping. */
export interface LLMChatMessage {
  role: "system" | "user" | "assistant" | "tool";
  /** Flat string content. For "assistant" turns with tool_calls,
   *  the content may be empty. For "tool" turns, this is the tool
   *  result text. */
  content?: string;
  /** Set on "assistant" turns that requested tool invocations. */
  tool_calls?: LLMChatToolCall[];
  /** Set on "tool" turns; correlates back to the assistant's
   *  tool_calls[].id. */
  tool_call_id?: string;
}

/** One assistant-requested tool invocation. */
export interface LLMChatToolCall {
  id: string;
  name: string;
  input: Record<string, unknown>;
}

/** Tool the model may call. The substrate translates this into the
 *  driver-native shape (Anthropic input_schema vs OpenAI function.
 *  parameters vs Gemini function_declarations) — caller passes the
 *  flat JSON schema and trusts the gateway's per-driver translation. */
export interface LLMTool {
  name: string;
  description?: string;
  input_schema: Record<string, unknown>;
}

/** Request body for llmChat / llmStream.
 *
 *  Two RFC-mentioned fields are deliberately absent in v1:
 *  - `stop_sequences`: providers.Request has no matching field today;
 *    accepting it would silently drop it. Lands when the providers
 *    package surface grows the equivalent.
 *  - `user_bearer`: the gateway calls provider.Call() directly with
 *    no MCP transport, so `${run.user_bearer}` substitution has
 *    nowhere to apply. Lands when the gateway grows an MCP path. */
export interface LLMChatOptions {
  messages: LLMChatMessage[];
  tools?: LLMTool[];
  max_tokens?: number;
  temperature?: number | null;

  /** Routing hint. When set with `model`, the resolver short-circuits
   *  to that explicit pin. When set alone, the resolver picks the
   *  best model in that provider given tier/user_tier. */
  provider?: string;
  /** Routing hint. When set with `provider`, explicit pin. When set
   *  alone, the resolver picks the provider hosting that model. */
  model?: string;
  /** Tier for resolver dispatch. Defaults to "default" when neither
   *  pin nor tier supplied. */
  tier?: string;

  /** Per-user quota tracking. Empty bypasses the per-user cap. */
  user_id?: string;
  /** Per-user tier overlay; takes precedence over `tier` when set. */
  user_tier?: string;

  /** Optional AbortSignal for caller-driven cancellation. */
  signal?: AbortSignal;
}

/** Non-streaming response shape. */
export interface LLMChatResponse {
  /** Per-response id (llm_<hex>); useful in audit logs. */
  id: string;
  /** Per-request id (req_<hex>); cross-references the audit log. */
  request_id: string;
  /** Which provider the resolver picked. */
  provider: string;
  /** Specific model id picked. */
  model: string;
  /** Content blocks; one per text or tool_use output. */
  content: LLMChatContent[];
  stop_reason: "end_turn" | "max_tokens" | "tool_use" | "stop_sequence";
  usage: LLMChatUsage;
}

/** One output content block. */
export type LLMChatContent =
  | { type: "text"; text: string }
  | { type: "tool_use"; id: string; name: string; input: Record<string, unknown> };

/** Token-accounting payload. Cache fields are populated only on
 *  providers that surface them (Anthropic today). */
export interface LLMChatUsage {
  input_tokens: number;
  output_tokens: number;
  cache_creation_input_tokens?: number;
  cache_read_input_tokens?: number;
}

/** One streaming-mode SSE frame. The `kind` field is the SSE event
 *  name; the `payload` carries the per-frame shape. v1 mirrors
 *  Anthropic's streaming event names. */
export type LLMChatStreamItem =
  | { kind: "provider_chosen"; payload: { provider: string; model: string; request_id: string } }
  | { kind: "content_block_start"; payload: { index: number; block: LLMChatContent } }
  | { kind: "content_block_delta"; payload: { index: number; delta: LLMChatStreamDelta } }
  | { kind: "content_block_stop"; payload: { index: number } }
  | { kind: "message_delta"; payload: { delta: { stop_reason?: string }; usage: LLMChatUsage } }
  | { kind: "done"; payload: { id: string; stop_reason: string; usage: LLMChatUsage } }
  | { kind: "error"; payload: { type: string; code: string; message: string } };

export interface LLMChatStreamDelta {
  type: "text_delta" | "input_json_delta";
  text?: string;          // for text_delta
  partial_json?: string;  // for input_json_delta
}

// ---- v0.11.4 OpenAI Embeddings compatibility shim ----
//
// Wraps POST /v1/embeddings. Dispatches to the single configured
// `providers.Embedder` (the same instance Memory tool uses for
// embed:true). No resolver path, no tier overlay, no streaming.
//
// Consumers using @loomcycle/client get richer typing here than
// pointing the raw OpenAI SDK at /v1/embeddings — the response
// shape is properly typed per encoding_format.

/** Request body for `embeddings()`. */
export interface LLMEmbeddingsOptions {
  /** Consumer's requested model id (echoed in the response).
   *  Loomcycle uses the single configured embedder regardless;
   *  the field is informational for drop-in compatibility. */
  model: string;

  /** Text(s) to embed. v1 accepts string or string[]; tokenized
   *  inputs (number arrays) are refused — send text. */
  input: string | string[];

  /** "float" (default) emits each vector as a JSON array of
   *  numbers; "base64" packs each float32 little-endian then
   *  base64-encodes (saves ~25% wire bytes on 1536-dim vectors). */
  encoding_format?: "float" | "base64";

  /** OpenAI's post-hoc dimension reduction parameter. Accepted-
   *  but-ignored in v0.11.4 (the providers.Embedder interface
   *  doesn't take a dimension parameter today). */
  dimensions?: number;

  /** OpenAI-standard opaque end-user identifier. Maps to
   *  loomcycle's per-user quota tracking + audit log. */
  user?: string;

  /** Optional AbortSignal for caller-driven cancellation. */
  signal?: AbortSignal;
}

/** Per-embedding entry in the response data array. */
export interface LLMEmbeddingItem {
  object: "embedding";
  /** float[] when the request used encoding_format:"float"
   *  (default); base64 string when encoding_format:"base64". */
  embedding: number[] | string;
  index: number;
}

/** Token-accounting payload. v0.11.4 leaves both fields at 0 — the
 *  substrate's Embedder interface doesn't return per-call token
 *  counts today. When that lands, the shim's translator populates
 *  these automatically. */
export interface LLMEmbeddingsUsage {
  prompt_tokens: number;
  total_tokens: number;
}

/** Response shape mirrors OpenAI's /v1/embeddings exactly. */
export interface LLMEmbeddingsResponse {
  object: "list";
  data: LLMEmbeddingItem[];
  model: string;
  usage: LLMEmbeddingsUsage;
}

// --- RFC AV: token-usage & cost report (GET /v1/_usage) ---

/** A whitelisted grouping dimension for the usage report. */
export type UsageDimension =
  | "tenant"
  | "user"
  | "provider"
  | "model"
  | "source";

/** One grouped row of a usage report; only the grouped dimensions are set. */
export interface UsageAggregate {
  tenant_id?: string;
  user_id?: string;
  provider?: string;
  model?: string;
  /** operator | tenant | user */
  credential_source?: string;
  input_tokens: number;
  output_tokens: number;
  cache_creation_tokens: number;
  cache_read_tokens: number;
  cost: number;
  currency?: string;
  call_count: number;
  unpriced_calls: number;
}

export interface UsageReportResponse {
  group_by: string[];
  from?: string;
  to?: string;
  rows: UsageAggregate[];
}

/** A per-scope token budget (RFC AW) plus its live month-to-date usage.
 *  `soft_limit` / `hard_limit` are absent when that tier is unset (no ceiling
 *  on that axis). Mirrors one row of GET /v1/_limits. */
export interface TokenLimit {
  tenant_id: string;
  /** "operator" | "tenant" | "user" */
  scope: string;
  /** tenant id (scope=tenant), user subject (scope=user), "" (operator). */
  scope_id?: string;
  soft_limit?: number;
  hard_limit?: number;
  /** The scope's current month-to-date token total. */
  used: number;
  updated_at?: string;
  updated_by?: string;
}

export interface TokenLimitsResponse {
  limits: TokenLimit[];
}

/** The PUT /v1/_limits body (RFC AW). A present `soft_limit`/`hard_limit` sets
 *  that tier; omitting it clears the tier (unlimited on that axis) — a full-row
 *  upsert. `tenant_id` is an admin-only target; a tenant operator is confined to
 *  its own tenant regardless of this field. */
export interface SetTokenLimitRequest {
  /** Admin-only target tenant; ignored for confinement on a scoped caller. */
  tenant_id?: string;
  /** "operator" | "tenant" | "user" */
  scope: string;
  /** Required for scope=user (the subject); must be empty for scope=tenant. */
  scope_id?: string;
  soft_limit?: number;
  hard_limit?: number;
}
