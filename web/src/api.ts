// Thin client over loomcycle's /v1/* surface. Auth is via the
// `loomcycle_session` HttpOnly cookie set by the server when the
// operator visits `/ui?token=...` — fetch() includes it
// automatically because we're same-origin. No bearer header needed.

export interface UserSummary {
  user_id: string;
  running_count: number;
  total_count: number;
  last_started_at: string;
}

export interface ListUsersResponse {
  users: UserSummary[];
}

// tenant is the super-admin tenant-focus (?tenant=); a tenant principal's
// own tenant is enforced server-side regardless, so this is admin-only in
// practice and harmless when set by a tenant (ignored by the backend).
export function listUsers(tenant?: string): Promise<ListUsersResponse> {
  const q = tenant ? `?tenant=${encodeURIComponent(tenant)}` : "";
  return jsonFetch<ListUsersResponse>(`/v1/_users${q}`);
}

// HealthResponse mirrors handleHealthz on the server. v0.8.21 added
// build identifiers (version/commit/built/uptime_seconds) so the UI
// topbar can show the real running version, plus metrics_enabled so
// the Activity Monitor can render its "metrics off" empty state on
// mount without probing /v1/_metrics and getting 503. Pre-v0.8.21
// servers return just {"ok":true}; the new fields are undefined on
// those responses, which the UI treats accordingly.
export interface HealthResponse {
  ok: boolean;
  version?: string;
  commit?: string;
  built?: string;
  uptime_seconds?: number;
  metrics_enabled?: boolean;
}

export function getHealth(): Promise<HealthResponse> {
  // /healthz isn't under /v1/ — it's unauthenticated and lives at
  // the root mux per the server's pattern.
  return jsonFetch<HealthResponse>("/healthz");
}

// Agent mirrors agentResponse on the server (internal/api/http/server.go).
// Field names use snake_case to match the wire shape; renaming any of
// them is a wire change, not a UI change.
export interface Agent {
  agent_id: string;
  run_id: string;
  session_id: string;
  // agent is the YAML-declared agent name ("qa-agent",
  // "company-researcher") — what the operator wants to see in lists.
  // Empty when loomcycle's parser didn't see a session row (legacy
  // rows pre-dating the agent column on the JOIN).
  agent: string;
  parent_agent_id: string | null;
  user_id: string;
  status: "running" | "completed" | "failed" | "cancelled";
  started_at: string;
  completed_at: string | null;
  stop_reason: string | null;
  error: string | null;
  usage: {
    input_tokens?: number;
    output_tokens?: number;
    cache_creation_tokens?: number;
    cache_read_tokens?: number;
    model?: string;
  };
  last_heartbeat_at: string | null;
  live: boolean;
  // interactive marks a persistent interactive run (parks at end_turn for
  // operator steering). Drives the run page's interactive-session switcher
  // and the runs-page "interactive" tag. Absent/false for ordinary runs.
  interactive?: boolean;
  // v0.8.21 awaited-state surface. Empty/absent for non-running
  // runs AND for running runs making normal progress. When set,
  // `awaited_state` is "channel" (open Channel.subscribe) or
  // "interrupted" (open Interruption.ask), and `awaited_on` carries
  // the channel name or interruption kind respectively.
  awaited_state?: "channel" | "interrupted" | "";
  awaited_on?: string;
  // v0.12.x cluster mode: the replica owning this run's live cancel
  // handle. Absent in single-replica deployments (the server omits
  // the field when its replica_id is unset).
  replica_id?: string;
}

export interface ListAgentsResponse {
  agents: Agent[];
}

// LimitEventInfo mirrors providers.LimitInfo — the sidecar on a `limit`
// event (RFC AW per-scope token budgets). The event is SERVER-generated
// (not from a provider): emitted once per (scope, severity) per run when a
// scope crosses its soft/hard ceiling. Present identically on the live SSE
// `limit` frame and the persisted `limit` transcript row, so the terminal
// renders both through the same path. Non-secret (ids + integer counts).
export interface LimitEventInfo {
  scope: string; // operator | tenant | user
  scope_id?: string; // tenant id / user subject; "" for operator-global
  severity: "soft" | "hard" | string; // soft = warn, hard = next run refused
  window: string; // "month" (calendar month, UTC) in Phase 1
  used: number; // month-to-date token total at the crossing
  limit: number; // the tier crossed (the soft or hard ceiling)
  message?: string;
}

// EventPayload mirrors providers.Event on the server side. Carried
// inside `TranscriptEvent.event` (the wire shape from
// /v1/sessions/{id}/transcript wraps each row with seq/run_id/ts_ns
// plus the embedded event).
export interface EventPayload {
  type: string;
  text?: string;
  tool_use?: { id: string; name: string; input: unknown };
  tool_use_id?: string;
  is_error?: boolean;
  stop_reason?: string;
  reasoning?: string;
  usage?: {
    input_tokens?: number;
    output_tokens?: number;
    cache_read_input_tokens?: number;
    cache_creation_input_tokens?: number;
    model?: string;
    // Serving model's context-window ceiling (loop stamps it from
    // Provider.Capabilities()). 0/absent = unknown (e.g. Ollama). Used
    // by the live terminal to render a "context used / max" gauge.
    max_context_tokens?: number;
  };
  error?: string;
  retry?: { provider?: string; attempt?: number; wait_ms?: number; reason?: string };
  // interruption_pending sidecar (providers.InterruptionEventInfo): the agent
  // raised a question via the Interruption tool and is now blocked. The run's
  // SSE stream stays open and resumes once the interrupt is resolved (POST
  // /v1/runs/{run_id}/interrupts/{interrupt_id}/resolve). `options`, when
  // present, is the fixed answer set the resolve must match.
  interruption?: {
    interrupt_id: string;
    kind?: string;
    question?: string;
    options?: string[];
    context?: string;
    priority?: string;
    expires_at?: string;
  };
  // "steer" sidecar — an operator-injected steering message drained mid-turn.
  user_input?: { text: string; source?: string; seen_at?: string };
  // "awaiting_input" sidecar — a persistent interactive run parked at end_turn.
  awaiting_input?: { since_turn?: number };
  // "context_compaction" sidecar — the conversation before this marker was
  // summarized to free context (interactive compaction).
  context_compaction?: { summary?: string; before_tokens?: number; after_tokens?: number };
  // "limit" sidecar (RFC AW) — a per-scope token-budget crossing. Present on
  // both the live SSE `limit` frame and the persisted `limit` transcript row.
  limit?: LimitEventInfo;
}

// v0.9.x — payloads for event types whose shape doesn't fit
// providers.Event (and is carried via the `payload` sidecar on
// TranscriptEvent rather than `event`).

/** UserInputPayload mirrors the JSON of `[]loop.PromptSegment` —
 *  what the caller supplied as `segments` on POST /v1/runs +
 *  /v1/sessions/{id}/messages. Surfaced as the run's "user input"
 *  card in the Web UI. */
export interface UserInputPayload {
  role: string; // "system" | "user"
  content: Array<{ type: string; text?: string; cacheable?: boolean }>;
}

/** SystemPromptPayload mirrors the v0.9.x system_prompt event
 *  payload — the resolved system prompt + provenance metadata so
 *  operators can see WHICH AgentDef + WHICH SkillDef rows fed in. */
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

