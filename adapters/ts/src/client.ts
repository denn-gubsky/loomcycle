/**
 * LoomcycleClient — the single public class exported by
 * @loomcycle/client. Speaks HTTP+SSE to a running loomcycle sidecar.
 *
 * hooks-connector PR C: full Python-adapter parity + hook management.
 * 27 methods total — 26 async (run streaming, continuation, agent
 * metadata, transcript, health, users, pause/resume/state, snapshot
 * lifecycle capture / list / get / restore / delete, memory admin,
 * interruption listing + resolve, hook registration / list / delete)
 * plus one synchronous helper (exportSnapshotURL builds a URL string
 * without issuing a request).
 *
 * Construction:
 *
 *   const client = new LoomcycleClient({
 *     baseUrl: "http://127.0.0.1:8787",  // or process.env.LOOMCYCLE_BASE_URL
 *     authToken: "...",                  // or process.env.LOOMCYCLE_AUTH_TOKEN
 *   });
 *
 * Streaming methods (`runStreaming`, `continueSession`) return
 * AsyncIterable<AgentEvent>; non-streaming methods return
 * Promise<T>. Non-2xx responses throw typed errors from errors.ts
 * via fetch-helpers.ts:raiseFromResponse — see README.md for the
 * full mapping table.
 */

import type { _FetchContext } from "./fetch-helpers.js";
import {
  deleteRequest,
  jsonFetch,
  postJSON,
  raiseFromResponse,
} from "./fetch-helpers.js";
import { parseSSE } from "./stream.js";
import type {
  Agent,
  AgentEvent,
  AgentStatus,
  CancelAgentResult,
  ClientOptions,
  ContinueOptions,
  CreateSnapshotOptions,
  HealthResponse,
  Hook,
  InterruptListResponse,
  InterruptStatus,
  ListAgentsResponse,
  ListHooksResponse,
  ListUsersResponse,
  MemoryEntriesResponse,
  MemoryEntryResponse,
  MemoryScopeIDsResponse,
  MemoryScopesResponse,
  PauseResult,
  RegisterHookOptions,
  RegisterHookResponse,
  ResolveInterruptOptions,
  ResumeResult,
  RunOptions,
  RuntimeStateResponse,
  SnapshotCreateResponse,
  SnapshotDescriptor,
  SnapshotEnvelope,
  SnapshotListResponse,
  SnapshotRestoreResponse,
  TranscriptResponse,
} from "./types.js";

export class LoomcycleClient {
  private ctx: _FetchContext;

  constructor(opts: ClientOptions = {}) {
    this.ctx = {
      baseUrl: (opts.baseUrl ?? "http://127.0.0.1:8787").replace(/\/$/, ""),
      authToken: opts.authToken,
      fetchImpl: opts.fetch ?? fetch,
    };
  }

  // ---- Run lifecycle ----

  /**
   * Run an agent and stream events. Returns AsyncIterable<AgentEvent>;
   * the iterator completes when the server closes the SSE stream.
   *
   * Errors during the run surface as `{ type: "error", error }` events;
   * only transport / HTTP-level failures throw — and those throw typed
   * errors (e.g. AuthError for 401, BackpressureError for 429).
   */
  async *runStreaming(opts: RunOptions): AsyncIterable<AgentEvent> {
    yield* this.streamSSE("/v1/runs", {
      agent: opts.agent,
      segments: opts.segments,
      allowed_tools: opts.allowedTools,
    }, opts.signal);
  }

  /**
   * Continue an existing session with a new run. The session's prior
   * transcript is replayed into the model's context server-side;
   * this iterator yields only the NEW events from the continuation.
   *
   * Raises SessionNotFoundError when sessionId is unknown,
   * SessionBusyError when another request is in flight on the same
   * session.
   */
  async *continueSession(opts: ContinueOptions): AsyncIterable<AgentEvent> {
    yield* this.streamSSE(
      `/v1/sessions/${encodeURIComponent(opts.sessionId)}/messages`,
      {
        segments: opts.segments,
        allowed_tools: opts.allowedTools,
        ...(opts.agentId ? { agent_id: opts.agentId } : {}),
      },
      opts.signal,
    );
  }

