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
  };
  error?: string;
  retry?: { provider?: string; attempt?: number; wait_ms?: number; reason?: string };
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
  | "memorybackenddef";

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
  channel: string;
  id: string;
  deliver_at?: string;
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