// TranscriptEvent is one persisted row from
// /v1/sessions/{id}/transcript. The server wraps each providers.Event
// in {seq, run_id, ts_ns, type, event:{...}} — the type field at the
// top level mirrors event.type so consumers can filter without
// digging into the payload, but the actual content lives under
// `event`.
//
// v0.9.x: for event types whose payload doesn't fit providers.Event
// (`user_input`, `system_prompt`), the server surfaces the raw JSON
// via the optional `payload` field. Streaming event types
// (text/tool_call/tool_result/done/...) leave `payload` undefined.
export interface TranscriptEvent {
  seq: number;
  run_id: string;
  ts_ns: number;
  type: string;
  event: EventPayload;
  payload?: UserInputPayload[] | SystemPromptPayload | unknown;
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

const baseURL = ""; // same-origin

// LOGIN_PATH is the SPA login route (router basename is /ui). An
// unauthenticated /v1/* call (401 — missing/expired/invalid bearer
// cookie) bounces here. A 403 (authenticated but insufficient scope) does
// NOT redirect — you're logged in, just not permitted; callers surface it.
const LOGIN_PATH = "/ui/login";

// redirectToLoginOn401 sends the browser to the login page on a 401,
// unless already there (no redirect loop). Returns true when it
// triggered a navigation so callers can stop processing.
function redirectToLoginOn401(status: number): boolean {
  if (status === 401 && window.location.pathname !== LOGIN_PATH) {
    window.location.href = LOGIN_PATH;
    return true;
  }
  return false;
}

async function jsonFetch<T>(path: string, init?: RequestInit): Promise<T> {
  const resp = await fetch(baseURL + path, {
    credentials: "same-origin",
    ...init,
    headers: {
      Accept: "application/json",
      ...(init?.headers ?? {}),
    },
  });
  if (!resp.ok) {
    if (redirectToLoginOn401(resp.status)) {
      // Navigation underway — return a never-settling promise so the
      // caller doesn't flash an error mid-redirect.
      return new Promise<T>(() => {});
    }
    const body = await resp.text();
    throw new Error(`${resp.status} ${resp.statusText}: ${body.slice(0, 200)}`);
  }
  return resp.json() as Promise<T>;
}

// Principal is the authenticated identity behind the session cookie,
// resolved by GET /v1/_me. Drives the UI's role: super-admin (is_admin)
// sees all tenants' workspaces; a tenant sees only its own.
export interface Principal {
  tenant_id: string;
  subject: string;
  scopes: string[];
  is_admin: boolean;
  legacy: boolean;
  open_mode?: boolean;
  /**
   * Non-secret, booleans-only runtime posture (RFC AU) the UI uses to gate
   * affordances — notably whether a tenant may import a stdio MCP server (host
   * RCE, off by default). Absent on older servers → treat every capability as
   * false (safe default). NEVER carries allowlist contents or any secret.
   */
  capabilities?: ServerCapabilities;
}

export interface ServerCapabilities {
  /** LOOMCYCLE_MCP_ALLOW_DYNAMIC_STDIO — gates the stdio MCP import path. */
  mcp_allow_dynamic_stdio?: boolean;
  /** Whether any http host allowlist is configured (presence only). */
  http_host_allowlist_configured?: boolean;
}

// getWhoami resolves the current principal. A 401 redirects to /login via
// jsonFetch (the returned promise never settles), so callers only ever
// see a resolved Principal or a non-auth error.
export function getWhoami(): Promise<Principal> {
  return jsonFetch<Principal>("/v1/_me");
}

// jsonFetchAllowing is a sibling of jsonFetch that does NOT throw on
// the status codes in `allow` — instead it returns a discriminated
// {ok:false, status, body} so the caller can branch. Used by the
// metrics helpers: the /v1/_metrics/* endpoints return 503 when the
// server-side sampler is off (LOOMCYCLE_METRICS_ENABLED unset), and
// the Activity Monitor wants to render an instructional empty
// state instead of surfacing the 503 as a thrown error.
async function jsonFetchAllowing<T>(
  path: string,
  allow: number[],
  init?: RequestInit,
): Promise<{ ok: true; data: T } | { ok: false; status: number; body: any }> {
  const resp = await fetch(baseURL + path, {
    credentials: "same-origin",
    ...init,
    headers: {
      Accept: "application/json",
      ...(init?.headers ?? {}),
    },
  });
  if (resp.ok) {
    return { ok: true, data: (await resp.json()) as T };
  }
  if (allow.includes(resp.status)) {
    // Best-effort JSON decode; the server's 503 carries an
    // enable_hint string in the body for the metrics path.
    let body: any = null;
    try {
      body = await resp.json();
    } catch {
      // non-JSON body — leave as null
    }
    return { ok: false, status: resp.status, body };
  }
  const text = await resp.text();
  throw new Error(`${resp.status} ${resp.statusText}: ${text.slice(0, 200)}`);
}

export function listAgents(userId: string, status?: string, tenant?: string): Promise<ListAgentsResponse> {
  const params = new URLSearchParams();
  if (status) params.set("status", status);
  // Super-admin tenant-focus; ignored server-side for tenant principals.
  if (tenant) params.set("tenant", tenant);
  const q = params.toString() ? `?${params.toString()}` : "";
  return jsonFetch<ListAgentsResponse>(`/v1/users/${encodeURIComponent(userId)}/agents${q}`);
}

export function getAgent(agentId: string): Promise<Agent> {
  return jsonFetch<Agent>(`/v1/agents/${encodeURIComponent(agentId)}`);
}

export function getTranscript(sessionId: string): Promise<TranscriptResponse> {
  return jsonFetch<TranscriptResponse>(`/v1/sessions/${encodeURIComponent(sessionId)}/transcript`);
}

// v0.8.0 Memory admin types — mirrors internal/api/http/memory.go.

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

// MemoryEntry mirrors store.MemoryEntry. value is a parsed JSON value
// (the server stored it as JSONB on Postgres, TEXT-as-JSON on SQLite,
// and re-parsed it on the way out — `unknown` here so the UI can
// pretty-print whatever shape the agent wrote).
export interface MemoryEntry {
  key: string;
  value: unknown;
  expires_at?: string;
  created_at: string;
  updated_at: string;
}

// MemoryEmbeddingMeta is the per-key embedding descriptor returned when
// the keys listing is requested with include_embedding_metadata=true
// (RFC I MR-6). Keys without an embedding are simply absent from the
// embedding_metadata map.
export interface MemoryEmbeddingMeta {
  provider: string;
  model: string;
  dimension: number;
}

export interface MemoryEntriesResponse {
  scope: string;
  scope_id: string;
  entries: MemoryEntry[];
  truncated: boolean;
  // Present only when the caller asked for include_embedding_metadata.
  // Older servers / non-vector stores omit it entirely.
  embedding_metadata?: Record<string, MemoryEmbeddingMeta>;
}

export interface MemoryEntryResponse {
  scope: string;
  scope_id: string;
  entry: MemoryEntry;
}

export function listMemoryScopes(): Promise<MemoryScopesResponse> {
  return jsonFetch<MemoryScopesResponse>("/v1/_memory/scopes");
}

export function listMemoryScopeIDs(scope: string): Promise<MemoryScopeIDsResponse> {
  return jsonFetch<MemoryScopeIDsResponse>(`/v1/_memory/scopes/${encodeURIComponent(scope)}`);
}

export function listMemoryEntries(
  scope: string,
  scopeID: string,
  prefix?: string,
  limit?: number,
  includeEmbeddingMetadata?: boolean,
): Promise<MemoryEntriesResponse> {
  const params = new URLSearchParams();
  if (prefix) params.set("prefix", prefix);
  if (limit) params.set("limit", String(limit));
  if (includeEmbeddingMetadata) params.set("include_embedding_metadata", "true");
  const qs = params.toString();
  return jsonFetch<MemoryEntriesResponse>(
    `/v1/_memory/scopes/${encodeURIComponent(scope)}/${encodeURIComponent(scopeID)}/keys${qs ? "?" + qs : ""}`,
  );
}

export function getMemoryEntry(
  scope: string,
  scopeID: string,
  key: string,
): Promise<MemoryEntryResponse> {
  return jsonFetch<MemoryEntryResponse>(
    `/v1/_memory/scopes/${encodeURIComponent(scope)}/${encodeURIComponent(scopeID)}/keys/${encodeURIComponent(key)}`,
  );
}

// ---- v0.9.0 Vector Memory admin ----
//
// Both endpoints return 503 when the backend lacks vector support
// (LOOMCYCLE_PGVECTOR_ENABLED unset on Postgres / SQLite always) or
// when no embedder is configured. The MemoryView uses
// jsonFetchAllowing(..., [503]) so the UI can render an
// instructional empty state instead of a thrown error banner.

export interface MemoryEmbedModelStats {
  provider: string;
  model: string;
  dimension: number;
  row_count: number;
}

export interface MemoryEmbedStatsResponse {
  scope: string;
  models: MemoryEmbedModelStats[];
  total_embedding_bytes: number;
}

export type MemoryEmbedStatsResult =
  | { ok: true; data: MemoryEmbedStatsResponse }
  | { ok: false; status: number; body: any };

export function listMemoryEmbedStats(scope: string): Promise<MemoryEmbedStatsResult> {
  return jsonFetchAllowing<MemoryEmbedStatsResponse>(
    `/v1/_memory/embed_stats?scope=${encodeURIComponent(scope)}`,
    [503],
  );
}

export interface MemoryReembedCurrentEmbedder {
  provider: string;
  model: string;
  dimension: number;
}

export interface MemoryReembedDryRunResponse {
  scope: string;
  scope_id: string;
  dry_run: true;
  rows_total: number;
  rows_to_reembed: number;
  current_embedder: MemoryReembedCurrentEmbedder;
  sample_keys: string[];
  sample_keys_capped: boolean;
}

export interface MemoryReembedRealResponse {
  scope: string;
  scope_id: string;
  dry_run: false;
  rows_reembedded: number;
  rows_failed: number;
  current_embedder: MemoryReembedCurrentEmbedder;
  failed_keys?: string[];
}

export type MemoryReembedResponse = MemoryReembedDryRunResponse | MemoryReembedRealResponse;

export function reembedMemory(
  scope: string,
  scopeID: string,
  dryRun: boolean,
): Promise<MemoryReembedResponse> {
  const params = new URLSearchParams({
    scope,
    scope_id: scopeID,
    dry_run: String(dryRun),
  });
  return jsonFetch<MemoryReembedResponse>(
    `/v1/_memory/reembed?${params.toString()}`,
    { method: "POST" },
  );
}

export async function cancelAgent(agentId: string, reason?: string): Promise<unknown> {
  const resp = await fetch(`/v1/agents/${encodeURIComponent(agentId)}/cancel`, {
    method: "POST",
    credentials: "same-origin",
    headers: { "Content-Type": "application/json", Accept: "application/json" },
    body: JSON.stringify({ reason: reason ?? "" }),
  });
  if (!resp.ok) {
    const body = await resp.text();
    throw new Error(`${resp.status} ${resp.statusText}: ${body.slice(0, 200)}`);
  }
  return resp.json();
}

// ---- Interruption (v0.8.16) ----------------------------------------

export interface InterruptRow {
  interrupt_id: string;
  run_id: string;
  kind: string;
  status: string;
  question?: string;
  options?: string[]; // server stores as JSON array
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

export function listUserInterrupts(
  userID: string,
  status: string = "pending",
): Promise<InterruptListResponse> {
  return jsonFetch<InterruptListResponse>(
    `/v1/users/${encodeURIComponent(userID)}/interrupts?status=${encodeURIComponent(status)}`,
  );
}

export function listRunInterrupts(
  runID: string,
  status: string = "pending",
): Promise<InterruptListResponse> {
  return jsonFetch<InterruptListResponse>(
    `/v1/runs/${encodeURIComponent(runID)}/interrupts?status=${encodeURIComponent(status)}`,
  );
}

// v0.8.17 — runtime pause / resume / state + snapshot admin.

export interface RuntimeStateResponse {
  state: "running" | "pausing" | "paused";
  paused_runs_count: number;
}

export interface PauseResult {
  state: string;
  duration_ms: number;
  force_cancelled_count: number;
  paused_runs_count: number;
  warnings?: string[];
}

export interface ResumeResult {
  state: string;
  resumed_runs_count: number;
  warnings?: string[];
}

export function getRuntimeState(): Promise<RuntimeStateResponse> {
  return jsonFetch<RuntimeStateResponse>("/v1/_state");
}

export async function pauseRuntime(timeoutMs?: number): Promise<PauseResult> {
  const body = timeoutMs && timeoutMs > 0 ? JSON.stringify({ timeout_ms: timeoutMs }) : undefined;
  return postJSON<PauseResult>("/v1/_pause", body);
}

export async function resumeRuntime(): Promise<ResumeResult> {
  return postJSON<ResumeResult>("/v1/_resume", undefined);
}

export interface SnapshotListEntry {
  id: string;
  created_at: string;
  label?: string;
  schema_version: number;
  byte_size: number;
}

export interface SnapshotListResponse {
  entries: SnapshotListEntry[];
}

export interface SnapshotCreateResponse {
  id: string;
  created_at: string;
  label?: string;
  schema_version: number;
  byte_size: number;
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

export function listSnapshots(limit = 200, labelContains = ""): Promise<SnapshotListResponse> {
  const q = new URLSearchParams();
  if (limit > 0) q.set("limit", String(limit));
  if (labelContains) q.set("label_contains", labelContains);
  const path = q.toString() ? `/v1/_snapshots?${q}` : "/v1/_snapshots";
  return jsonFetch<SnapshotListResponse>(path);
}

export async function createSnapshot(label?: string): Promise<SnapshotCreateResponse> {
  const body = label ? JSON.stringify({ label }) : undefined;
  return postJSON<SnapshotCreateResponse>("/v1/_snapshots", body);
}

export async function deleteSnapshot(id: string): Promise<void> {
  const resp = await fetch(baseURL + `/v1/_snapshots/${encodeURIComponent(id)}`, {
    method: "DELETE",
    credentials: "same-origin",
  });
  if (!resp.ok) {
    const body = await resp.text();
    throw new Error(`${resp.status} ${resp.statusText}: ${body.slice(0, 200)}`);
  }
}

// exportSnapshotURL returns the URL the browser can navigate to (or
// the user can right-click + "save link as…") to download the
// envelope. The server sets Content-Disposition so the browser names
// the file using the snapshot id.
export function exportSnapshotURL(id: string): string {
  return baseURL + `/v1/_snapshots/${encodeURIComponent(id)}/export`;
}

export async function restoreSnapshotFromText(
  envelopeJSON: string,
  includeHistory = false,
): Promise<SnapshotRestoreResponse> {
  let parsed: unknown;
  try {
    parsed = JSON.parse(envelopeJSON);
  } catch (e) {
    throw new Error(`envelope is not valid JSON: ${e instanceof Error ? e.message : String(e)}`);
  }
  return postJSON<SnapshotRestoreResponse>(
    "/v1/_snapshots/inline/restore",
    JSON.stringify({ include_history: includeHistory, json: parsed }),
  );
}

async function postJSON<T>(path: string, body: string | undefined): Promise<T> {
  const resp = await fetch(baseURL + path, {
    method: "POST",
    credentials: "same-origin",
    headers: body
      ? { "Content-Type": "application/json", Accept: "application/json" }
      : { Accept: "application/json" },
    body,
  });
  if (!resp.ok) {
    const raw = await resp.text();
    throw new Error(`${resp.status} ${resp.statusText}: ${raw.slice(0, 200)}`);
  }
  return resp.json() as Promise<T>;
}

// ───────────────────────────────────────────────────────────────────────────
// RFC AM — Path + Document console (off-run substrate endpoints).
//
// The Web UI drives the Path VFS (RFC AL) and chunked-graph Documents (RFC AK)
// through the off-run endpoints POST /v1/_path and POST /v1/_document. Scope +
// tenant are resolved server-side from the authenticated principal
// (substrateAdminUserCtx) — the browser never sends them as authority; `scope`
// is only the subtree SELECTOR (which of the principal's own trees). The
// console defaults to `user` scope so its tree lines up with the principal's
// own agent runs (user_id = principal.subject). `agent` scope is operator-
// private off-run; Documents support agent|user only (tenant is refused).

export type PathScope = "agent" | "user" | "tenant";
export type DocScope = "agent" | "user";

// BrowseScope is an optional override for the off-run Path/Document endpoints
// (RFC AS): which subject's tree (scopeId → ?scope_id=) and, for an admin, which
// tenant (tenant → ?tenant=). Both are CALLER-AUTHORITATIVE only in the sense
// that the SERVER re-checks them — a substrate:tenant principal's ?tenant= is
// ignored (confined to its own tenant); scope_id picks any subject it may see.
// Unset fields are omitted, so the server falls back to the caller's own
// subject (byte-identical to the pre-RFC-AS behaviour).
export interface BrowseScope {
  scopeId?: string;
  tenant?: string;
}

// PathEntry is one dirent in an `ls` listing.
export interface PathEntry {
  name: string;
  kind: string; // directory | document | volume_mount | memory_entry
  full_path: string;
  resource_ref?: unknown;
}

// substratePost POSTs a tool op to an off-run substrate endpoint. A 200 returns
// the tool's JSON result verbatim; a 422 tool refusal carries {code,error,tool}
// — surface the human-readable `error` (e.g. "Documents require SQL Memory").
async function substratePost<T>(path: string, body: unknown, browse?: BrowseScope): Promise<T> {
  let url = baseURL + path;
  if (browse && (browse.scopeId || browse.tenant)) {
    const q = new URLSearchParams();
    if (browse.scopeId) q.set("scope_id", browse.scopeId);
    if (browse.tenant) q.set("tenant", browse.tenant);
    url += `?${q.toString()}`;
  }
  const resp = await fetch(url, {
    method: "POST",
    credentials: "same-origin",
    headers: { "Content-Type": "application/json", Accept: "application/json" },
    body: JSON.stringify(body),
  });
  if (resp.ok) return resp.json() as Promise<T>;
  if (redirectToLoginOn401(resp.status)) return new Promise<T>(() => {});
  const raw = await resp.text();
  let msg = `${resp.status} ${resp.statusText}: ${raw.slice(0, 200)}`;
  try {
    const j = JSON.parse(raw) as { error?: string };
    if (j && typeof j.error === "string") msg = j.error;
  } catch {
    /* non-JSON body — keep the status-line message */
  }
  throw new Error(msg);
}

// pathLs lists a path. recursive returns the whole subtree (the tree view
// fetches the subtree once and reconstructs intermediate dirs client-side).
export function pathLs(
  path: string,
  scope: PathScope,
  recursive = false,
  browse?: BrowseScope,
): Promise<{ path: string; entries: PathEntry[] }> {
  return substratePost("/v1/_path", { op: "ls", path, scope, recursive }, browse);
}

// pathMkdir materializes an empty `directory` dirent (RFC AM); idempotent.
export function pathMkdir(
  path: string,
  scope: PathScope,
  browse?: BrowseScope,
): Promise<{ ok: boolean; created: boolean; path: string }> {
  return substratePost("/v1/_path", { op: "mkdir", path, scope }, browse);
}

// pathMv renames/relocates a dirent (atomic; cascades a subtree; no-clobber).
export function pathMv(
  from: string,
  to: string,
  scope: PathScope,
  browse?: BrowseScope,
): Promise<{ ok: boolean }> {
  return substratePost("/v1/_path", { op: "mv", path: from, to, scope }, browse);
}

// pathRm removes a dirent (recursive required for a non-empty branch). Removes
// the path entry only — backing resources are deleted via their own tool.
export function pathRm(
  path: string,
  scope: PathScope,
  recursive = false,
  browse?: BrowseScope,
): Promise<{ ok: boolean; n_removed: number }> {
  return substratePost("/v1/_path", { op: "rm", path, scope, recursive }, browse);
}

// documentCreate makes a new chunked-graph Document and names it in the Path
// tree at `path`. Requires SQL Memory on the runtime; a not-enabled runtime
// returns a tool refusal surfaced by substratePost.
export function documentCreate(
  title: string,
  path: string,
  scope: DocScope,
  browse?: BrowseScope,
): Promise<{ document_id: string; root_chunk_id: string; title: string; path?: string }> {
  return substratePost("/v1/_document", { op: "create_document", title, path, scope }, browse);
}

// documentDelete removes a Document by id: SQL rows + Memory bodies + the Path
// dirent, cascading its chunks (RFC AK).
export function documentDelete(
  id: string,
  scope: DocScope,
  browse?: BrowseScope,
): Promise<{ deleted: boolean; document_id: string; n_chunks_deleted: number }> {
  return substratePost("/v1/_document", { op: "delete_document", id, scope }, browse);
}

// ChunkRow is the structural record returned by query_chunks (no body — kept
// light; fetch the body with documentGetChunk).
export interface ChunkRow {
  id: string;
  document_id: string;
  parent_id?: string;
  position: number;
  title: string;
  type?: string;
  status?: string;
  revision: number;
}
// ChunkDetail adds the Markdown body + typed fields (from get_chunk).
export interface ChunkDetail extends ChunkRow {
  body: string;
  fields?: unknown;
}

// documentQueryChunks lists a document's chunks (structure only). limit caps at
// 1000 server-side — adequate for the viewer's per-document trees.
export function documentQueryChunks(
  documentId: string,
  scope: DocScope,
  browse?: BrowseScope,
): Promise<{ chunks: ChunkRow[] }> {
  return substratePost(
    "/v1/_document",
    {
      op: "query_chunks",
      document_id: documentId,
      scope,
      limit: 1000,
    },
    browse,
  );
}

export function documentGetChunk(id: string, scope: DocScope, browse?: BrowseScope): Promise<ChunkDetail> {
  return substratePost("/v1/_document", { op: "get_chunk", id, scope }, browse);
}

// documentUpdateChunk applies an optimistic-concurrency update — pass the
// chunk's current `revision`; a stale revision rejects with a "revision
// conflict" error (surfaced by substratePost) the editor reloads on.
export function documentUpdateChunk(
  id: string,
  revision: number,
  patch: { body?: string; fields?: unknown; title?: string; status?: string; type?: string },
  scope: DocScope,
  browse?: BrowseScope,
): Promise<ChunkDetail> {
  return substratePost("/v1/_document", { op: "update_chunk", id, revision, scope, ...patch }, browse);
}

// documentExportMd renders the document to Markdown. includeMetadata=true is
// round-trippable (HTML-comment metadata + edges); false is clean human MD.
export function documentExportMd(
  documentId: string,
  scope: DocScope,
  includeMetadata: boolean,
  browse?: BrowseScope,
): Promise<{ markdown: string; title: string; document_id: string }> {
  return substratePost(
    "/v1/_document",
    {
      op: "export_md",
      document_id: documentId,
      scope,
      include_metadata: includeMetadata,
    },
    browse,
  );
}

// AuditEvent mirrors wireEvent in internal/api/http/events_audit.go.
// Payload is left as `unknown` because event payloads vary by type
// (text / tool_call / tool_result / usage / done / …); the audit
// view stringifies + pretty-prints them rather than decoding.
export interface AuditEvent {
  seq: number;
  session_id: string;
  run_id: string;
  timestamp: string;
  type: string;
  payload: unknown;
}

export interface ListEventsResponse {
  events: AuditEvent[];
  total: number;
  limit: number;
  offset: number;
}

export interface ListEventsParams {
  type?: string;
  from?: string;
  to?: string;
  limit?: number;
  offset?: number;
}

export function listEvents(params: ListEventsParams = {}): Promise<ListEventsResponse> {
  const q = new URLSearchParams();
  if (params.type) q.set("type", params.type);
  if (params.from) q.set("from", params.from);
  if (params.to) q.set("to", params.to);
  if (params.limit !== undefined) q.set("limit", String(params.limit));
  if (params.offset !== undefined) q.set("offset", String(params.offset));
  const qs = q.toString();
  return jsonFetch<ListEventsResponse>(`/v1/_events${qs ? "?" + qs : ""}`);
}

export async function resolveInterrupt(
  runID: string,
  interruptID: string,
  answer: string,
  resolvedBy: string = "webui",
): Promise<unknown> {
  const resp = await fetch(
    `/v1/runs/${encodeURIComponent(runID)}/interrupts/${encodeURIComponent(interruptID)}/resolve`,
    {
      method: "POST",
      credentials: "same-origin",
      headers: { "Content-Type": "application/json", Accept: "application/json" },
      body: JSON.stringify({ kind: "question", answer, resolved_by: resolvedBy }),
    },
  );
  if (!resp.ok) {
    const body = await resp.text();
    throw new Error(`${resp.status} ${resp.statusText}: ${body.slice(0, 200)}`);
  }
  return resp.json();
}

// --- v0.8.21 Process-resource metrics ---
//
// ProcessSample mirrors the server's ProcessSample row. Optional
// system_* fields are populated only when LOOMCYCLE_METRICS_COLLECT_SYSTEM=1
// — null/undefined on installs where the operator didn't opt in.
// The UI detects system-availability by inspecting the first
// non-null row.
export interface ProcessSample {
  sampled_at: string;
  active_runs: number;
  queued_runs: number;
  loomcycle_rss_bytes: number;
  loomcycle_heap_alloc_bytes: number;
  loomcycle_heap_inuse_bytes: number;
  loomcycle_num_goroutines: number;
  loomcycle_cpu_pct_x100: number;
  system_cpu_pct_x100?: number | null;
  system_mem_used_mb?: number | null;
  system_mem_available_mb?: number | null;
}

export interface SamplesResponse {
  samples: ProcessSample[];
  next_cursor?: string;
}

export interface SummaryBucket {
  at: string;
  mean_rss_bytes: number;
  max_rss_bytes: number;
  p95_cpu_pct_x100: number;
  active_runs_max: number;
  sample_count: number;
}

export interface SummaryResponse {
  period: "1h" | "24h" | "7d";
  buckets: SummaryBucket[];
}

// MetricsResult is the typed-disabled discriminated union returned
// by the metrics helpers. The page's render branch reads this:
//   if (r.disabled) → show MetricsDisabledEmpty with enableHint
//   else            → render charts from r.data
//
// We deliberately don't throw on 503 — the disabled state is a
// LEGITIMATE runtime mode, not an error.
export type MetricsResult<T> =
  | { disabled: false; data: T }
  | { disabled: true; enableHint: string };

const DEFAULT_METRICS_DISABLED_HINT =
  "set LOOMCYCLE_METRICS_ENABLED=1 and restart loomcycle";

function asMetricsResult<T>(
  r: { ok: true; data: T } | { ok: false; status: number; body: any },
): MetricsResult<T> {
  if (r.ok) return { disabled: false, data: r.data };
  const hint =
    (r.body && typeof r.body.enable_hint === "string"
      ? r.body.enable_hint
      : null) ?? DEFAULT_METRICS_DISABLED_HINT;
  return { disabled: true, enableHint: hint };
}

export interface MetricsSamplesParams {
  since?: string;
  until?: string;
  limit?: number;
  cursor?: string;
}

export async function getMetricsSamples(
  params: MetricsSamplesParams = {},
): Promise<MetricsResult<SamplesResponse>> {
  const q = new URLSearchParams();
  if (params.since) q.set("since", params.since);
  if (params.until) q.set("until", params.until);
  if (params.limit !== undefined) q.set("limit", String(params.limit));
  if (params.cursor) q.set("cursor", params.cursor);
  const qs = q.toString();
  const r = await jsonFetchAllowing<SamplesResponse>(
    `/v1/_metrics/samples${qs ? "?" + qs : ""}`,
    [503],
  );
  return asMetricsResult(r);
}

export async function getMetricsSummary(
  period: "1h" | "24h" | "7d",
): Promise<MetricsResult<SummaryResponse>> {
  const r = await jsonFetchAllowing<SummaryResponse>(
    `/v1/_metrics/summary?period=${encodeURIComponent(period)}`,
    [503],
  );
  return asMetricsResult(r);
}

// ---- v0.9.x Introspection — Library + Channels + Agent sub-views ----

// Substrate name summary — same shape across AgentDef / SkillDef /
// MCPServerDef and the v0.24.0 families (Webhook / A2A / MemoryBackend).
// Returned by the GET /v1/_*/names endpoints.
export interface DefNameSummary {
  name: string;
  version_count: number;
  active_def_id?: string;
  latest_version: number;
  last_updated: string;
  // Tenant-isolated families (RFC N) return one summary row per
  // (name, tenant). Absent / "" in legacy/open mode. List rows must be
  // keyed on name+tenant_id to avoid collisions in multi-tenant deploys.
  tenant_id?: string;
}

export interface DefNamesResponse {
  names: DefNameSummary[];
}

// Substrate row — what `op:list` returns inside its `versions: []`
// array. Same fields across AgentDef / SkillDef / MCPServerDef so the
// UI's lineage tree can render any of them with one component.
export interface DefRow {
  def_id: string;
  name: string;
  version: number;
  parent_def_id?: string;
  description?: string;
  retired?: boolean;
  bootstrapped_from_static?: boolean;
  content_sha256?: string;
  created_at: string;
  created_by_agent_id?: string;
  created_by_run_id?: string;
  definition?: unknown;
}

export interface DefListByNameResponse {
  name: string;
  versions: DefRow[];
}

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
  oldest_visible_at?: string;
  newest_visible_at?: string;
  // v0.11.5: "yaml" (operator yaml, immutable from UI),
  // "runtime" (substrate, CRUD-mutable), "orphan" (no declaration —
  // just persisted messages from a removed/renamed channel).
  source?: "yaml" | "runtime" | "orphan" | string;
}

export interface ListChannelsResponse {
  channels: ChannelDescriptor[];
}

export interface ChannelMessageItem {
  id: string;
  value: unknown;
  published_at: string;
}

export interface ChannelPeekResponse {
  channel: string;
  messages: ChannelMessageItem[];
}

export interface ChannelCursorEntry {
  channel: string;
  scope: string;
  scope_id: string;
  cursor: string;
  updated_at: string;
}

export interface AgentChannelsResponse {
  channels: ChannelCursorEntry[];
}

// ---- Library v2 (unified — yaml + substrate merged) ----

// LibraryEntry is one row of the v0.9.x /v1/_library/* endpoints. It
// merges substrate name-summary fields with cfg-side staticDefinition,
// plus the source discriminator (computed server-side from
// in_static + in_substrate booleans).
export interface LibraryEntry {
  name: string;
  source: "static-only" | "dynamic-only" | "both";
  in_static: boolean;
  in_substrate: boolean;
  version_count: number;
  active_def_id?: string;
  latest_version?: number;
  last_updated?: string;
  /** Agents only (soft reclaim): live (non-retired) version count, and
   *  whether the active pointer references a retired row. A name with
   *  live_version_count === 0 and no live active is "inactive" — badged,
   *  and reclaimable by a fresh create. Absent for skills + mcp-servers. */
  live_version_count?: number;
  active_retired?: boolean;
  /**
   * Static-side definition payload. Same JSON shape as the substrate
   * body (snake_case keys mirroring lookup.SubstrateAgentDef /
   * skillDefOverlay / mcpServerOverlay) so the same renderer consumes
   * both static and dynamic sources. Omitted when in_static is false.
   */
  static_definition?: unknown;
}

export interface LibraryListResponse {
  entries: LibraryEntry[];
}

export function listLibraryAgents(): Promise<LibraryListResponse> {
  return jsonFetch<LibraryListResponse>("/v1/_library/agents");
}

export function listLibrarySkills(): Promise<LibraryListResponse> {
  return jsonFetch<LibraryListResponse>("/v1/_library/skills");
}

export function listLibraryMcpServers(): Promise<LibraryListResponse> {
  return jsonFetch<LibraryListResponse>("/v1/_library/mcp-servers");
}

// ---- Schedules (v1.x RFC E) ----

// ScheduleListEntry mirrors the server-side wire shape. Same merged
// pattern as LibraryEntry: yaml-defined entries carry static_definition
// inline; substrate-defined entries carry active_def_id + version
// counters; entries present in both have source="both" and both sets
// of fields populated.
export interface ScheduleListEntry {
  name: string;
  source: "static-only" | "dynamic-only" | "both";
  in_static: boolean;
  in_substrate: boolean;
  version_count?: number;
  active_def_id?: string;
  latest_version?: number;
  last_updated?: string;
  static_definition?: unknown;
}

export interface ScheduleListResponse {
  entries: ScheduleListEntry[];
}

export function listSchedules(): Promise<ScheduleListResponse> {
  return jsonFetch<ScheduleListResponse>("/v1/_schedules/list-all");
}

// ScheduleStateView is the runtime telemetry row — last/next + status.
// Sensitive substrate-stored fields (user_credentials) NEVER appear
// here; they live only behind POST /v1/_scheduledef {op:"get"}.
export interface ScheduleStateView {
  def_id: string;
  last_run_at?: string;
  last_run_id?: string;
  last_status?: string;
  last_error?: string;
  next_run_at: string;
  paused_until?: string;
}

export function getScheduleState(defID: string): Promise<ScheduleStateView> {
  return jsonFetch<ScheduleStateView>(`/v1/_schedules/${encodeURIComponent(defID)}/state`);
}

// ---- Schedule admin mutations (run-now / pause / resume) ----

export function scheduleRunNow(defID: string): Promise<{ def_id: string; scheduled: string }> {
  return jsonFetch(`/v1/_schedules/${encodeURIComponent(defID)}/run-now`, { method: "POST" });
}

export function schedulePause(defID: string): Promise<{ def_id: string; paused: boolean }> {
  return jsonFetch(`/v1/_schedules/${encodeURIComponent(defID)}/pause`, { method: "POST" });
}

export function scheduleResume(defID: string): Promise<{ def_id: string; paused: boolean }> {
  return jsonFetch(`/v1/_schedules/${encodeURIComponent(defID)}/resume`, { method: "POST" });
}

// ---- Schedule substrate ops (uses the existing POST /v1/_scheduledef) ----

// ScheduleDefRow mirrors the row envelope returned by the ScheduleDef
// tool's create / fork / get ops. Field shape matches store.ScheduleDefRow
// on the wire.
export interface ScheduleDefRow {
  def_id: string;
  name: string;
  version: number;
  parent_def_id?: string;
  description?: string;
  created_at: string;
  created_by_agent_id?: string;
  retired: boolean;
  bootstrapped_from_static: boolean;
  promoted?: boolean;
  definition?: Record<string, unknown>;
}

export interface ScheduleDefListResponse {
  name: string;
  versions: ScheduleDefRow[];
}

// scheduleDefGet fetches one def by def_id. Used by the UI's detail
// pane to render the substrate-side definition (including user_id,
// cron, on_complete, user_credentials — all of which the list endpoint
// deliberately omits).
export function scheduleDefGet(defID: string): Promise<ScheduleDefRow> {
  return jsonFetch<ScheduleDefRow>("/v1/_scheduledef", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ op: "get", def_id: defID }),
  });
}