  // ---- Agent metadata ----

  /** Read one agent's status + usage stats. Raises AgentNotFoundError
   *  when the agent_id is unknown. */
  async getAgent(agentId: string, opts?: { signal?: AbortSignal }): Promise<Agent> {
    return jsonFetch<Agent>(this.ctx, `/v1/agents/${encodeURIComponent(agentId)}`, opts);
  }

  /** Cancel a live agent (cascades to children via parent_agent_id).
   *  Returns count of agents cancelled. Idempotent — already-terminated
   *  agents return 0. */
  async cancelAgent(
    agentId: string,
    opts?: { reason?: string; signal?: AbortSignal },
  ): Promise<CancelAgentResult> {
    const resp = await postJSON<{ cancelled_count: number }>(
      this.ctx,
      `/v1/agents/${encodeURIComponent(agentId)}/cancel`,
      { reason: opts?.reason ?? "" },
      opts,
    );
    return { cancelledCount: resp.cancelled_count };
  }

  /** List a user's recent agent runs, optionally filtered by status. */
  async listUserAgents(
    userId: string,
    opts?: { status?: AgentStatus; signal?: AbortSignal },
  ): Promise<Agent[]> {
    const q = opts?.status ? `?status=${encodeURIComponent(opts.status)}` : "";
    const resp = await jsonFetch<ListAgentsResponse>(
      this.ctx,
      `/v1/users/${encodeURIComponent(userId)}/agents${q}`,
      opts,
    );
    return resp.agents ?? [];
  }

  /** Read the full event log for a session. Each entry has seq,
   *  run_id, ts_ns, type, event (the providers.Event payload). */
  async getTranscript(
    sessionId: string,
    opts?: { signal?: AbortSignal },
  ): Promise<TranscriptResponse> {
    return jsonFetch<TranscriptResponse>(
      this.ctx,
      `/v1/sessions/${encodeURIComponent(sessionId)}/transcript`,
      opts,
    );
  }

  /** Liveness probe. Unauthenticated. Returns build info + uptime.
   *  Hits /healthz, not /v1/. */
  async health(opts?: { signal?: AbortSignal }): Promise<HealthResponse> {
    return jsonFetch<HealthResponse>(this.ctx, "/healthz", opts);
  }

  /** Admin: list known users with running-count summary. Drives the
   *  Web UI's user picker; operators with bearer auth can call too. */
  async listUsers(opts?: { signal?: AbortSignal }): Promise<ListUsersResponse> {
    return jsonFetch<ListUsersResponse>(this.ctx, "/v1/_users", opts);
  }

  // ---- v0.8.17/8.18 Pause / Resume / State ----

  /** Quiesce the runtime. Idempotent tools cancel immediately;
   *  non-idempotent + external tools get a grace window then
   *  force-cancel. Raises AlreadyPausingError on 409,
   *  PauseNotConfiguredError on 503. */
  async pauseRuntime(opts?: {
    timeoutMs?: number;
    signal?: AbortSignal;
  }): Promise<PauseResult> {
    const body =
      opts?.timeoutMs && opts.timeoutMs > 0
        ? { timeout_ms: opts.timeoutMs }
        : undefined;
    return postJSON<PauseResult>(this.ctx, "/v1/_pause", body, opts);
  }

  /** Release the runtime quiesce. Raises NotPausedError on 409. */
  async resumeRuntime(opts?: { signal?: AbortSignal }): Promise<ResumeResult> {
    return postJSON<ResumeResult>(this.ctx, "/v1/_resume", undefined, opts);
  }

  /** Current runtime state. Cheap query — atomic state + a
   *  bounded snapshots count. */
  async getRuntimeState(opts?: {
    signal?: AbortSignal;
  }): Promise<RuntimeStateResponse> {
    return jsonFetch<RuntimeStateResponse>(this.ctx, "/v1/_state", opts);
  }

  // ---- Snapshot lifecycle ----

