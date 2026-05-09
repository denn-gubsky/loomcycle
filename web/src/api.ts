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