// scheduleDefList fetches all versions of a name — drives the lineage
// tree on the detail pane.
export function scheduleDefList(name: string): Promise<ScheduleDefListResponse> {
  return jsonFetch<ScheduleDefListResponse>("/v1/_scheduledef", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ op: "list", name }),
  });
}

// scheduleDefCreate authors a brand-new schedule from scratch (no
// parent). The overlay carries agent + schedule (cron) + prompt +
// credentials; the server validates agent-required and the
// cron-XOR-user_tier_schedules invariant. `promote` flips the active
// pointer to the new version on create (the standalone-create default).
export function scheduleDefCreate(input: {
  name: string;
  overlay: Record<string, unknown>;
  description?: string;
  promote?: boolean;
}): Promise<ScheduleDefRow> {
  return jsonFetch<ScheduleDefRow>("/v1/_scheduledef", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ op: "create", ...input }),
  });
}

// scheduleDefActivate re-activates an older version. ScheduleDef has
// NO standalone promote op (see scheduledef.go: "schedules'
// fork-auto-promote model makes a separate promote step unnecessary"),
// so activating a prior version is a zero-overlay fork from it — the
// new version inherits the parent's definition and becomes active.
export function scheduleDefActivate(name: string, parentDefID: string): Promise<ScheduleDefRow> {
  return scheduleDefFork({ name, parent_def_id: parentDefID, overlay: {} });
}