  /** Capture running-state into a per-section-semver JSON envelope.
   *  Raises SnapshotTooLargeError on 413 when the envelope exceeds
   *  LOOMCYCLE_SNAPSHOT_MAX_BYTES (default 512 MiB). */
  async createSnapshot(
    opts?: CreateSnapshotOptions & { signal?: AbortSignal },
  ): Promise<SnapshotCreateResponse> {
    const body: Record<string, unknown> = {};
    if (opts?.label) body.label = opts.label;
    if (opts?.includeHistory) body.include_history = true;
    if (opts?.includeHistorySince)
      body.include_history_since = opts.includeHistorySince;
    if (opts?.maxBytes && opts.maxBytes > 0) body.max_bytes = opts.maxBytes;
    return postJSON<SnapshotCreateResponse>(this.ctx, "/v1/_snapshots", body, opts);
  }

  /** List captured snapshots (most-recent first). Capped at 200
   *  server-side; the limit param defaults to 200 too. */
  async listSnapshots(opts?: {
    limit?: number;
    labelContains?: string;
    signal?: AbortSignal;
  }): Promise<SnapshotDescriptor[]> {
    const params = new URLSearchParams();
    if (opts?.limit && opts.limit > 0) params.set("limit", String(opts.limit));
    if (opts?.labelContains)
      params.set("label_contains", opts.labelContains);
    const qs = params.toString();
    const path = qs ? `/v1/_snapshots?${qs}` : "/v1/_snapshots";
    const resp = await jsonFetch<SnapshotListResponse>(this.ctx, path, opts);
    return resp.entries ?? [];
  }

  /** Fetch the full snapshot envelope including JSON content.
   *  Distinct from exportSnapshot (which is operator-facing
   *  "where did this land on the host" semantics with a download
   *  URL). Raises SnapshotNotFoundError on 404. */
  async getSnapshot(
    snapshotId: string,
    opts?: { signal?: AbortSignal },
  ): Promise<SnapshotEnvelope> {
    return jsonFetch<SnapshotEnvelope>(
      this.ctx,
      `/v1/_snapshots/${encodeURIComponent(snapshotId)}`,
      opts,
    );
  }

  /** Returns the URL of the snapshot's canonical envelope —
   *  synchronous and side-effect-free; does NOT issue an HTTP
   *  request. The endpoint is bearer-authed like every other
   *  `/v1/_snapshots/*` route, so callers must attach the same
   *  `Authorization: Bearer <token>` header when fetching this
   *  URL (e.g. `curl -H "Authorization: Bearer $TOKEN" ...`).
   *  There is no token query-param fallback. */
  exportSnapshotURL(snapshotId: string): string {
    return `${this.ctx.baseUrl}/v1/_snapshots/${encodeURIComponent(snapshotId)}/export`;
  }

  /** Restore from a same-instance snapshot id OR an inline
   *  envelope JSON. Idempotent: ON CONFLICT DO NOTHING per row;
   *  the returned counters reflect rows actually written.
   *  Raises SnapshotVersionError on 422 when a section's
   *  declared version is newer than the reader supports. */
  async restoreSnapshot(opts: {
    snapshotId?: string;
    /** Pass a parsed JSON object (e.g. `getSnapshot.json_content` or
     *  the result of JSON.parse on an exported envelope). */
    json?: unknown;
    includeHistory?: boolean;
    signal?: AbortSignal;
  }): Promise<SnapshotRestoreResponse> {
    if (!opts.snapshotId && opts.json === undefined) {
      // Client-side validation — match Python adapter's
      // InvalidArgumentError pattern but the typed-error layer
      // lives in errors.ts; for a thrown plain error here the
      // method's catchers just see the message.
      throw new Error(
        "restoreSnapshot: pass snapshotId or json (one is required)",
      );
    }
    if (opts.snapshotId && opts.json !== undefined) {
      throw new Error(
        "restoreSnapshot: pass only one of snapshotId or json",
      );
    }
    // When json is supplied the id path-segment is ignored
    // server-side; we use a placeholder "inline" segment to keep
    // the URL well-formed.
    const id = opts.snapshotId ?? "inline";
    const body: Record<string, unknown> = {};
    if (opts.includeHistory) body.include_history = true;
    if (opts.json !== undefined) body.json = opts.json;
    return postJSON<SnapshotRestoreResponse>(
      this.ctx,
      `/v1/_snapshots/${encodeURIComponent(id)}/restore`,
      body,
      opts,
    );
  }

