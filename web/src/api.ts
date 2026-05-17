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

// TranscriptEvent is one persisted row from
// /v1/sessions/{id}/transcript. The server wraps each providers.Event
// in {seq, run_id, ts_ns, type, event:{...}} — the type field at the
// top level mirrors event.type so consumers can filter without
// digging into the payload, but the actual content lives under
// `event`.
export interface TranscriptEvent {
  seq: number;
  run_id: string;
  ts_ns: number;
  type: string;
  event: EventPayload;
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