// scheduleDefFork creates a new version from an existing parent. The
// overlay carries the user-supplied fields (user_id, user_tier,
// user_credentials, optional cron override). Returns the new fork row.
export function scheduleDefFork(input: {
  name: string;
  parent_def_id?: string;
  overlay: Record<string, unknown>;
  description?: string;
}): Promise<ScheduleDefRow> {
  return jsonFetch<ScheduleDefRow>("/v1/_scheduledef", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ op: "fork", ...input }),
  });
}

// scheduleDefRetire flips the retired flag (true to retire, false to
// un-retire). Returns {def_id, retired}.
export function scheduleDefRetire(defID: string, retired: boolean): Promise<{ def_id: string; retired: boolean }> {
  return jsonFetch("/v1/_scheduledef", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ op: "retire", def_id: defID, retired }),
  });
}

// ScheduleHook mirrors mergedScheduleHook on the substrate side. Three
// closed-set kinds: channel.publish, mcp.call, memory.set. Kind-
// specific fields are required by the server-side validator; the type
// here keeps everything optional because the actual presence depends
// on `kind`.
export interface ScheduleHook {
  kind: "channel.publish" | "mcp.call" | "memory.set";
  // channel.publish:
  channel?: string;
  payload?: Record<string, unknown>;
  // mcp.call:
  server?: string;
  tool?: string;
  args?: Record<string, unknown>;
  // memory.set:
  scope?: "agent" | "user" | "global";
  key?: string;
}