  /** Delete a snapshot. Idempotent — succeeds whether or not the
   *  row existed (server returns 204 in both cases). */
  async deleteSnapshot(
    snapshotId: string,
    opts?: { signal?: AbortSignal },
  ): Promise<void> {
    await deleteRequest(
      this.ctx,
      `/v1/_snapshots/${encodeURIComponent(snapshotId)}`,
      opts,
    );
  }

  // ---- Memory admin ----

  /** List the kinds of memory scopes the server knows about
   *  (agent, user — or whatever the operator yaml declares). */
  async listMemoryScopes(opts?: {
    signal?: AbortSignal;
  }): Promise<MemoryScopesResponse> {
    return jsonFetch<MemoryScopesResponse>(this.ctx, "/v1/_memory/scopes", opts);
  }

  /** List the scope_ids that have at least one memory row under
   *  a given scope. */
  async listMemoryScopeIDs(
    scope: string,
    opts?: { signal?: AbortSignal },
  ): Promise<MemoryScopeIDsResponse> {
    return jsonFetch<MemoryScopeIDsResponse>(
      this.ctx,
      `/v1/_memory/scopes/${encodeURIComponent(scope)}`,
      opts,
    );
  }

  /** List memory entries under a (scope, scope_id) tuple.
   *  Optional prefix narrows by key prefix. */
  async listMemoryEntries(
    scope: string,
    scopeID: string,
    opts?: { prefix?: string; limit?: number; signal?: AbortSignal },
  ): Promise<MemoryEntriesResponse> {
    const params = new URLSearchParams();
    if (opts?.prefix) params.set("prefix", opts.prefix);
    // Guard against `limit: 0` (falsy but valid-looking) and negatives —
    // both would either send `limit=0` (server treats as default but the
    // semantic is unclear) or `limit=-N` (server rejects). Only send the
    // param when the caller passed a meaningful positive number.
    if (opts?.limit && opts.limit > 0) params.set("limit", String(opts.limit));
    const qs = params.toString();
    const path = `/v1/_memory/scopes/${encodeURIComponent(scope)}/${encodeURIComponent(scopeID)}/keys${qs ? "?" + qs : ""}`;
    return jsonFetch<MemoryEntriesResponse>(this.ctx, path, opts);
  }

  /** Read a single memory entry by (scope, scope_id, key). */
  async getMemoryEntry(
    scope: string,
    scopeID: string,
    key: string,
    opts?: { signal?: AbortSignal },
  ): Promise<MemoryEntryResponse> {
    return jsonFetch<MemoryEntryResponse>(
      this.ctx,
      `/v1/_memory/scopes/${encodeURIComponent(scope)}/${encodeURIComponent(scopeID)}/keys/${encodeURIComponent(key)}`,
      opts,
    );
  }

  // ---- Interruption ----

  /** List interrupts addressable to a user_id. Default filter is
   *  status=pending. */
  async listUserInterrupts(
    userId: string,
    opts?: { status?: InterruptStatus; signal?: AbortSignal },
  ): Promise<InterruptListResponse> {
    const status = opts?.status ?? "pending";
    return jsonFetch<InterruptListResponse>(
      this.ctx,
      `/v1/users/${encodeURIComponent(userId)}/interrupts?status=${encodeURIComponent(status)}`,
      opts,
    );
  }

  /** List interrupts emitted by a specific run. */
  async listRunInterrupts(
    runId: string,
    opts?: { status?: InterruptStatus; signal?: AbortSignal },
  ): Promise<InterruptListResponse> {
    const status = opts?.status ?? "pending";
    return jsonFetch<InterruptListResponse>(
      this.ctx,
      `/v1/runs/${encodeURIComponent(runId)}/interrupts?status=${encodeURIComponent(status)}`,
      opts,
    );
  }

