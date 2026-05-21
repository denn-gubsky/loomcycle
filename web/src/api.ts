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

export function listUsers(): Promise<ListUsersResponse> {
  return jsonFetch<ListUsersResponse>("/v1/_users");
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
    const body = await resp.text();
    throw new Error(`${resp.status} ${resp.statusText}: ${body.slice(0, 200)}`);
  }
  return resp.json() as Promise<T>;
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

export function listAgents(userId: string, status?: string): Promise<ListAgentsResponse> {
  const q = status ? `?status=${encodeURIComponent(status)}` : "";
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
): Promise<MemoryEntriesResponse> {
  const params = new URLSearchParams();
  if (prefix) params.set("prefix", prefix);
  if (limit) params.set("limit", String(limit));
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