// scheduleDefAddHook appends a hook to a def's on_complete list,
// persisting as a new fork version with auto-promote. The substrate
// validates the hook (kind enum + kind-specific required fields)
// server-side; malformed hooks refuse with SubstrateToolRefusedError.
export function scheduleDefAddHook(defID: string, hook: ScheduleHook): Promise<ScheduleDefRow> {
  return jsonFetch<ScheduleDefRow>("/v1/_scheduledef", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ op: "add_hook", def_id: defID, hook }),
  });
}

// scheduleDefRemoveHook removes the hook at hook_index from a def's
// on_complete list, persisting as a new fork version with auto-promote.
// hook_index is 0-based and refers to the index in the PARENT
// version's list; out-of-range refuses.
export function scheduleDefRemoveHook(defID: string, hookIndex: number): Promise<ScheduleDefRow> {
  return jsonFetch<ScheduleDefRow>("/v1/_scheduledef", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ op: "remove_hook", def_id: defID, hook_index: hookIndex }),
  });
}

// ---- Legacy /names fetchers (substrate-only) ----
//
// Kept for the existing Library UI v1 surface + any external adapter
// consumers that pinned the substrate-only wire shape. Library UI v2
// uses listLibrary{Agents,Skills,McpServers} above.

export function listAgentDefNames(): Promise<DefNamesResponse> {
  return jsonFetch<DefNamesResponse>("/v1/_agentdef/names");
}

export function listSkillDefNames(): Promise<DefNamesResponse> {
  return jsonFetch<DefNamesResponse>("/v1/_skilldef/names");
}

export function listMcpServerDefNames(): Promise<DefNamesResponse> {
  return jsonFetch<DefNamesResponse>("/v1/_mcpserverdef/names");
}

// v0.24.0 "Integrations" families — same /names wire shape as the
// three above. These have no unified /v1/_library/* endpoint, so the
// UI drives its list from /names + per-name lineage (op:list).
export function listWebhookDefNames(): Promise<DefNamesResponse> {
  return jsonFetch<DefNamesResponse>("/v1/_webhookdef/names");
}

export function listA2AServerCardDefNames(): Promise<DefNamesResponse> {
  return jsonFetch<DefNamesResponse>("/v1/_a2aservercarddef/names");
}

export function listA2AAgentDefNames(): Promise<DefNamesResponse> {
  return jsonFetch<DefNamesResponse>("/v1/_a2aagentdef/names");
}

export function listMemoryBackendDefNames(): Promise<DefNamesResponse> {
  return jsonFetch<DefNamesResponse>("/v1/_memorybackenddef/names");
}

// listDefVersionsByName uses the existing op-discriminated POST
// endpoint with `{op:"list", name}` to retrieve every version of one
// declared name. Used by the Library UI when an operator clicks into
// a name to inspect its lineage.
export function listDefVersionsByName(
  kind: SubstrateKind,
  name: string,
): Promise<DefListByNameResponse> {
  return jsonFetch<DefListByNameResponse>(`/v1/_${kind}`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ op: "list", name }),
  });
}

// ---- v0.10.4 Substrate admin mutations (Library admin UI) ----
//
// Thin wrappers around the existing POST /v1/_{agentdef,skilldef,
// mcpserverdef} endpoints (v0.8.22 + v0.9.x). All ops dispatch through
// the same op-discriminated body shape. On refusal the substrate
// returns HTTP 422 + `{"code":"tool_refused","error":"<text>","tool":"X"}`;
// jsonFetch surfaces this as a thrown Error whose message contains
// the JSON body — callers parse for the human-readable text.

export type SubstrateKind =
  | "agentdef"
  | "skilldef"
  | "mcpserverdef"
  | "webhookdef"
  | "a2aservercarddef"
  | "a2aagentdef"
  | "memorybackenddef"
  // RFC AH dynamic volume substrate. Flat (no versioning): the only ops are
  | "volumedef"; // create / delete / purge — not the create/fork/promote/retire lifecycle.

export function createDef(
  kind: SubstrateKind,
  name: string,
  overlay: Record<string, unknown>,
  promote: boolean,
): Promise<DefRow> {
  return substrateDispatch<DefRow>(kind, {
    op: "create",
    name,
    overlay,
    promote,
  });
}

export function forkDef(
  kind: SubstrateKind,
  name: string,
  overlay: Record<string, unknown>,
  promote: boolean,
  parentDefID?: string,
): Promise<DefRow> {
  const body: Record<string, unknown> = {
    op: "fork",
    name,
    overlay,
    promote,
  };
  if (parentDefID) body.parent_def_id = parentDefID;
  return substrateDispatch<DefRow>(kind, body);
}

export function promoteDef(kind: SubstrateKind, defID: string): Promise<unknown> {
  return substrateDispatch<unknown>(kind, { op: "promote", def_id: defID });
}

export function retireDef(kind: SubstrateKind, defID: string): Promise<unknown> {
  return substrateDispatch<unknown>(kind, {
    op: "retire",
    def_id: defID,
    retired: true,
  });
}

// rediscoverMcpServerDef is MCP-only. Re-runs the upstream MCP server's
// tools/list handshake, refreshes the cached discovered_tools, and
// forks+promotes a new version with the refreshed snapshot.
export function rediscoverMcpServerDef(name: string): Promise<DefRow> {
  return substrateDispatch<DefRow>("mcpserverdef", {
    op: "rediscover",
    name,
  });
}

function substrateDispatch<T>(
  kind: SubstrateKind,
  body: Record<string, unknown>,
): Promise<T> {
  return jsonFetch<T>(`/v1/_${kind}`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
  });
}

// ---- RFC AH Phase 4 Volumes (Web UI volume management) ----
//
// Reads come from two ADDITIVE, tenant-scoped endpoints; CRUD reuses the
// existing POST /v1/_volumedef op-discriminated dispatch. A VolumeDef is FLAT
// (no versions / promote / retire) — the op set is create / delete / purge.

export type VolumeMode = "rw" | "ro";

// PersistentVolumeEntry is one row of GET /v1/_volumes. STATIC rows
// (source="static") are operator yaml — read-only from the UI; DYNAMIC rows
// (source="dynamic") are the caller-tenant's VolumeDefs — create/delete/purge.
export interface PersistentVolumeEntry {
  name: string;
  source: "static" | "dynamic";
  path: string;
  mode: VolumeMode | string;
  /** Static volume the operator flagged default:true. Dynamic rows: false. */
  default: boolean;
  /** Static volume that is the operator-blessed parent dynamic volumes are
   *  provisioned inside. Dynamic rows: false. */
  dynamic_root: boolean;
  /** Set for dynamic rows (substrate-stamped); absent/"" for static volumes. */
  created_at?: string;
}

export interface PersistentVolumesResponse {
  entries: PersistentVolumeEntry[];
}

// EphemeralVolumeEntry is one row of GET /v1/_volumes/ephemeral — a live,
// run-scoped volume auto-purged at run completion.
export interface EphemeralVolumeEntry {
  name: string;
  root_run_id: string;
  path: string;
  mode: VolumeMode | string;
  created_at: string;
}

export interface EphemeralVolumesResponse {
  entries: EphemeralVolumeEntry[];
}

// listVolumes fetches the merged persistent volume universe for the caller's
// tenant: static cfg volumes (the shared bind floor) + the tenant's dynamic
// VolumeDefs. Tenant scoping is server-side (authoritative principal).
export function listVolumes(): Promise<PersistentVolumesResponse> {
  return jsonFetch<PersistentVolumesResponse>("/v1/_volumes");
}

// listEphemeralVolumes fetches the caller-tenant's live ephemeral volumes.
export function listEphemeralVolumes(): Promise<EphemeralVolumesResponse> {
  return jsonFetch<EphemeralVolumesResponse>("/v1/_volumes/ephemeral");
}

// VolumeDefRow mirrors the {name, path, mode, created_at} the VolumeDef tool
// returns on create. The runtime DERIVES the path (the caller never supplies a
// host path), so create takes only name + mode.
export interface VolumeDefRow {
  name: string;
  path?: string;
  mode?: string;
  created_at?: string;
  updated_at?: string;
}