  /** Resolve a pending Interruption.ask from outside the agent
   *  loop. Lets a TS-side dashboard or service act as the human
   *  answerer when operator yaml configures the consumer-MCP
   *  backend. */
  async resolveInterrupt(
    runId: string,
    interruptId: string,
    opts: ResolveInterruptOptions & { signal?: AbortSignal },
  ): Promise<unknown> {
    return postJSON<unknown>(
      this.ctx,
      `/v1/runs/${encodeURIComponent(runId)}/interrupts/${encodeURIComponent(interruptId)}/resolve`,
      {
        kind: opts.kind ?? "question",
        answer: opts.answer,
        resolved_by: opts.resolvedBy ?? "client",
      },
      opts,
    );
  }

  // ---- Hook management (hooks-connector series, PR C) ----

  /** Register a pre- or post-tool webhook. The callback_url must be
   *  an http:// or https:// endpoint the CONSUMER runs — loomcycle
   *  POSTs PreHookCall / PostHookCall payloads to it. This method
   *  manages registration only; the receiver is the consumer's own
   *  HTTP framework (Express, Next.js, etc.).
   *
   *  Re-registering the same (owner, name) replaces the prior entry
   *  with a fresh id (idempotent app-restart contract).
   *
   *  Raises InvalidArgumentError on 400 (bad URL / phase / missing
   *  required fields). */
  async registerHook(
    opts: RegisterHookOptions & { signal?: AbortSignal },
  ): Promise<RegisterHookResponse> {
    const body: Record<string, unknown> = {
      owner: opts.owner,
      name: opts.name,
      phase: opts.phase,
      callback_url: opts.callbackUrl,
    };
    if (opts.agents !== undefined) body.agents = opts.agents;
    if (opts.tools !== undefined) body.tools = opts.tools;
    if (opts.failMode !== undefined) body.fail_mode = opts.failMode;
    if (opts.timeoutMs !== undefined && opts.timeoutMs > 0) {
      body.timeout_ms = opts.timeoutMs;
    }
    return postJSON<RegisterHookResponse>(this.ctx, "/v1/hooks", body, opts);
  }

  /** List every currently-registered hook. Returns the array
   *  unwrapped (the wire envelope is `{hooks: [...]}` — we strip
   *  the envelope to match listUserAgents). In-memory only — empty
   *  after a loomcycle restart. */
  async listHooks(opts?: { signal?: AbortSignal }): Promise<Hook[]> {
    const resp = await jsonFetch<ListHooksResponse>(this.ctx, "/v1/hooks", opts);
    return resp.hooks ?? [];
  }

  /** Delete a registered hook by id. Raises HookNotFoundError on
   *  404. Returns void on success (the HTTP 200 body `{deleted: id}`
   *  is dropped — callers already know the id they passed). */
  async deleteHook(id: string, opts?: { signal?: AbortSignal }): Promise<void> {
    await deleteRequest(
      this.ctx,
      `/v1/hooks/${encodeURIComponent(id)}`,
      opts,
    );
  }

  // ---- Internal helpers ----

  /** Shared SSE POST → stream-of-AgentEvent path. Used by
   *  runStreaming + continueSession. */
  private async *streamSSE(
    path: string,
    body: Record<string, unknown>,
    signal?: AbortSignal,
  ): AsyncIterable<AgentEvent> {
    const headers: Record<string, string> = {
      "Content-Type": "application/json",
      // Accept BOTH text/event-stream (the success path) AND
      // application/json (the error path — non-2xx responses come
      // back as JSON so raiseFromResponse can extract typed errors).
      // Per the Streamable HTTP spec; strict reverse proxies in
      // front of the sidecar 406 otherwise. Same rationale as the
      // v0.8.x MCP HTTP-transport hardening note in CLAUDE.md.
      Accept: "text/event-stream, application/json",
    };
    if (this.ctx.authToken) headers.Authorization = `Bearer ${this.ctx.authToken}`;

    const resp = await this.ctx.fetchImpl(this.ctx.baseUrl + path, {
      method: "POST",
      headers,
      body: JSON.stringify(body),
      signal,
    });

    if (!resp.ok) {
      await raiseFromResponse(resp);
    }
    if (!resp.body) {
      throw new Error("loomcycle: response has no body");
    }

    yield* parseSSE(resp.body.getReader());
  }
}
