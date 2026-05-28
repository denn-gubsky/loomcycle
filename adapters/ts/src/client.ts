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
  authHeaders,
  deleteRequest,
  jsonFetch,
  patchJSON,
  postJSON,
  putJSON,
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
  AckChannelOptions,
  ChannelAckResult,
  ChannelDescriptor,
  ChannelPeekResult,
  ChannelPublishResult,
  ChannelSubscribeResult,
  CreateChannelOptions,
  ListChannelsResponse,
  PeekChannelOptions,
  PublishChannelOptions,
  SetMemoryEntryOptions,
  SetMemoryEntryResponse,
  SubscribeChannelOptions,
  UpdateChannelOptions,
  LibraryAgentDefinition,
  LibraryListResponse,
  LibraryMcpServerDefinition,
  LibrarySkillDefinition,
  ListHooksResponse,
  ListUsersResponse,
  LLMChatOptions,
  LLMChatResponse,
  LLMChatStreamItem,
  LLMEmbeddingsOptions,
  LLMEmbeddingsResponse,
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
  RunStateStreamItem,
  RuntimeStateResponse,
  SnapshotCreateResponse,
  SnapshotDescriptor,
  SnapshotEnvelope,
  SnapshotListResponse,
  SnapshotRestoreResponse,
  StreamUserRunStatesOptions,
  SubstrateToolInput,
  SubstrateToolResponse,
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
   *
   * **Blocking semantics.** This iterator is alive for the FULL
   * duration of the run — typically seconds, occasionally minutes for
   * long tool chains. Callers that need fire-and-forget completion
   * notifications (n8n's worker model, dashboards that don't want to
   * hold a connection per active run) should subscribe to
   * {@link LoomcycleClient.streamUserRunStates} instead, which yields
   * one terminal-state frame per completed run without holding the
   * run's stream open.
   *
   * v0.9.x — pass `opts.debug = true` to emit synthetic
   * `{ type: "_meta", meta_subtype: "stream_open" | "stream_close" }`
   * events around the real frames. Silent (default) when omitted.
   */
  async *runStreaming(opts: RunOptions): AsyncIterable<AgentEvent> {
    // Build the body conditionally so omitted fields stay off the wire.
    // The pointer-vs-empty distinction on allowed_hosts is preserved by
    // treating `null` as "omit" — same as the server's nil semantics —
    // so callers threading a possibly-unset slice don't accidentally
    // send `allowed_hosts: null` (which JSON-decodes to a deny-all on
    // some implementations).
    const body: Record<string, unknown> = {
      agent: opts.agent,
      segments: opts.segments,
    };
    if (opts.allowedTools !== undefined) body.allowed_tools = opts.allowedTools;
    if (opts.allowedHosts !== undefined && opts.allowedHosts !== null) {
      body.allowed_hosts = opts.allowedHosts;
    }
    if (opts.webSearchFilter !== undefined) body.web_search_filter = opts.webSearchFilter;
    if (opts.sessionId !== undefined) body.session_id = opts.sessionId;
    if (opts.tenantId !== undefined) body.tenant_id = opts.tenantId;
    if (opts.userId !== undefined) body.user_id = opts.userId;
    if (opts.agentId !== undefined) body.agent_id = opts.agentId;
    if (opts.userTier !== undefined) body.user_tier = opts.userTier;
    if (opts.userBearer !== undefined) body.user_bearer = opts.userBearer;
    if (opts.userCredentials !== undefined) body.user_credentials = opts.userCredentials;
    yield* this.streamSSE("/v1/runs", body, opts.signal, opts.debug);
  }

  /**
   * Continue an existing session with a new run. The session's prior
   * transcript is replayed into the model's context server-side;
   * this iterator yields only the NEW events from the continuation.
   *
   * Raises SessionNotFoundError when sessionId is unknown,
   * SessionBusyError when another request is in flight on the same
   * session.
   *
   * **Blocking semantics.** Same as {@link LoomcycleClient.runStreaming} —
   * the iterator stays alive for the duration of the new run. For
   * async fire-and-forget completion patterns, see
   * {@link LoomcycleClient.streamUserRunStates}.
   *
   * v0.9.x — pass `opts.debug = true` for synthetic
   * `_meta` open/close events.
   */
  async *continueSession(opts: ContinueOptions): AsyncIterable<AgentEvent> {
    const body: Record<string, unknown> = {
      segments: opts.segments,
    };
    if (opts.allowedTools !== undefined) body.allowed_tools = opts.allowedTools;
    if (opts.allowedHosts !== undefined && opts.allowedHosts !== null) {
      body.allowed_hosts = opts.allowedHosts;
    }
    if (opts.webSearchFilter !== undefined) body.web_search_filter = opts.webSearchFilter;
    if (opts.agentId !== undefined) body.agent_id = opts.agentId;
    if (opts.userTier !== undefined) body.user_tier = opts.userTier;
    if (opts.userBearer !== undefined) body.user_bearer = opts.userBearer;
    if (opts.userCredentials !== undefined) body.user_credentials = opts.userCredentials;
    yield* this.streamSSE(
      `/v1/sessions/${encodeURIComponent(opts.sessionId)}/messages`,
      body,
      opts.signal,
      opts.debug,
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

  /** List a user's recent agent runs, optionally filtered by status.
   *
   *  v0.9.x — `parentAgentId` narrows the result CLIENT-SIDE to runs
   *  whose `parent_agent_id` matches. The server still returns the
   *  full set (server-side `?parent_agent_id=` filter is a future
   *  request); the adapter trims before returning. Useful for the
   *  n8n trigger pattern "show me all sub-runs spawned by parent X." */
  async listUserAgents(
    userId: string,
    opts?: {
      status?: AgentStatus;
      parentAgentId?: string;
      signal?: AbortSignal;
    },
  ): Promise<Agent[]> {
    const q = opts?.status ? `?status=${encodeURIComponent(opts.status)}` : "";
    const resp = await jsonFetch<ListAgentsResponse>(
      this.ctx,
      `/v1/users/${encodeURIComponent(userId)}/agents${q}`,
      opts,
    );
    const all = resp.agents ?? [];
    if (opts?.parentAgentId !== undefined && opts.parentAgentId !== "") {
      return all.filter((a) => a.parent_agent_id === opts.parentAgentId);
    }
    return all;
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

  // ---- v0.8.22 substrate admin (AgentDef + SkillDef) ----

  /** Invoke the AgentDef substrate tool over HTTP. Mirrors the
   *  MCP `agentdef` meta-tool and the in-band agent tool_use of
   *  the same name — different transport, identical semantics.
   *
   *  The `input.op` field discriminates create / fork / get /
   *  list / promote / retire. The remaining fields are op-specific;
   *  see the in-process tool's documentation.
   *
   *  Raises {@link SubstrateToolRefusedError} when the tool itself
   *  refuses the call (scope deny, empty body, allowed-tools
   *  widening, etc.) — distinct from transport failures so callers
   *  can branch on the typed error class.
   *
   *  Raises {@link InvalidArgumentError} on 400 (malformed JSON
   *  body); {@link AuthError} on 401; {@link UnavailableError} on
   *  503 (store / connector unwired). */
  async agentDef(
    input: SubstrateToolInput,
    opts?: { signal?: AbortSignal },
  ): Promise<SubstrateToolResponse> {
    return postJSON<SubstrateToolResponse>(this.ctx, "/v1/_agentdef", input, opts);
  }

  /** Invoke the SkillDef substrate tool over HTTP. Mirror of
   *  {@link LoomcycleClient.agentDef} for skills (v0.8.22+). Same
   *  input grammar, same error class on refusal. See the
   *  agentDef() doc for the full shape and error contract. */
  async skillDef(
    input: SubstrateToolInput,
    opts?: { signal?: AbortSignal },
  ): Promise<SubstrateToolResponse> {
    return postJSON<SubstrateToolResponse>(this.ctx, "/v1/_skilldef", input, opts);
  }

  /** Invoke the v0.9.x MCPServerDef substrate tool over HTTP.
   *  Dynamic MCP server registration — register an HTTP /
   *  Streamable-HTTP MCP server at runtime so its tools become
   *  callable from any agent's `allowed_tools` list without a yaml
   *  edit + restart.
   *
   *  Operator-admin-only: this endpoint requires the bearer token.
   *
   *  Op-discriminated input: `{op: "create" | "fork" | "get" | "list"
   *  | "promote" | "retire" | "rediscover" | "verify", ...}`. Returns
   *  shape varies — narrow with {@link MCPServerDefRowResponse} for
   *  create/fork/get/list rows, {@link MCPServerDefVerifyResult} for
   *  verify responses.
   *
   *  Hard constraints (substrate refuses these):
   *  - Transport must be `http` or `streamable-http` (stdio stays
   *    yaml-only — dynamic registration doesn't allow process spawn).
   *  - URL hostname must be in LOOMCYCLE_HTTP_HOST_ALLOWLIST (SSRF
   *    defence at the registration boundary).
   *  - Name colliding with a static cfg.MCPServers entry is refused
   *    (yaml is ground truth; use a different name).
   *
   *  Raises {@link SubstrateToolRefusedError} on tool-level refusals
   *  (transport/host/yaml-name); {@link InvalidArgumentError} on 400
   *  (malformed JSON); {@link AuthError} on 401. */
  async mcpServerDef(
    input: SubstrateToolInput,
    opts?: { signal?: AbortSignal },
  ): Promise<SubstrateToolResponse> {
    return postJSON<SubstrateToolResponse>(this.ctx, "/v1/_mcpserverdef", input, opts);
  }

  // ---- v0.10.3 Library v2 enumeration (read-only, merged yaml+substrate) ----

  /** List every agent the runtime knows about — yaml-static + dynamic
   *  AgentDefs merged into one envelope per name. Each entry carries
   *  `source: "static-only" | "dynamic-only" | "both"` so callers can
   *  badge / filter by origin. Static-source entries inline the
   *  yaml-side definition body in `static_definition`; dynamic-only
   *  entries omit it (fetch the active version's body via
   *  `agentDef({op: "get", name})` when needed).
   *
   *  Wraps the v0.9.3 endpoint `GET /v1/_library/agents`. Bearer-authed.
   *  Deterministic alphabetical sort by name.
   *
   *  Typical use: populate an integration's "which agent to dispatch?"
   *  dropdown (n8n, Slack slash commands, custom workflow editors).
   *
   *  Raises {@link AuthError} on 401; {@link UnavailableError} on 503
   *  (store unwired in test configurations). */
  async listLibraryAgents(opts?: {
    signal?: AbortSignal;
  }): Promise<LibraryListResponse<LibraryAgentDefinition>> {
    return jsonFetch<LibraryListResponse<LibraryAgentDefinition>>(
      this.ctx,
      "/v1/_library/agents",
      opts,
    );
  }

  /** List every skill the runtime knows about — bundled skills (Approach
   *  A) + filesystem overlay + dynamic SkillDefs merged into one
   *  envelope per name. See {@link LoomcycleClient.listLibraryAgents}
   *  for the source-tagging contract; the only difference is the
   *  `static_definition` shape ({@link LibrarySkillDefinition}).
   *
   *  Wraps `GET /v1/_library/skills` (v0.9.3). */
  async listLibrarySkills(opts?: {
    signal?: AbortSignal;
  }): Promise<LibraryListResponse<LibrarySkillDefinition>> {
    return jsonFetch<LibraryListResponse<LibrarySkillDefinition>>(
      this.ctx,
      "/v1/_library/skills",
      opts,
    );
  }

  /** List every MCP server the runtime knows about — yaml-declared
   *  servers + dynamic MCPServerDef registrations merged into one
   *  envelope per name. Static entries additionally inline the cached
   *  `discovered_tools` snapshot when the pool inspector is wired
   *  AND the pool has finished init for that server (omitted otherwise
   *  — re-check after the pool finishes).
   *
   *  Wraps `GET /v1/_library/mcp-servers` (v0.9.3). */
  async listLibraryMcpServers(opts?: {
    signal?: AbortSignal;
  }): Promise<LibraryListResponse<LibraryMcpServerDefinition>> {
    return jsonFetch<LibraryListResponse<LibraryMcpServerDefinition>>(
      this.ctx,
      "/v1/_library/mcp-servers",
      opts,
    );
  }

  // ---- v0.11.0 LLM Gateway ----

  /** Non-streaming LLM chat completion via the gateway endpoint.
   *  Wraps `POST /v1/_llm/chat` with `stream: false`. The gateway
   *  resolves a provider per the routing precedence
   *  (explicit-pin > explicit-provider > explicit-model > resolver
   *  default), invokes it directly (no agent loop), and returns the
   *  aggregated response with usage counters + the chosen
   *  provider/model echoed back.
   *
   *  Use this when you want loomcycle's routing benefits (one
   *  credential, one quota, one observability surface across
   *  providers) without paying for the agent runtime overhead.
   *  Tool-calling works: pass `tools[]` and read `tool_use` content
   *  blocks back; the per-provider schema translation is handled by
   *  the substrate's existing driver layer.
   *
   *  Raises {@link AuthError} on 401; {@link UnavailableError} on 503
   *  (resolver not configured, store unwired);
   *  {@link InvalidArgumentError} on 400 (bad request shape). */
  async llmChat(opts: LLMChatOptions): Promise<LLMChatResponse> {
    const body = serializeLLMOptions(opts, false);
    return postJSON<LLMChatResponse>(this.ctx, "/v1/_llm/chat", body, {
      signal: opts.signal,
    });
  }

  /** Streaming LLM chat completion. Wraps `POST /v1/_llm/chat` with
   *  `stream: true`. Yields one {@link LLMChatStreamItem} per SSE
   *  frame in Anthropic-style: provider_chosen first, then
   *  content_block_start / content_block_delta / content_block_stop
   *  pairs, then message_delta + done.
   *
   *  Iteration terminates when the gateway closes the stream. On a
   *  terminal error the gateway emits an `error` frame; the iterator
   *  yields it and the caller decides whether to throw.
   *
   *  Use this for live token streaming into LangChain BaseChatModel
   *  `_stream` callbacks. */
  async *llmStream(opts: LLMChatOptions): AsyncIterable<LLMChatStreamItem> {
    const body = serializeLLMOptions(opts, true);
    const resp = await this.ctx.fetchImpl(this.ctx.baseUrl + "/v1/_llm/chat", {
      method: "POST",
      headers: {
        ...authHeaders(this.ctx),
        "Content-Type": "application/json",
        Accept: "text/event-stream",
      },
      body: JSON.stringify(body),
      signal: opts.signal,
    });
    if (!resp.ok) {
      await raiseFromResponse(resp);
    }
    if (!resp.body) {
      throw new Error("llmStream: response has no body");
    }
    const reader = resp.body.getReader();
    yield* parseLLMStreamFrames(reader);
  }

  // ---- v0.11.4 OpenAI Embeddings compatibility shim ----

  /** Compute embedding vectors via the OpenAI-compatible
   *  `/v1/embeddings` endpoint. Dispatches to the single configured
   *  `providers.Embedder` (the same instance Memory tool uses for
   *  `embed:true` internally). No resolver path, no tier overlay,
   *  no streaming — embeddings are synchronous.
   *
   *  Accepts `input` as a string OR string array. Tokenized inputs
   *  (number arrays) are refused at the server boundary — send text.
   *  `encoding_format: "base64"` packs each float32 little-endian
   *  then base64-encodes (saves ~25% wire bytes on 1536-dim
   *  vectors); "float" (default) emits each vector as a JSON array.
   *
   *  Raises {@link AuthError} on 401; {@link UnavailableError} on
   *  503 (no embedder configured — set memory.embedder.* in
   *  loomcycle.yaml); {@link InvalidArgumentError} on 400 (bad
   *  request shape). */
  async embeddings(opts: LLMEmbeddingsOptions): Promise<LLMEmbeddingsResponse> {
    const { signal: _signal, ...body } = opts;
    return postJSON<LLMEmbeddingsResponse>(this.ctx, "/v1/embeddings", body, {
      signal: opts.signal,
    });
  }

  // ---- Internal helpers ----

  /** Shared SSE POST → stream-of-AgentEvent path. Used by
   *  runStreaming + continueSession.
   *
   *  When `debug` is true, the iterator yields a synthetic
   *  `{ type: "_meta", meta_subtype: "stream_open" }` before any real
   *  events AND a `{ type: "_meta", meta_subtype: "stream_close",
   *  meta_reason }` on EOF / abort / error. The default is silent
   *  (matches pre-v0.9.x behaviour). */
  private async *streamSSE(
    path: string,
    body: Record<string, unknown>,
    signal?: AbortSignal,
    debug?: boolean,
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

    if (!debug) {
      // Silent default — pre-v0.9.x shape.
      yield* parseSSE(resp.body.getReader());
      return;
    }

    // Debug shape: synthetic open + close around the real stream.
    // The open frame carries no meta_reason — the frame itself IS the
    // signal. The close frame's meta_reason distinguishes normal EOF
    // from caller-side abort or a typed-error throw mid-stream.
    //
    // Close is emitted on both paths via try/catch/throw: success path
    // emits AFTER the try block; error path emits INSIDE the catch
    // before re-throwing. NOT a try/finally — the duplication is
    // intentional so the close-then-throw ordering is explicit and
    // a refactor adding `finally` doesn't accidentally double-emit.
    yield { type: "_meta", meta_subtype: "stream_open" };
    let closeReason = "eof";
    try {
      yield* parseSSE(resp.body.getReader());
    } catch (e) {
      // Capture the error type for the close frame, then re-throw so
      // typed-error handling at the consumer site still works.
      closeReason =
        e && typeof e === "object" && "name" in e
          ? String((e as { name: string }).name)
          : "error";
      yield {
        type: "_meta",
        meta_subtype: "stream_close",
        meta_reason: closeReason,
      };
      throw e;
    }
    yield {
      type: "_meta",
      meta_subtype: "stream_close",
      meta_reason: closeReason,
    };
  }

  // ---- v0.9.x n8n RFC Phase 0 ----

  /** List every operator-declared channel with aggregate stats
   *  (message_count, oldest_visible_at, newest_visible_at).
   *  Channels with no published messages still appear with
   *  message_count=0. Orphaned message rows for un-declared channels
   *  also appear (forensic visibility). Mirrors GET /v1/_channels. */
  async listChannels(opts?: {
    signal?: AbortSignal;
  }): Promise<ListChannelsResponse> {
    return await jsonFetch<ListChannelsResponse>(
      this.ctx,
      "/v1/_channels",
      opts,
    );
  }

  // ---- v0.9.x Channel CRUD ----
  //
  // Four bearer-authed ops mirroring the in-band Channel tool's
  // publish/subscribe/peek/ack. Two URL families behind the
  // `scope` field:
  //   - scope: "global" → POST /v1/_channels/{name}/{op} (admin)
  //   - scope: "user"   → POST /v1/users/{userId}/channels/{name}/{op}
  //
  // The same operator bearer token guards both surfaces; the per-user
  // URL embeds the user_id in the path so a caller can't forge a
  // different user_id by lying in the body.
  //
  // Subscribe is a SINGLE-ROUND-TRIP long-poll, not an open stream.
  // For continuous delivery, call `subscribeChannel` in a loop (the
  // n8n trigger node's pattern). Auto-commits the cursor on non-empty
  // batches (at-most-once shape) — use `peekChannel` + explicit
  // `ackChannel` for at-least-once semantics.

  /** Publish a JSON payload to an operator-declared channel. Mirrors
   *  the in-band Channel tool's publish op semantics — including
   *  deferred delivery via `deliverAt` (RFC3339Nano).
   *
   *  Errors:
   *  - {@link NotFoundError} (404) when the channel isn't in operator
   *    yaml. The wire `code` is `channel_not_declared`.
   *  - {@link InvalidArgumentError} (400) on invalid scope / payload.
   *  - {@link AuthError} (401) on bearer mismatch. */
  async publishChannel(
    channel: string,
    opts: PublishChannelOptions,
  ): Promise<ChannelPublishResult> {
    const path = channelOpPath(channel, opts.scope, opts.userId, "publish");
    const body: Record<string, unknown> = { payload: opts.payload };
    if (opts.deliverAt) body.deliver_at = opts.deliverAt;
    return postJSON<ChannelPublishResult>(this.ctx, path, body, {
      signal: opts.signal,
    });
  }

  /** Read the next batch of messages from a channel. Single-round-
   *  trip long-poll: returns immediately if messages are present,
   *  otherwise waits up to `waitMs` for a publish. AUTO-COMMITS the
   *  cursor on a non-empty batch.
   *
   *  For at-least-once delivery (crash safety between "loomcycle
   *  returned the batch" and "consumer finished processing"), use
   *  {@link LoomcycleClient.peekChannel} + an explicit
   *  {@link LoomcycleClient.ackChannel} after durable processing. */
  async subscribeChannel(
    channel: string,
    opts: SubscribeChannelOptions,
  ): Promise<ChannelSubscribeResult> {
    const path = channelOpPath(channel, opts.scope, opts.userId, "subscribe");
    const body: Record<string, unknown> = {};
    if (opts.fromCursor !== undefined) body.from_cursor = opts.fromCursor;
    if (opts.maxMessages !== undefined) body.max_messages = opts.maxMessages;
    if (opts.waitMs !== undefined) body.wait_ms = opts.waitMs;
    return postJSON<ChannelSubscribeResult>(this.ctx, path, body, {
      signal: opts.signal,
    });
  }

  /** Non-destructive read — never advances the committed cursor.
   *  Use for at-least-once consumption patterns: peek, process the
   *  batch durably, then `ackChannel` to advance. Multiple consumers
   *  can peek the same channel without disturbing each other. */
  async peekChannel(
    channel: string,
    opts: PeekChannelOptions,
  ): Promise<ChannelPeekResult> {
    let path = channelOpPath(channel, opts.scope, opts.userId, "peek");
    const params: string[] = [];
    if (opts.fromCursor) params.push(`from_cursor=${encodeURIComponent(opts.fromCursor)}`);
    if (opts.maxMessages) params.push(`max_messages=${opts.maxMessages}`);
    if (params.length > 0) path += `?${params.join("&")}`;
    return jsonFetch<ChannelPeekResult>(this.ctx, path, { signal: opts.signal });
  }

  /** Advance the committed cursor for a (channel, scope, scope_id)
   *  tuple. Cursor must be monotonically forward — older cursors
   *  raise a {@link ConflictError} (HTTP 409, code
   *  `channel_cursor_regression`). */
  async ackChannel(
    channel: string,
    opts: AckChannelOptions,
  ): Promise<ChannelAckResult> {
    const path = channelOpPath(channel, opts.scope, opts.userId, "ack");
    return postJSON<ChannelAckResult>(
      this.ctx,
      path,
      { cursor: opts.cursor },
      { signal: opts.signal },
    );
  }

  // ---- v0.11.5 Channel admin CRUD ----
  //
  // Three bearer-authed ops that mutate the runtime-substrate
  // channel registry. yaml-declared channels are immutable from
  // this surface — the server refuses with HTTP 409 + wire `code`
  // `channel_yaml_immutable`. The TS adapter surfaces that as a
  // {@link LoomcycleError} with status 409.

  /** Create a new runtime-substrate channel. Refuses with HTTP 409
   *  when the name matches a yaml-declared channel (code
   *  `channel_yaml_immutable`) or an existing runtime channel
   *  (code `channel_name_in_use`). */
  async createChannel(opts: CreateChannelOptions): Promise<ChannelDescriptor> {
    const body: Record<string, unknown> = { name: opts.name };
    if (opts.description !== undefined) body.description = opts.description;
    if (opts.scope !== undefined) body.scope = opts.scope;
    if (opts.semantic !== undefined) body.semantic = opts.semantic;
    if (opts.default_ttl !== undefined) body.default_ttl = opts.default_ttl;
    if (opts.max_messages !== undefined) body.max_messages = opts.max_messages;
    if (opts.publisher !== undefined) body.publisher = opts.publisher;
    if (opts.period !== undefined) body.period = opts.period;
    return postJSON<ChannelDescriptor>(this.ctx, "/v1/_channels", body, {
      signal: opts.signal,
    });
  }

  /** Partially update a runtime-substrate channel. Nil-valued fields
   *  in `opts` leave the corresponding attribute unchanged. Refuses
   *  yaml-declared channels with HTTP 409. */
  async updateChannel(
    name: string,
    opts: UpdateChannelOptions,
  ): Promise<ChannelDescriptor> {
    const body: Record<string, unknown> = {};
    if (opts.description !== undefined) body.description = opts.description;
    if (opts.default_ttl !== undefined) body.default_ttl = opts.default_ttl;
    if (opts.max_messages !== undefined) body.max_messages = opts.max_messages;
    if (opts.semantic !== undefined) body.semantic = opts.semantic;
    return patchJSON<ChannelDescriptor>(
      this.ctx,
      `/v1/_channels/${encodeURIComponent(name)}`,
      body,
      { signal: opts.signal },
    );
  }

  /** Delete a runtime-substrate channel + cascade its persisted
   *  messages + cursors. yaml-declared channels refuse with HTTP 409.
   *  Idempotent: deleting a non-existent runtime channel returns a
   *  {@link NotFoundError} so the caller can distinguish that case. */
  async deleteChannel(
    name: string,
    opts?: { signal?: AbortSignal },
  ): Promise<void> {
    return deleteRequest(
      this.ctx,
      `/v1/_channels/${encodeURIComponent(name)}`,
      opts,
    );
  }

  // ---- v0.11.5 Memory entry admin CRUD ----

  /** Idempotently upsert one memory entry by full (scope, scope_id,
   *  key) identifier. PUT semantics — re-writes overwrite the value.
   *  Optional embed flag triggers a synchronous embed via the
   *  operator-configured embedder. */
  async setMemoryEntry(
    scope: string,
    scopeID: string,
    key: string,
    opts: SetMemoryEntryOptions,
  ): Promise<SetMemoryEntryResponse> {
    const body: Record<string, unknown> = { value: opts.value };
    if (opts.embed !== undefined) body.embed = opts.embed;
    if (opts.ttl_seconds !== undefined) body.ttl_seconds = opts.ttl_seconds;
    return putJSON<SetMemoryEntryResponse>(
      this.ctx,
      `/v1/_memory/scopes/${encodeURIComponent(scope)}/${encodeURIComponent(scopeID)}/keys/${encodeURIComponent(key)}`,
      body,
      { signal: opts.signal },
    );
  }

  /** Delete one memory entry by (scope, scope_id, key). Idempotent:
   *  deleting a missing row is a non-error per the in-band Memory
   *  tool's semantics — both surfaces return 204. */
  async deleteMemoryEntry(
    scope: string,
    scopeID: string,
    key: string,
    opts?: { signal?: AbortSignal },
  ): Promise<void> {
    return deleteRequest(
      this.ctx,
      `/v1/_memory/scopes/${encodeURIComponent(scope)}/${encodeURIComponent(scopeID)}/keys/${encodeURIComponent(key)}`,
      opts,
    );
  }

  /** Subscribe to run state transitions for one user_id via SSE.
   *  Yields one `{ kind: "open", ... }` item first (confirms the
   *  connection is live), then one `{ kind: "event", ... }` per
   *  matching state transition until the stream closes.
   *
   *  The stream stays open for at most 30 minutes (server-enforced).
   *  Callers running indefinitely should reconnect on close.
   *
   *  Errors during the stream throw — they do NOT surface as items.
   *  Pass an AbortSignal to terminate cleanly from the consumer side.
   *
   *  v0.9.x options:
   *  - `parentAgentId` — client-side filter: only `kind: "event"`
   *    items whose payload's `parent_agent_id` matches are yielded.
   *    The server still streams every matching event; the adapter
   *    filters before yielding. Empty/omitted = no filter.
   *  - `debug` — when true, an additional `{ kind: "close", payload:
   *    { reason } }` item is yielded when the stream ends (EOF,
   *    abort, or pre-yield error). Useful for n8n nodes that surface
   *    "stream re-opened / closed" log entries without inferring
   *    from timing. Default false. */
  async *streamUserRunStates(
    userId: string,
    opts?: StreamUserRunStatesOptions,
  ): AsyncIterable<RunStateStreamItem> {
    const params = new URLSearchParams();
    if (opts?.statuses && opts.statuses.length > 0) {
      params.set("status", opts.statuses.join(","));
    }
    if (opts?.agent) {
      params.set("agent", opts.agent);
    }
    const qs = params.toString();
    const path =
      `/v1/users/${encodeURIComponent(userId)}/agents/stream` +
      (qs ? `?${qs}` : "");

    const headers: Record<string, string> = {
      Accept: "text/event-stream",
    };
    if (this.ctx.authToken) {
      headers.Authorization = `Bearer ${this.ctx.authToken}`;
    }

    const resp = await this.ctx.fetchImpl(this.ctx.baseUrl + path, {
      method: "GET",
      headers,
      signal: opts?.signal,
    });

    if (!resp.ok) {
      await raiseFromResponse(resp);
    }
    if (!resp.body) {
      throw new Error("loomcycle: streamUserRunStates response has no body");
    }

    const parentFilter = opts?.parentAgentId ?? "";
    const debug = opts?.debug === true;

    let closeReason = "eof";
    try {
      for await (const item of parseRunStateSSE(resp.body.getReader())) {
        // Client-side parent_agent_id filter. Pre-v1 the server has no
        // ?parent_agent_id= query param; n8n-style consumers that need
        // a narrow view get a smaller iterator at the cost of
        // unchanged server load. See StreamUserRunStatesOptions for
        // the trade-off note.
        if (
          parentFilter !== "" &&
          item.kind === "event" &&
          item.payload.parent_agent_id !== parentFilter
        ) {
          continue;
        }
        yield item;
      }
    } catch (e) {
      closeReason =
        e && typeof e === "object" && "name" in e
          ? String((e as { name: string }).name)
          : "error";
      if (debug) {
        yield { kind: "close", payload: { reason: closeReason } };
      }
      throw e;
    }
    if (debug) {
      yield { kind: "close", payload: { reason: closeReason } };
    }
  }
}

/** Lightweight SSE parser tailored to the run-state stream. Each
 *  frame's event name distinguishes the two kinds; data is JSON.
 *  Comment lines (": keepalive") are ignored. */
async function* parseRunStateSSE(
  reader: ReadableStreamDefaultReader<Uint8Array>,
): AsyncIterable<RunStateStreamItem> {
  const decoder = new TextDecoder("utf-8");
  let buf = "";
  let event = "";
  let data = "";

  while (true) {
    const { value, done } = await reader.read();
    if (done) break;
    buf += decoder.decode(value, { stream: true });

    let idx;
    while ((idx = buf.indexOf("\n")) !== -1) {
      const line = buf.slice(0, idx).replace(/\r$/, "");
      buf = buf.slice(idx + 1);

      if (line === "") {
        if (event && data) {
          try {
            const parsed = JSON.parse(data) as unknown;
            if (event === "stream_open") {
              yield {
                kind: "open",
                payload: parsed as RunStateStreamItem extends { kind: "open" }
                  ? RunStateStreamItem["payload"]
                  : never,
              } as RunStateStreamItem;
            } else if (event === "run_state") {
              yield {
                kind: "event",
                payload: parsed as RunStateStreamItem extends { kind: "event" }
                  ? RunStateStreamItem["payload"]
                  : never,
              } as RunStateStreamItem;
            }
          } catch {
            // Drop malformed frame silently — same posture as parseSSE.
          }
        }
        event = "";
        data = "";
        continue;
      }
      if (line.startsWith("event:")) event = line.slice("event:".length).trim();
      else if (line.startsWith("data:")) data = line.slice("data:".length).trim();
    }
  }
}

// channelOpPath builds the v0.9.x Channel CRUD URL. Two families:
//   - scope === "global" → /v1/_channels/{channel}/{op}
//   - scope === "user"   → /v1/users/{userId}/channels/{channel}/{op}
// Channel name is URL-encoded so names containing slashes
// ("findings/alpha", "_system/foo") survive transport.
function channelOpPath(
  channel: string,
  scope: "global" | "user",
  userId: string | undefined,
  op: "publish" | "subscribe" | "peek" | "ack",
): string {
  const enc = encodeURIComponent(channel);
  if (scope === "user") {
    if (!userId) {
      throw new Error(
        `loomcycle: scope="user" requires opts.userId for the channel ${op} call`,
      );
    }
    return `/v1/users/${encodeURIComponent(userId)}/channels/${enc}/${op}`;
  }
  return `/v1/_channels/${enc}/${op}`;
}


// ---- v0.11.0 LLM Gateway helpers ----

/** serializeLLMOptions strips the AbortSignal (transport concern) and
 *  forces the stream flag to match the call mode. */
function serializeLLMOptions(opts: LLMChatOptions, stream: boolean): Record<string, unknown> {
  const { signal: _signal, ...rest } = opts;
  return { ...rest, stream };
}

/** parseLLMStreamFrames drains an SSE stream from /v1/_llm/chat and
 *  yields one LLMChatStreamItem per frame. Unlike parseSSE in
 *  stream.ts, the gateway's frames discriminate purely by the SSE
 *  event name — there's no `type` field on the data payload. */
async function* parseLLMStreamFrames(
  reader: ReadableStreamDefaultReader<Uint8Array>,
): AsyncIterable<LLMChatStreamItem> {
  const decoder = new TextDecoder("utf-8");
  let buf = "";
  let event = "";
  let data = "";

  const flush = (): LLMChatStreamItem | null => {
    if (!event || !data) {
      event = "";
      data = "";
      return null;
    }
    try {
      const payload = JSON.parse(data);
      const item = { kind: event, payload } as LLMChatStreamItem;
      event = "";
      data = "";
      return item;
    } catch {
      event = "";
      data = "";
      return null;
    }
  };

  while (true) {
    const { value, done } = await reader.read();
    if (done) break;
    buf += decoder.decode(value, { stream: true });

    let idx;
    while ((idx = buf.indexOf("\n")) !== -1) {
      const line = buf.slice(0, idx).replace(/\r$/, "");
      buf = buf.slice(idx + 1);

      if (line === "") {
        const item = flush();
        if (item) yield item;
        continue;
      }
      if (line.startsWith("event:")) event = line.slice("event:".length).trim();
      else if (line.startsWith("data:")) data = line.slice("data:".length).trim();
      // Keepalive comment lines (`:keepalive`) are silently dropped.
    }
  }
  // Drain a final un-newlined frame on connection close.
  if (buf.length > 0) {
    const line = buf.replace(/\r$/, "");
    if (line.startsWith("event:")) event = line.slice("event:".length).trim();
    else if (line.startsWith("data:")) data = line.slice("data:".length).trim();
  }
  const item = flush();
  if (item) yield item;
}