// createVolume provisions a dynamic VolumeDef (name + mode only — the runtime
// derives the path inside the operator-blessed dynamic_root). Idempotent: a
// re-create with the same name updates the mode.
export function createVolume(name: string, mode: VolumeMode): Promise<VolumeDefRow> {
  return substrateDispatch<VolumeDefRow>("volumedef", { op: "create", name, mode });
}

// deleteVolume is NON-destructive — it unmaps the volume (removes the row) but
// LEAVES the files on disk. Mirrors the tool's `delete` op.
export function deleteVolume(name: string): Promise<{ name: string; deleted: boolean; files_removed: boolean }> {
  return substrateDispatch<{ name: string; deleted: boolean; files_removed: boolean }>(
    "volumedef",
    { op: "delete", name },
  );
}

// purgeVolume is DESTRUCTIVE — it removes the row AND RemoveAll's the directory
// tree (behind the server-side four-way fence). The UI gates this behind a
// type-to-confirm modal; the server-side fence is the real guard.
export function purgeVolume(name: string): Promise<{ name: string; deleted: boolean; files_removed: boolean }> {
  return substrateDispatch<{ name: string; deleted: boolean; files_removed: boolean }>(
    "volumedef",
    { op: "purge", name },
  );
}

// ---- Channel fetchers ----

export function listChannels(): Promise<ListChannelsResponse> {
  return jsonFetch<ListChannelsResponse>("/v1/_channels");
}

export function peekChannel(
  name: string,
  opts: { maxMessages?: number; fromCursor?: string } = {},
): Promise<ChannelPeekResponse> {
  const q = new URLSearchParams();
  if (opts.maxMessages !== undefined)
    q.set("max_messages", String(opts.maxMessages));
  if (opts.fromCursor) q.set("from_cursor", opts.fromCursor);
  const qs = q.toString();
  return jsonFetch<ChannelPeekResponse>(
    `/v1/_channels/${encodeURIComponent(name)}/peek${qs ? "?" + qs : ""}`,
  );
}

export function listAgentChannels(agentName: string): Promise<AgentChannelsResponse> {
  return jsonFetch<AgentChannelsResponse>(
    `/v1/agents/${encodeURIComponent(agentName)}/channels`,
  );
}

export interface ChannelPublishResponse {
  msg_id: string;
  channel: string;
  created_at: string;
  // Set only when the publish was deferred (deliver_at in the future).
  visible_at?: string;
}

// publishChannel posts a message to a channel via the admin publish
// route (handleAdminChannelPublish). `payload` is the raw JSON value
// (object / array / string / number) — REQUIRED and may not be null.
// `deliver_at` (RFC3339) defers delivery; omit for "publish now".
// Server validation: missing/null/invalid payload → 400; oversize → 413
// payload_too_large; bad deliver_at → 400 invalid_deliver_at.
export function publishChannel(
  name: string,
  body: { payload: unknown; deliver_at?: string },
): Promise<ChannelPublishResponse> {
  return jsonFetch<ChannelPublishResponse>(
    `/v1/_channels/${encodeURIComponent(name)}/publish`,
    {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
    },
  );
}

// ---- v0.25.0 Channel fan-out (broadcast) / fan-in (await) ----

// ChannelScope is the addressing scope shared by broadcast + await.
// "global" needs no scope_id; "user"/"agent" require one.
export type ChannelScope = "global" | "user" | "agent";

export interface BroadcastChannelsRequest {
  channels: string[];
  scope: ChannelScope;
  scope_id?: string;
  payload: unknown;
  // RFC3339; defers delivery for every channel. Omit for "publish now".
  deliver_at?: string;
}

export interface ChannelBroadcastEntry {
  channel: string;
  msg_id?: string;
  created_at?: string;
  // Set only when the publish was deferred (deliver_at in the future).
  visible_at?: string;
  // Set (and msg_id empty) when that channel's write failed after the
  // pre-flight passed — the successful publishes still stand.
  error?: string;
}

export interface BroadcastChannelsResponse {
  published: number;
  failed: number;
  results: ChannelBroadcastEntry[];
}

// broadcastChannels publishes one payload to a SET of channels (same scope,
// same payload). ATOMIC at the ACL pre-flight — one undeclared/invalid
// channel refuses the whole op (nothing is published). Server caps the set
// at 32 channels.
export function broadcastChannels(
  body: BroadcastChannelsRequest,
): Promise<BroadcastChannelsResponse> {
  return jsonFetch<BroadcastChannelsResponse>("/v1/_channels/_broadcast", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
  });
}

export type ChannelAwaitMode = "any" | "all" | "at_least";

export interface AwaitChannelsRequest {
  channels: string[];
  scope: ChannelScope;
  scope_id?: string;
  mode?: ChannelAwaitMode; // default "any"
  n?: number; // threshold for at_least (>0)
  from_cursor?: string;
  max_messages?: number;
  wait_ms?: number; // bounded long-poll
}

export interface ChannelAwaitEntry {
  messages: ChannelMessageItem[];
  next_cursor: string;
}

export interface AwaitChannelsResponse {
  satisfied: boolean;
  timed_out: boolean;
  mode: string;
  fired: string[];
  total_messages: number;
  results: Record<string, ChannelAwaitEntry>;
}

// awaitChannels fans IN across a SET of channels: a bounded long-poll (up to
// wait_ms) that returns when the mode predicate is met (any / all / at_least
// n) or the timeout elapses. Reads are NON-committing (detection only); the
// returned cursors are not advanced. timed_out=true means the predicate was
// unmet within wait_ms — not an error.
export function awaitChannels(
  body: AwaitChannelsRequest,
): Promise<AwaitChannelsResponse> {
  return jsonFetch<AwaitChannelsResponse>("/v1/_channels/_await", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
  });
}

// ---- v0.11.5 channel admin CRUD ----

export interface ChannelCreateRequest {
  name: string;
  description?: string;
  scope?: string;
  semantic?: string;
  default_ttl?: number;
  max_messages?: number;
  publisher?: string;
  period?: string;
}

export interface ChannelUpdateRequest {
  description?: string;
  default_ttl?: number;
  max_messages?: number;
  semantic?: string;
}

export function createChannel(body: ChannelCreateRequest): Promise<ChannelDescriptor> {
  return jsonFetch<ChannelDescriptor>("/v1/_channels", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
  });
}

export function updateChannel(
  name: string,
  patch: ChannelUpdateRequest,
): Promise<ChannelDescriptor> {
  return jsonFetch<ChannelDescriptor>(`/v1/_channels/${encodeURIComponent(name)}`, {
    method: "PATCH",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(patch),
  });
}

export async function deleteChannel(name: string): Promise<void> {
  const resp = await fetch(`/v1/_channels/${encodeURIComponent(name)}`, {
    method: "DELETE",
    credentials: "same-origin",
    headers: { Accept: "application/json" },
  });
  if (!resp.ok && resp.status !== 204) {
    const body = await resp.text();
    throw new Error(`${resp.status} ${resp.statusText}: ${body.slice(0, 200)}`);
  }
}

// ---- v0.11.5 memory admin CRUD ----

export interface MemoryEntrySetRequest {
  value: unknown;
  embed?: boolean;
  ttl_seconds?: number;
}

export interface MemoryEntrySetResponse {
  scope: string;
  scope_id: string;
  key: string;
  embedded: boolean;
  embed_warning?: string;
}

export function setMemoryEntry(
  scope: string,
  scopeID: string,
  key: string,
  body: MemoryEntrySetRequest,
): Promise<MemoryEntrySetResponse> {
  return jsonFetch<MemoryEntrySetResponse>(
    `/v1/_memory/scopes/${encodeURIComponent(scope)}/${encodeURIComponent(scopeID)}/keys/${encodeURIComponent(key)}`,
    {
      method: "PUT",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
    },
  );
}

export async function deleteMemoryEntry(
  scope: string,
  scopeID: string,
  key: string,
): Promise<void> {
  const resp = await fetch(
    `/v1/_memory/scopes/${encodeURIComponent(scope)}/${encodeURIComponent(scopeID)}/keys/${encodeURIComponent(key)}`,
    {
      method: "DELETE",
      credentials: "same-origin",
      headers: { Accept: "application/json" },
    },
  );
  if (!resp.ok && resp.status !== 204) {
    const body = await resp.text();
    throw new Error(`${resp.status} ${resp.statusText}: ${body.slice(0, 200)}`);
  }
}

// ---- v0.24.0 Run execution + SSE streaming ----
//
// POST /v1/runs and POST /v1/sessions/{id}/messages return
// text/event-stream. EventSource can't POST a body or set headers, but
// the loomcycle_session cookie authenticates fetch() same-origin (the
// server's extractBearer cookie fallback), so we read the body as a
// ReadableStream and parse SSE frames ourselves. This is the UI's first
// streaming reader — every other call is a one-shot jsonFetch.

export interface SSEFrame {
  event: string; // the `event:` name ("session"|"agent"|"text"|... )
  data: string; // the raw `data:` JSON string
}

export interface RunStreamHandlers {
  // onFrame receives each parsed SSE frame (comment/keepalive frames are
  // dropped before this is called). The caller parses frame.data as JSON.
  onFrame: (f: SSEFrame) => void;
  signal?: AbortSignal; // wire to an AbortController for cancel/unmount
}

// streamSSE POSTs and dispatches parsed SSE frames, resolving when the
// server closes the stream (run done/failed → EOF). It buffers decoded
// chunks, splits on the `\n\n` frame boundary, reads the `event:` +
// (possibly multiple) `data:` lines per the SSE spec, and drops
// comment-only frames (lines starting with `:` — loomcycle's keepalive).
// A non-OK initial response bounces to /login on 401, else throws with
// the body so the caller surfaces it before the first frame.
async function streamSSE(
  path: string,
  body: unknown,
  h: RunStreamHandlers,
): Promise<void> {
  const resp = await fetch(baseURL + path, {
    method: "POST",
    credentials: "same-origin",
    signal: h.signal,
    headers: { "Content-Type": "application/json", Accept: "text/event-stream" },
    body: JSON.stringify(body),
  });
  if (!resp.ok) {
    if (redirectToLoginOn401(resp.status)) {
      return new Promise<void>(() => {});
    }
    const text = await resp.text();
    throw new Error(`${resp.status} ${resp.statusText}: ${text.slice(0, 200)}`);
  }
  if (!resp.body) {
    throw new Error("stream response had no body");
  }
  await pumpSSE(resp.body, h.onFrame);
}

// streamRunByID RE-ATTACHES to a running (or finished) run's event stream via
// GET /v1/runs/{run_id}/stream — the operator left the interactive terminal
// and returned to the same live run. Replays from fromSeq (0 = whole run) then
// live-tails until the run terminates or the reader is aborted. Same frame
// shape as the POST /v1/runs stream, so the caller's onFrame is unchanged.
export async function streamRunByID(
  runID: string,
  fromSeq: number,
  h: RunStreamHandlers,
): Promise<void> {
  const q = fromSeq > 0 ? `?from_seq=${fromSeq}` : "";
  const resp = await fetch(
    `${baseURL}/v1/runs/${encodeURIComponent(runID)}/stream${q}`,
    {
      method: "GET",
      credentials: "same-origin",
      signal: h.signal,
      headers: { Accept: "text/event-stream" },
    },
  );
  if (!resp.ok) {
    if (redirectToLoginOn401(resp.status)) {
      return new Promise<void>(() => {});
    }
    const text = await resp.text();
    throw new Error(`${resp.status} ${resp.statusText}: ${text.slice(0, 200)}`);
  }
  if (!resp.body) {
    throw new Error("stream response had no body");
  }
  await pumpSSE(resp.body, h.onFrame);
}

// pumpSSE drains a text/event-stream ReadableStream, parsing frames and
// invoking onFrame for each non-comment frame. Shared by streamSSE (POST)
// and streamRunStates (GET).
async function pumpSSE(
  stream: ReadableStream<Uint8Array>,
  onFrame: (f: SSEFrame) => void,
): Promise<void> {
  const reader = stream.getReader();
  const decoder = new TextDecoder();
  let buf = "";
  const flushFrame = (raw: string) => {
    let event = "message";
    const dataLines: string[] = [];
    for (const line of raw.split("\n")) {
      // Skip blank lines and comment lines (`:`-prefixed, e.g. loomcycle's
      // keepalive) per-line, so a frame that mixes a comment with data
      // still dispatches its data.
      if (line === "" || line.startsWith(":")) continue;
      if (line.startsWith("event:")) {
        event = line.slice(6).trim();
      } else if (line.startsWith("data:")) {
        dataLines.push(line.slice(5).replace(/^ /, ""));
      }
    }
    // Pure-comment / event-only frames carry no data — nothing to dispatch.
    if (dataLines.length === 0) return;
    onFrame({ event, data: dataLines.join("\n") });
  };
  for (;;) {
    const { done, value } = await reader.read();
    if (done) break;
    buf += decoder.decode(value, { stream: true });
    let sep: number;
    while ((sep = buf.indexOf("\n\n")) !== -1) {
      const raw = buf.slice(0, sep);
      buf = buf.slice(sep + 2);
      if (raw.trim() !== "") flushFrame(raw);
    }
  }
  // Flush any trailing frame the server didn't terminate with \n\n.
  if (buf.trim() !== "") flushFrame(buf);
}

export interface StartRunRequest {
  agent: string; // REQUIRED — the agent name (LibraryEntry.name)
  prompt: string; // becomes one user trusted-text segment
  user_id?: string;
  agent_id?: string; // optional caller handle; else the server generates one
  session_id?: string;
  user_tier?: string;
  // omit => no host narrowing; [] => deny all; non-empty => intersection
  allowed_hosts?: string[];
  web_search_filter?: "drop" | "keep";
  tools?: string[];
  metadata?: Record<string, unknown>;
  // interactive: start a PERSISTENT run that parks for operator steering at
  // end_turn instead of terminating (the interactive terminal session mode).
  // Drive it via sendRunInput; pair with an unbounded_iterations agent for a
  // true always-on terminal. Cancel ends it.
  interactive?: boolean;
}

// startRun POSTs /v1/runs and streams events to the handlers. The prompt
// becomes a single user trusted-text segment (the agent's own system
// prompt is prepended server-side — do NOT send a system segment).
// Resolves at stream EOF.
export function startRun(req: StartRunRequest, h: RunStreamHandlers): Promise<void> {
  const body: Record<string, unknown> = {
    agent: req.agent,
    segments: [
      { role: "user", content: [{ type: "trusted-text", text: req.prompt }] },
    ],
  };
  if (req.user_id) body.user_id = req.user_id;
  if (req.agent_id) body.agent_id = req.agent_id;
  if (req.session_id) body.session_id = req.session_id;
  if (req.user_tier) body.user_tier = req.user_tier;
  if (req.allowed_hosts !== undefined) body.allowed_hosts = req.allowed_hosts;
  if (req.web_search_filter) body.web_search_filter = req.web_search_filter;
  if (req.tools && req.tools.length > 0)
    body.tools = req.tools;
  if (req.metadata) body.metadata = req.metadata;
  if (req.interactive) body.interactive = true;
  return streamSSE("/v1/runs", body, h);
}

// sendRunInput injects an operator "steering" instruction into an IN-FLIGHT
// run (POST /v1/runs/{run_id}/input) — appended to the live conversation at
// the loop's next iteration (and resumes a parked interactive run). 404 if no
// run is live for run_id; 429 if its input buffer is full; 422 on empty text.
export async function sendRunInput(runID: string, text: string): Promise<void> {
  const resp = await fetch(`/v1/runs/${encodeURIComponent(runID)}/input`, {
    method: "POST",
    credentials: "same-origin",
    headers: { "Content-Type": "application/json", Accept: "application/json" },
    body: JSON.stringify({ text }),
  });
  if (!resp.ok) {
    const b = await resp.text();
    throw new Error(`${resp.status} ${resp.statusText}: ${b.slice(0, 200)}`);
  }
}

// CompactResult mirrors the POST /v1/runs/{run_id}/compact JSON body.
export interface CompactResult {
  run_id: string;
  compacted: boolean;
  before_tokens: number;
  after_tokens: number;
  applied: string; // "live" | "marker" | "noop"
}

// compactRun summarizes a run's conversation to free context and continue from
// the summary (POST /v1/runs/{run_id}/compact). Only valid at a safe boundary
// (the agent parked awaiting input, or a terminal run) — the server returns 409
// mid-turn.
export async function compactRun(runID: string): Promise<CompactResult> {
  const resp = await fetch(`/v1/runs/${encodeURIComponent(runID)}/compact`, {
    method: "POST",
    credentials: "same-origin",
    headers: { "Content-Type": "application/json", Accept: "application/json" },
    body: JSON.stringify({}),
  });
  if (!resp.ok) {
    const b = await resp.text();
    throw new Error(`${resp.status} ${resp.statusText}: ${b.slice(0, 200)}`);
  }
  return resp.json() as Promise<CompactResult>;
}

// continueSession appends a new turn to an existing session
// (POST /v1/sessions/{id}/messages — same SSE stream shape as /v1/runs).
// A 409 session_busy surfaces via the thrown error before the first frame.
export function continueSession(
  sessionId: string,
  prompt: string,
  h: RunStreamHandlers,
  opts?: {
    user_tier?: string;
    allowed_hosts?: string[];
    web_search_filter?: "drop" | "keep";
  },
): Promise<void> {
  const body: Record<string, unknown> = {
    segments: [
      { role: "user", content: [{ type: "trusted-text", text: prompt }] },
    ],
  };
  if (opts?.user_tier) body.user_tier = opts.user_tier;
  if (opts?.allowed_hosts !== undefined) body.allowed_hosts = opts.allowed_hosts;
  if (opts?.web_search_filter) body.web_search_filter = opts.web_search_filter;
  return streamSSE(
    `/v1/sessions/${encodeURIComponent(sessionId)}/messages`,
    body,
    h,
  );
}

// sseEventToTranscript wraps a bare providers.Event (parsed from a run
// SSE frame's data) into the persisted TranscriptEvent envelope the
// existing TerminalTranscript / EventCard renderers consume — so the
// live stream renders through the exact same components as the persisted
// transcript. seq is a monotonic client counter; ts_ns is best-effort
// client time (the live stream carries no server seq/ts).
export function sseEventToTranscript(
  seq: number,
  ev: EventPayload,
): TranscriptEvent {
  return {
    seq,
    run_id: "",
    ts_ns: Date.now() * 1_000_000,
    type: ev.type,
    event: ev,
  };
}

// userEchoTranscript builds a CLIENT-ONLY transcript entry echoing the
// operator's own message (initial prompt, steer, or continuation) into
// the live terminal. The persisted `user_input` event is filtered from
// the live SSE tail (run_stream.go nonStreamableEventTypes), so without
// this the operator never sees what they typed. type "user_echo" renders
// as `❯ {text}` (TerminalTranscript) — distinct from the agent's text and
// from a drained `steer` frame. Never sent to the server.
export function userEchoTranscript(seq: number, text: string): TranscriptEvent {
  return {
    seq,
    run_id: "",
    ts_ns: Date.now() * 1_000_000,
    type: "user_echo",
    event: { type: "user_echo", text },
  };
}

// ─── RFC AQ / RFC L — Settings hub: embedded presets + operator tokens ───────

export interface PresetUnit {
  name: string;
  kind: string; // "preset" | "bundle"
  description: string;
}

// listPresets / showPreset / getEnvTemplate mirror the `loomcycle presets` /
// `env-template` CLI over HTTP (admin-gated) — the embedded config base, viewable
// on a no-shell deployment.
export function listPresets(): Promise<{ units: PresetUnit[] }> {
  return jsonFetch<{ units: PresetUnit[] }>("/v1/_presets");
}

export function showPreset(name: string): Promise<{ name: string; yaml: string }> {
  return jsonFetch<{ name: string; yaml: string }>(`/v1/_presets/${encodeURIComponent(name)}`);
}

export function getEnvTemplate(): Promise<{ env: string }> {
  return jsonFetch<{ env: string }>("/v1/_env_template");
}

// Operator/tenant token management (RFC L). POST /v1/_operatortokendef dispatches
// by `op`. A refused op returns 422 {code:"tool_refused", error, tool}; the
// helper surfaces the human-readable `error`.
export interface OperatorTokenNameSummary {
  name: string;
  tenant_id: string;
  subject: string;
  token_count: number;
  has_current: boolean;
  last_updated: string;
}

export interface OperatorTokenCreateResult {
  def_id: string;
  name: string;
  tenant_id: string;
  subject: string;
  allowed_scopes: string[];
  created_at: string;
  token: string; // plaintext — shown ONCE, never retrievable again
  token_suffix: string;
  warning: string;
  retired: boolean;
}

async function postOperatorToken<T>(input: Record<string, unknown>): Promise<T> {
  const resp = await fetch(baseURL + "/v1/_operatortokendef", {
    method: "POST",
    credentials: "same-origin",
    headers: { "Content-Type": "application/json", Accept: "application/json" },
    body: JSON.stringify(input),
  });
  if (!resp.ok) {
    if (redirectToLoginOn401(resp.status)) return new Promise<T>(() => {});
    const raw = await resp.text();
    let msg = `${resp.status} ${resp.statusText}: ${raw.slice(0, 200)}`;
    try {
      const env = JSON.parse(raw);
      if (env && typeof env.error === "string") msg = env.error;
    } catch {
      // non-JSON body — keep the status-line message
    }
    throw new Error(msg);
  }
  return resp.json() as Promise<T>;
}

// listOperatorTokens returns one summary per token NAME (no secrets).
export function listOperatorTokens(): Promise<{ names: OperatorTokenNameSummary[] }> {
  return jsonFetch<{ names: OperatorTokenNameSummary[] }>("/v1/_operatortokendef/names");
}

export function mintOperatorToken(req: {
  name: string;
  tenant_id: string;
  subject?: string;
  scopes?: string[];
}): Promise<OperatorTokenCreateResult> {
  return postOperatorToken<OperatorTokenCreateResult>({ op: "create", ...req });
}

export function rotateOperatorToken(name: string, graceSeconds?: number): Promise<OperatorTokenCreateResult> {
  const input: Record<string, unknown> = { op: "rotate", name };
  if (graceSeconds && graceSeconds > 0) input.grace_seconds = graceSeconds;
  return postOperatorToken<OperatorTokenCreateResult>(input);
}

export function retireOperatorToken(
  name: string,
): Promise<{ def_id: string; name: string; retired: boolean }> {
  return postOperatorToken<{ def_id: string; name: string; retired: boolean }>({ op: "retire", name });
}

// The RFC L closed scope catalog — the token-mint form's scope choices.
// Scopes the Web UI is allowed to MINT. `substrate:admin` is deliberately
// EXCLUDED: minting a global admin token trips the runtime's no-lockout
// migration gate (legacyFallbackDisabled), which disables the
// LOOMCYCLE_AUTH_TOKEN login — and the UI mint loses the show-once secret in
// the resulting logout, locking the operator out entirely. Admin tokens must
// be created from the CLI (`loomcycle operator-token create --scopes
// substrate:admin …`), which prints the new token so access is never lost.
export const TOKEN_SCOPES = [
  "substrate:tenant",
  "runs:create",
  "runs:read",
  "channel:publish",
  "channel:read",
  // RFC AX: grants the run the operator's HOST provider API key. Tenant-implied
  // (substrate:tenant already covers it), so it only matters on a GRANULAR token
  // when the deployment sets LOOMCYCLE_OPERATOR_KEY_RESTRICTION — OMIT it there to
  // force that tenant to bring its own key (an RFC AR CredentialDef). Inert when
  // the gate is off.
  "providers:operator-key",
] as const;

// ---- Routing view (GET /v1/_routing) ----
// For each user_tier × tier, the ordered provider/model cascade a consumer
// resolves to right now. The admin view additionally carries live availability
// per candidate + an active-providers header; a substrate:tenant view is the
// config cascade only (availability/infra fields are absent — the API omits
// them, so the UI keys rendering off their presence, not `admin`).
export interface RoutingCandidate {
  provider: string;
  model: string;
  primary: boolean; // configured top of the cascade
  // Admin-only live-availability fields (undefined in a tenant view).
  available?: boolean;
  selected?: boolean; // first AVAILABLE — what runs now
  stalled?: boolean;
  rate_limited?: boolean;
  reachable?: boolean;
}

export interface RoutingTier {
  tier: string; // low / middle / high
  cascade: RoutingCandidate[];
}

export interface RoutingUserTier {
  name: string; // "" in library-mode (no user_tiers configured)
  tiers: RoutingTier[];
}

export interface RoutingProvider {
  provider: string;
  reachable: boolean;
  excluded: boolean;
  last_error?: string;
}

export interface RoutingResponse {
  generated_at: string;
  admin: boolean;
  providers?: RoutingProvider[]; // admin-only
  user_tiers: RoutingUserTier[];
  /**
   * RFC AX: true when the operator-key gate is ON and this (non-admin) tenant's
   * cascade has been filtered to providers it can key itself (needs no operator
   * key, or has an own CredentialDef). The UI shows a bring-your-own-key note.
   */
  operator_key_restricted?: boolean;
  /**
   * RFC BB: the web-search provider cascade — a single flat list (search has no
   * tier/model dimension), each with keyability + live availability. Omitted
   * when no search providers are configured.
   */
  search?: SearchRoutingProvider[];
}

// SearchRoutingProvider is one entry in the RFC BB search cascade: the ordered
// web-search providers, each with whether this caller can key it + its live
// availability. Rendered by field presence, like the LLM cascade.
export interface SearchRoutingProvider {
  provider: string;
  primary: boolean; // first in the (post-filter) cascade
  keyable?: boolean; // this caller has a usable key (operator/own-cred/keyless)
  available?: boolean; // keyable AND not in a failure cooldown
  selected?: boolean; // the first available provider — what runs now
  reachable?: boolean; // not in a cooldown, regardless of key
  last_error?: string; // admin-only
}

export function getRouting(): Promise<RoutingResponse> {
  return jsonFetch<RoutingResponse>("/v1/_routing");
}

// --- RFC AR: secure credential store (POST /v1/_credentialdef) ---

// CredentialScope buckets a stored secret. tenant = shared across the tenant;
// user = the calling principal's own subject (per-user tokens). The UI offers
// tenant | user; scope_id is derived server-side from the authoritative
// identity, never sent by the client.
export type CredentialScope = "tenant" | "user";

// CredentialMeta is one credential's METADATA — never the secret value (the API
// returns none for list/get). Mirrors the tool's credentialMeta output.
export interface CredentialMeta {
  name: string;
  scope: string;
  backend?: string;
  created_at?: string;
  updated_at?: string;
  expires_at?: string;
  status?: string;
}

export interface CredentialListResponse {
  scope: string;
  credentials: CredentialMeta[];
}

// listCredentials returns metadata for the given scope (never a value).
export function listCredentials(
  scope: CredentialScope,
): Promise<CredentialListResponse> {
  return jsonFetch<CredentialListResponse>("/v1/_credentialdef", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ op: "list", scope }),
  });
}

// createCredential stores (or rotates) a secret. The plaintext `value` is
// write-only: the server encrypts it, masks it from the transcript, and never
// returns it. Returns the stored row's metadata.
export function createCredential(input: {
  scope: CredentialScope;
  name: string;
  value: string;
}): Promise<CredentialMeta> {
  return jsonFetch<CredentialMeta>("/v1/_credentialdef", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ op: "create", ...input }),
  });
}

export function deleteCredential(input: {
  scope: CredentialScope;
  name: string;
}): Promise<{ name: string; scope: string; deleted: boolean }> {
  return jsonFetch("/v1/_credentialdef", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ op: "delete", ...input }),
  });
}

// --- RFC AV: token-usage & cost report (GET /v1/_usage) ---

// UsageAggregate is one grouped row. Only the dimensions in the query's
// group_by are populated; the rest are "".
export interface UsageAggregate {
  tenant_id?: string;
  user_id?: string;
  provider?: string;
  model?: string;
  credential_source?: string; // operator | tenant | user
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

export interface UsageReportParams {
  group_by?: string; // comma list of tenant,user,provider,model,source
  from?: string; // RFC3339
  to?: string; // RFC3339
  tenant?: string; // admin-only focus; ignored for a tenant operator
}

export function getUsage(
  params: UsageReportParams = {}
): Promise<UsageReportResponse> {
  const q = new URLSearchParams();
  if (params.group_by) q.set("group_by", params.group_by);
  if (params.from) q.set("from", params.from);
  if (params.to) q.set("to", params.to);
  if (params.tenant) q.set("tenant", params.tenant);
  const qs = q.toString();
  return jsonFetch<UsageReportResponse>(`/v1/_usage${qs ? "?" + qs : ""}`);
}

// --- RFC AW: per-scope token budgets (GET/PUT/DELETE /v1/_limits) ---

// TokenLimit is one token_limits row plus its live month-to-date usage
// (mirrors limitRowResponse). soft_limit/hard_limit are number|null — null
// (the wire omits an unset tier) means "no ceiling on that severity". `used`
// is the scope's calendar-month token total. Tenant-scoped server-side: a
// substrate:tenant operator sees only its own tenant + users; admin sees all
// (or one tenant via ?tenant=).
export interface TokenLimit {
  tenant_id?: string;
  scope: string; // operator | tenant | user
  scope_id?: string; // tenant id / user subject; "" for operator-global
  soft_limit: number | null;
  hard_limit: number | null;
  used: number;
  updated_at?: string;
  updated_by?: string;
}

export interface LimitsResponse {
  limits: TokenLimit[];
}

// listLimits fetches the budgets visible to the caller. tenant is the admin-
// only focus (?tenant=); ignored server-side for a tenant operator.
export function listLimits(tenant?: string): Promise<LimitsResponse> {
  const q = tenant ? `?tenant=${encodeURIComponent(tenant)}` : "";
  return jsonFetch<LimitsResponse>(`/v1/_limits${q}`);
}

// LimitPutBody upserts one budget row. Send a number to set a tier, or null /
// omit to leave it unset. tenant_id addresses a tenant (admin only); a tenant
// operator's tenant is stamped from the principal server-side, and the
// operator scope + any cross-tenant write is refused (403) for a tenant
// operator. scope_id is the user subject (scope=user); it must be empty for
// scope=tenant and scope=operator.
export interface LimitPutBody {
  tenant_id?: string;
  scope: string;
  scope_id?: string;
  soft_limit?: number | null;
  hard_limit?: number | null;
}

export function putLimit(body: LimitPutBody): Promise<TokenLimit> {
  return jsonFetch<TokenLimit>("/v1/_limits", {
    method: "PUT",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
  });
}

// deleteLimit removes a budget row (→ unlimited again). 204 No Content, so it
// uses a raw fetch (jsonFetch would choke parsing the empty body) — mirrors
// deleteSnapshot. tenant is the admin-only focus (ignored for a tenant op).
export async function deleteLimit(
  scope: string,
  scopeId?: string,
  tenant?: string,
): Promise<void> {
  const q = new URLSearchParams();
  q.set("scope", scope);
  if (scopeId) q.set("scope_id", scopeId);
  if (tenant) q.set("tenant", tenant);
  const resp = await fetch(baseURL + `/v1/_limits?${q.toString()}`, {
    method: "DELETE",
    credentials: "same-origin",
  });
  if (!resp.ok) {
    if (redirectToLoginOn401(resp.status)) return;
    const body = await resp.text();
    throw new Error(`${resp.status} ${resp.statusText}: ${body.slice(0, 200)}`);
  }
}
