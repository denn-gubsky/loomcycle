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
  deleteJSON,
  jsonFetch,
  patchJSON,
  postJSON,
  putJSON,
  raiseFromResponse,
} from "./fetch-helpers.js";
import { parseSSE } from "./stream.js";
import { InteractiveSession } from "./interactive.js";
import {
  ClientToolHost,
  clientToolsURL,
  type ConnectClientToolsOptions,
} from "./client-tools.js";
import type {
  ChangeFeedError,
  ChangeFeedFrame,
  ChangeFeedStatus,
  MemoryChange,
  StreamChangesOptions,
  CredentialListResponse,
  CredentialMeta,
  CryptoKeyEncoding,
  CryptoKeyExport,
  CryptoKeyExternal,
  CryptoKeyType,
  CryptoRequest,
  CryptoResultByOp,
  DecideRequest,
  DecideResponse,
  DecisionModelsResponse,
  CredentialScope,
  DirectoryInspection,
  DirectoryTenant,
  DirectoryUser,
  ErasureReport,
  ErasureResult,
  Agent,
  AgentEvent,
  AgentList,
  CancelAgentResult,
  ClientOptions,
  AgentDefOverlay,
  AgentDefRowResponse,
  ContinueOptions,
  RunOverrideOptions,
  EffectiveConfigResponse,
  AgentOverridableResponse,
  RetuneRunResponse,
  RunConfigResponse,
  RunPromptResponse,
  CreateSnapshotOptions,
  EnsureCodeAgentOptions,
  EnsureCodeAgentResult,
  EnsureHookDefOptions,
  EnsureHookDefResult,
  HookDefInput,
  HookDefOverlay,
  HookDefRowResponse,
  HookDefVerifyResult,
  CreateDefOptions,
  ForkDefOptions,
  HookDefForkOverlay,
  ScheduleDefInput,
  ScheduleDefOverlay,
  ScheduleDefRowResponse,
  WebhookDefInput,
  WebhookDefOverlay,
  WebhookDefRowResponse,
  EnsureMcpServerOptions,
  EnsureMcpServerResult,
  HealthResponse,
  InterruptListResponse,
  InterruptStatus,
  ListAgentsResponse,
  ListUserAgentsOptions,
  WalkRunsPage,
  AckChannelOptions,
  AwaitChannelsOptions,
  BroadcastChannelsOptions,
  ChannelAckResult,
  ChannelAwaitResult,
  ChannelBroadcastResult,
  ChannelDescriptor,
  ChannelPeekResult,
  ListTeamChannelsOptions,
  PeekTeamChannelOptions,
  ReleaseTeamChannelOptions,
  TeamChannelPeekResult,
  TeamChannelReleaseResult,
  TeamChannelsResult,
  ChannelPublishResult,
  ChannelPurgeResult,
  ChannelReleaseResult,
  ChannelSubscribeResult,
  CreateChannelOptions,
  ListChannelsResponse,
  PeekChannelOptions,
  PublishChannelOptions,
  ReleaseChannelOptions,
  SetMemoryEntryOptions,
  SetMemoryEntryResponse,
  SubscribeChannelOptions,
  UpdateChannelOptions,
  LibraryAgentDefinition,
  LibraryListResponse,
  LibraryMcpServerDefinition,
  LibrarySkillDefinition,
  ListUsersResponse,
  UserRecord,
  CreateUserBody,
  UpdateUserBody,
  MintedUserToken,
  UserTokenMeta,
  ListUserTokensResponse,
  RunnableAgentsResponse,
  LLMChatOptions,
  LLMChatResponse,
  LLMChatStreamItem,
  LLMEmbeddingsOptions,
  LLMEmbeddingsResponse,
  MCPServerDefRowResponse,
  MCPServerDefVerifyResult,
  MemoryEmbedStatsResponse,
  MemoryEntriesResponse,
  MemoryEntryResponse,
  MemoryBackfillResponse,
  MemoryPurgeResponse,
  MemoryReembedResponse,
  MemoryScopeIDsResponse,
  MemoryScopesResponse,
  MemorySearchInput,
  MemorySearchResponse,
  PauseResult,
  RetentionOverride,
  PersistentVolumesResponse,
  EphemeralVolumesResponse,
  ResolveInterruptOptions,
  ResumeResult,
  ResolverMatrix,
  CompactRunResult,
  RunTreeDeletion,
  ReplaySessionResult,
  CancelTurnResult,
  ReviewRunResult,
  CompactionOptions,
  ContextOptions,
  RunBatchOptions,
  RunBatchResult,
  RunOptions,
  ConfiguredRun,
  ConfiguredRunPatch,
  RunStateStreamItem,
  SamplingOptions,
  RuntimeStateResponse,
  SnapshotCreateResponse,
  SnapshotDescriptor,
  SnapshotEnvelope,
  SnapshotListResponse,
  SnapshotRestoreResponse,
  StreamUserRunStatesOptions,
  SubstrateToolInput,
  SubstrateToolResponse,
  CreatedTeam,
  CreateTeamOptions,
  DefNamesResponse,
  ForkTeamOptions,
  ModelAliasesResponse,
  OperatorTokenDefNamesResponse,
  ProviderModelsResponse,
  RoutingResponse,
  ScheduleListResponse,
  SchedulePauseResult,
  ScheduleRunNowResult,
  ScheduleStateView,
  ListTeamsResponse,
  PromotedTeam,
  RetiredTeam,
  TeamBreakpoints,
  TeamDefDetail,
  TeamRunDetached,
  TeamRunTarget,
  TeamVerification,
  TeamDraft,
  TeamDraftVerification,
  TeamVersionList,
  TeamDiagram,
  TeamRunResult,
  PathToolInput,
  PathToolResponse,
  DocumentToolInput,
  DocumentToolResponse,
  HistoryToolInput,
  HistoryToolResponse,
  EvaluationSubmitInput,
  EvaluationGetInput,
  EvaluationListForRunInput,
  EvaluationListForDefInput,
  EvaluationAggregateInput,
  EvaluationInput,
  EvaluationSubmitResult,
  EvaluationRecord,
  EvaluationListForRunResult,
  EvaluationListForDefResult,
  EvaluationAggregate,
  TranscriptResponse,
  WhoamiResponse,
  UsageDimension,
  UsageReportResponse,
  TokenLimit,
  TokenLimitsResponse,
  ConfigResponse,
  SetTokenLimitRequest,
  AclBinding,
  AclBindingListResponse,
  AclCatalog,
  AclDefInput,
  AclDefListResponse,
  AclDefNamesResponse,
  AclDefRow,
  AclRule,
  AclValidateResult,
  AuthzDecision,
  AuthzEvaluationRequest,
  AuthzEvaluationsRequest,
  AuthzEvaluationsResponse,
  AuthzExplainResponse,
  AuthzSimulateRequest,
  AuthzSimulateResponse,
  CreateAclBindingBody,
  CreateGroupBody,
  GroupDetail,
  GroupEffective,
  GroupListResponse,
  GroupMemberChange,
  GroupMemberKind,
  GroupRow,
} from "./types.js";

/** samplingToWire maps the camelCase SamplingOptions to the snake_case wire
 *  object, omitting unset fields so they inherit the agent's value. */
function samplingToWire(s: SamplingOptions): Record<string, unknown> {
  const w: Record<string, unknown> = {};
  if (s.temperature !== undefined) w.temperature = s.temperature;
  if (s.topP !== undefined) w.top_p = s.topP;
  if (s.topK !== undefined) w.top_k = s.topK;
  if (s.frequencyPenalty !== undefined) w.frequency_penalty = s.frequencyPenalty;
  if (s.presencePenalty !== undefined) w.presence_penalty = s.presencePenalty;
  if (s.seed !== undefined) w.seed = s.seed;
  if (s.stop !== undefined) w.stop = s.stop;
  return w;
}

/** compactionToWire maps the camelCase CompactionOptions to the snake_case
 *  wire object, omitting unset fields so they inherit the agent's value. */
function compactionToWire(c: CompactionOptions): Record<string, unknown> {
  const w: Record<string, unknown> = {};
  if (c.enabled !== undefined) w.enabled = c.enabled;
  if (c.targetPercentage !== undefined) w.target_percentage = c.targetPercentage;
  if (c.keepLastN !== undefined) w.keep_last_n = c.keepLastN;
  if (c.keepFirst !== undefined) w.keep_first = c.keepFirst;
  if (c.autocompactAtPct !== undefined) w.autocompact_at_pct = c.autocompactAtPct;
  if (c.model !== undefined) w.model = c.model;
  return w;
}

/** contextToWire maps the camelCase ContextOptions to the snake_case `context`
 *  block the server decodes.
 *
 *  Note `autorecapAtPct` -> `autorecap_at_pct`: the wire key has no underscore
 *  after "auto", unlike compaction's `autocompact_at_pct`. The two blocks are
 *  spelled differently on the wire and a shared helper would have to special-
 *  case one of them, so they stay separate functions. */
function contextToWire(c: ContextOptions): Record<string, unknown> {
  const w: Record<string, unknown> = {};
  if (c.mode !== undefined) w.mode = c.mode;
  if (c.keepLastN !== undefined) w.keep_last_n = c.keepLastN;
  if (c.reasoning !== undefined) w.reasoning = c.reasoning;
  if (c.recapMaxChars !== undefined) w.recap_max_chars = c.recapMaxChars;
  if (c.model !== undefined) w.model = c.model;
  if (c.autorecapAtPct !== undefined) w.autorecap_at_pct = c.autorecapAtPct;
  if (c.stateSchema !== undefined) w.state_schema = c.stateSchema;
  if (c.onInvalidPatch !== undefined) w.on_invalid_patch = c.onInvalidPatch;
  if (c.maxPatchRetries !== undefined) w.max_patch_retries = c.maxPatchRetries;
  if (c.recall !== undefined) w.recall = c.recall;
  if (c.harvestToMemory !== undefined) w.harvest_to_memory = c.harvestToMemory;
  return w;
}

/** runBody builds the snake_case /v1/runs request body from RunOptions,
 *  omitting unset fields (preserves the server's nil semantics — notably
 *  `allowedHosts: null` is treated as "omit", not deny-all). Shared by
 *  runStreaming and each spawn of spawnRunBatch. `signal`/`debug` are client
 *  concerns and never sent. */
function runBody(opts: RunOptions): Record<string, unknown> {
  const body: Record<string, unknown> = {
    agent: opts.agent,
    segments: opts.segments,
  };
  if (opts.tools !== undefined) body.tools = opts.tools;
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
  if (opts.parentContext !== undefined) body.parent_context = opts.parentContext;
  if (opts.idempotencyKey !== undefined) body.idempotency_key = opts.idempotencyKey;
  if (opts.maxWallSeconds !== undefined) body.max_wall_seconds = opts.maxWallSeconds;
  if (opts.admission !== undefined) body.admission = opts.admission;
  if (opts.retention !== undefined) body.retention = opts.retention;
  if (opts.metadata !== undefined) body.metadata = opts.metadata;
  if (opts.runTimeoutSeconds !== undefined) body.run_timeout_seconds = opts.runTimeoutSeconds;
  if (opts.reviewTtlSeconds !== undefined) body.review_ttl_seconds = opts.reviewTtlSeconds;
  if (opts.hooks !== undefined) body.hooks = opts.hooks;
  if (opts.toolHooks !== undefined) body.tool_hooks = opts.toolHooks;
  if (opts.sampling !== undefined) body.sampling = samplingToWire(opts.sampling);
  if (opts.compaction !== undefined) body.compaction = compactionToWire(opts.compaction);
  if (opts.context !== undefined) body.context = contextToWire(opts.context);
  if (opts.maxContextTokens !== undefined) body.max_context_tokens = opts.maxContextTokens;
  if (opts.interactive !== undefined) body.interactive = opts.interactive;
  applyOverridesToWire(body, opts);
  return body;
}

/** Serializes the RFC DC per-run overrides onto a request body.
 *
 *  One function rather than two copies of the list, because this file already
 *  serializes RunOptions field-by-field at two call sites — and a field added
 *  to the TYPE but to only one of those lists is invisible on the wire from one
 *  of them, silently. That is the shape of the bug RFC DA shipped.
 */
// The parameter is RunOverrideOptions itself, not a structural copy of its
// fields. It WAS such a copy — a second enumeration of the same list sitting in
// the signature of the function whose whole job is to be the single one — and
// adding a field to the shared interface then failed to compile here, which is
// the good outcome only because someone was looking. Typing it as the interface
// makes the list unduplicated rather than merely checked.
function applyOverridesToWire(body: Record<string, unknown>, opts: RunOverrideOptions): void {
  if (opts.model !== undefined) body.model = opts.model;
  if (opts.provider !== undefined) body.provider = opts.provider;
  if (opts.tier !== undefined) body.tier = opts.tier;
  if (opts.effort !== undefined) body.effort = opts.effort;
  if (opts.maxTokens !== undefined) body.max_tokens = opts.maxTokens;
  if (opts.maxIterations !== undefined) body.max_iterations = opts.maxIterations;
  if (opts.unboundedIterations !== undefined) body.unbounded_iterations = opts.unboundedIterations;
  if (opts.maxConcurrentChildren !== undefined) body.max_concurrent_children = opts.maxConcurrentChildren;
  if (opts.retryAttempts !== undefined) body.retry_attempts = opts.retryAttempts;
  if (opts.memoryInjectMaxTokens !== undefined) body.memory_inject_max_tokens = opts.memoryInjectMaxTokens;
  if (opts.memoryIndexMaxBytes !== undefined) body.memory_index_max_bytes = opts.memoryIndexMaxBytes;
  if (opts.injectToolGuide !== undefined) body.inject_tool_guide = opts.injectToolGuide;
  if (opts.interactive !== undefined) body.interactive = opts.interactive;
  if (opts.interruption !== undefined) body.interruption = opts.interruption;
  if (opts.review !== undefined) body.review = opts.review;
  // Same keys on the wire as in the option (mode / name / until).
  if (opts.toolChoice !== undefined) body.tool_choice = opts.toolChoice;
  if (opts.outputFormat !== undefined) body.output_format = opts.outputFormat;
  // Same keys on the wire as in the option (queue_aware / switch_after_ms).
  if (opts.routing !== undefined) body.routing = opts.routing;
}


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
    yield* this.streamSSE("/v1/runs", runBody(opts), opts.signal, opts.debug);
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
    if (opts.tools !== undefined) body.tools = opts.tools;
    if (opts.allowedHosts !== undefined && opts.allowedHosts !== null) {
      body.allowed_hosts = opts.allowedHosts;
    }
    if (opts.webSearchFilter !== undefined) body.web_search_filter = opts.webSearchFilter;
    if (opts.agentId !== undefined) body.agent_id = opts.agentId;
    if (opts.userTier !== undefined) body.user_tier = opts.userTier;
    if (opts.userBearer !== undefined) body.user_bearer = opts.userBearer;
    if (opts.userCredentials !== undefined) body.user_credentials = opts.userCredentials;
    if (opts.parentContext !== undefined) body.parent_context = opts.parentContext;
    if (opts.metadata !== undefined) body.metadata = opts.metadata;
    if (opts.runTimeoutSeconds !== undefined) body.run_timeout_seconds = opts.runTimeoutSeconds;
    if (opts.reviewTtlSeconds !== undefined) body.review_ttl_seconds = opts.reviewTtlSeconds;
    if (opts.hooks !== undefined) body.hooks = opts.hooks;
    if (opts.toolHooks !== undefined) body.tool_hooks = opts.toolHooks;
    if (opts.sampling !== undefined) body.sampling = samplingToWire(opts.sampling);
    if (opts.compaction !== undefined) body.compaction = compactionToWire(opts.compaction);
    if (opts.context !== undefined) body.context = contextToWire(opts.context);
    if (opts.maxContextTokens !== undefined) body.max_context_tokens = opts.maxContextTokens;
    if (opts.interactive !== undefined) body.interactive = opts.interactive;
    applyOverridesToWire(body, opts);
    yield* this.streamSSE(
      `/v1/sessions/${encodeURIComponent(opts.sessionId)}/messages`,
      body,
      opts.signal,
      opts.debug,
    );
  }

  /** Push an operator steering message into a LIVE interactive run (RFC AI).
   *  Mirrors `POST /v1/runs/{run_id}/input`. The run must be in-flight —
   *  parked at end_turn awaiting input, or mid-turn (the message is drained
   *  at the next iteration boundary). Does NOT open a stream; the operator's
   *  turn + the model's response arrive on the run's OWN event stream
   *  (the open `runStreaming` iterator, or a `streamRunByID` re-attach).
   *  Returns `{ run_id, delivered }`.
   *
   *  A message that reaches a run before it finishes is answered: one sent
   *  while a non-interactive run writes its final answer makes the run take
   *  one more turn, and that turn's answer is the run's result. Once the run
   *  has finished, a message is refused as not in flight
   *  ({@link NotFoundError}, 404) rather than accepted and never read.
   *
   *  Raises {@link UnavailableError} (503, steering off / no run),
   *  {@link AuthError} (401). A full steer queue surfaces as a 429. */
  async sendRunInput(
    runId: string,
    text: string,
    opts?: { signal?: AbortSignal; overrides?: RunOverrideOptions },
  ): Promise<{ run_id: string; delivered: boolean }> {
    const body: Record<string, unknown> = { text };
    if (opts?.overrides) {
      // The wire nests these under `overrides` on THIS endpoint (unlike the flat
      // fields on /v1/runs), because the request's other job is sending a turn
      // and the group needs a name.
      const o: Record<string, unknown> = {};
      applyOverridesToWire(o, opts.overrides);
      if (Object.keys(o).length > 0) body.overrides = o;
    }
    return postJSON<{ run_id: string; delivered: boolean }>(
      this.ctx,
      `/v1/runs/${encodeURIComponent(runId)}/input`,
      body,
      opts,
    );
  }

  /** Change a run's settings WITHOUT sending it a turn. Mirrors
   *  `POST /v1/runs/{run_id}/retune`.
   *
   *  Use this to retune a PARKED chat — switch it to a different model, raise
   *  its iteration bound — when you do not also want a message in the
   *  transcript. `sendRunInput(runId, text, { overrides })` is the other half:
   *  retune and speak in one atomic call.
   *
   *  The overrides select WITHIN what the agent's definition already allows and
   *  cannot widen it; an override the definition forbids is REFUSED here rather
   *  than silently dropped, so a rejected retune is visible at the call.
   *
   *  Raises {@link NotFoundError} (404, no in-flight run — which is also what a
   *  run belonging to another tenant returns, deliberately), and 422 when no
   *  override is supplied at all. */
  async retuneRun(
    runId: string,
    overrides: RunOverrideOptions,
    opts?: { signal?: AbortSignal },
  ): Promise<RetuneRunResponse> {
    const body: Record<string, unknown> = {};
    applyOverridesToWire(body, overrides);
    return postJSON<RetuneRunResponse>(
      this.ctx,
      `/v1/runs/${encodeURIComponent(runId)}/retune`,
      body,
      opts,
    );
  }

  /** Read a run's own stored overrides. Mirrors `GET /v1/runs/{run_id}/config`.
   *
   *  This is what the RUN overrides, not what it will effectively use: a field
   *  absent from `config` means "this run does not override it", and the
   *  definition, the tier or a driver still decides. {@link getEffectiveConfig}
   *  is the resolved view.
   *
   *  A run that was never retuned answers with an empty `config` — "this run
   *  overrides nothing" is an answer, not a 404.
   *
   *  Raises {@link NotFoundError} (404), which is also what a run belonging to
   *  another tenant returns — deliberately, so the gate is not an existence
   *  oracle for run ids that are not secrets. */
  async getRunConfig(
    runId: string,
    opts?: { signal?: AbortSignal },
  ): Promise<RunConfigResponse> {
    return jsonFetch<RunConfigResponse>(
      this.ctx,
      `/v1/runs/${encodeURIComponent(runId)}/config`,
      opts,
    );
  }

  /** Read the prompt a run's first model call received (RFC DI). Mirrors
   *  `GET /v1/runs/{run_id}/prompt`. Recorded when the model was called, not
   *  re-derived now, so it is what the model saw even if a document or memory
   *  it quoted has changed since.
   *
   *  Raises {@link NotFoundError} (404) for an unknown run and for another
   *  tenant's run alike, and also — with code `no_prompt_snapshot` — for a run
   *  that never reached a model call or predates prompt snapshots. */
  /** Create a run WITHOUT starting it (RFC DI): a configured run holds its
   *  request, runs nothing and takes no slot until
   *  {@link LoomcycleClient.startConfiguredRun}. It is validated now exactly as
   *  a run would be; admission and the budget check happen at start. It never
   *  stores a secret — pass `userBearer` / `userCredentials` to the start. */
  async createConfiguredRun(
    opts: Omit<RunOptions, "userBearer" | "userCredentials" | "sessionId" | "debug">,
  ): Promise<ConfiguredRun> {
    const body = runBody(opts as RunOptions);
    body.start = false;
    return postJSON<ConfiguredRun>(this.ctx, "/v1/runs", body, { signal: opts.signal });
  }

  /** Replace fields of a configured run's request. Set fields replace the
   *  draft's; `remove` names wire fields (snake_case, e.g. `"sampling"`) to
   *  drop. The agent, the identity and secrets cannot be changed. Refused
   *  (409) once the run has started. */
  async updateConfiguredRun(
    runId: string,
    patch: ConfiguredRunPatch,
    opts?: { remove?: string[]; signal?: AbortSignal },
  ): Promise<ConfiguredRun> {
    // runBody serializes only the fields that are set, so a partial patch
    // stays partial on the wire.
    const body = runBody(patch as RunOptions);
    delete body.agent;
    for (const k of opts?.remove ?? []) body[k] = null;
    return patchJSON<ConfiguredRun>(
      this.ctx,
      `/v1/runs/${encodeURIComponent(runId)}`,
      body,
      { signal: opts?.signal },
    );
  }

  /** Start a configured run and stream it, exactly as
   *  {@link LoomcycleClient.runStreaming} streams a new one. Admission happens
   *  here: a refusal (429 / 503) throws before any event and leaves the run
   *  configured, to be started again later. Secrets the draft never stored
   *  are passed here. */
  async *startConfiguredRun(
    runId: string,
    opts?: { userBearer?: string; userCredentials?: Record<string, string>; signal?: AbortSignal; debug?: boolean },
  ): AsyncIterable<AgentEvent> {
    const body: Record<string, unknown> = {};
    if (opts?.userBearer !== undefined) body.user_bearer = opts.userBearer;
    if (opts?.userCredentials !== undefined) body.user_credentials = opts.userCredentials;
    yield* this.streamSSE(`/v1/runs/${encodeURIComponent(runId)}/start`, body, opts?.signal, opts?.debug);
  }

  /** Discard a configured run and its session. A live run is not discarded —
   *  cancel it instead. */
  async deleteConfiguredRun(runId: string, opts?: { signal?: AbortSignal }): Promise<void> {
    await deleteRequest(this.ctx, `/v1/runs/${encodeURIComponent(runId)}`, opts);
  }

  async getRunPrompt(
    runId: string,
    opts?: { signal?: AbortSignal },
  ): Promise<RunPromptResponse> {
    return jsonFetch<RunPromptResponse>(
      this.ctx,
      `/v1/runs/${encodeURIComponent(runId)}/prompt`,
      opts,
    );
  }

  /** Read what a run will ACTUALLY use, field by field, with the layer that
   *  decided each. Mirrors `GET /v1/runs/{run_id}/effective-config`.
   *
   *  Nothing else assembles this: a definition is a sparse overlay, the tier and
   *  driver defaults are published nowhere else, and a field no layer set
   *  resolves later somewhere the caller cannot see. The `source` on each field
   *  is the point — `max_iterations: 16` cannot tell a deliberate setting from a
   *  default nobody chose, and those call for opposite actions.
   *
   *  Keys are wire names (`max_tokens`, not `maxTokens`), so the result joins
   *  directly against a definition from the library endpoints.
   *
   *  Raises {@link NotFoundError} (404) for an unknown or cross-tenant run. */
  async getEffectiveConfig(
    runId: string,
    opts?: { signal?: AbortSignal },
  ): Promise<EffectiveConfigResponse> {
    return jsonFetch<EffectiveConfigResponse>(
      this.ctx,
      `/v1/runs/${encodeURIComponent(runId)}/effective-config`,
      opts,
    );
  }

  /** Which per-run overrides a run of `name` may set, for this caller — what a
   *  run form needs before it sends anything. Mirrors
   *  `GET /v1/_agentdef/{name}/overridable`.
   *
   *  Folds the agent's `allow_overrides` with the caller's token overrides
   *  policy, through the same checks the run path refuses with; a refused field
   *  names which (`reason`). Overridable fields carry what the run path then
   *  accepts: `narrow_only`, `max`, and `values` for a closed set.
   *
   *  `substrate:tenant` (a non-isolated member too). A token limited to certain
   *  agents may ask about those agents only. Raises {@link AgentNotFoundError}
   *  (404) for an unknown or another tenant's agent, and a 403
   *  `agent_not_allowed` for an agent outside the token's allowlist. */
  async getAgentOverridable(
    name: string,
    opts?: {
      /** Admin-only tenant focus (?tenant=); ignored for a tenant principal. */
      tenant?: string;
      signal?: AbortSignal;
    },
  ): Promise<AgentOverridableResponse> {
    const q = opts?.tenant ? `?tenant=${encodeURIComponent(opts.tenant)}` : "";
    return jsonFetch<AgentOverridableResponse>(
      this.ctx,
      `/v1/_agentdef/${encodeURIComponent(name)}/overridable${q}`,
      opts,
    );
  }

  /** Re-attach to a run's event stream by `run_id` (RFC AI), replaying from
   *  `fromSeq` (default 0) then live-tailing — the gRPC-free twin of the Web
   *  UI's resume-in-terminal. Mirrors `GET /v1/runs/{run_id}/stream`. The
   *  operator's own prior turns are replayed as `steer` events
   *  (`user_input.source === "replay"`), so a cold client — e.g. resuming on
   *  another device — reconstructs the whole conversation. A PARKED
   *  interactive run keeps streaming until it ends or `signal` aborts; a
   *  disconnect does NOT stop the run.
   *
   *  The first frame is an `agent` side-channel carrying the run's
   *  agent_id / session_id (for `sendRunInput` / `cancelAgent`). */
  async *streamRunByID(
    runId: string,
    opts?: { fromSeq?: number; signal?: AbortSignal },
  ): AsyncIterable<AgentEvent> {
    const q = opts?.fromSeq ? `?from_seq=${opts.fromSeq}` : "";
    yield* this.streamSSEGet(
      `/v1/runs/${encodeURIComponent(runId)}/stream${q}`,
      opts?.signal,
    );
  }

  /** Start a high-level INTERACTIVE session (RFC AI) — the ergonomic driver
   *  for a persistent run you converse with turn by turn. Starts an
   *  `interactive` run and returns an {@link InteractiveSession} whose
   *  `events()` you iterate and whose `send()` / `cancel()` steer it. See
   *  {@link InteractiveSession}. For the raw primitives, use
   *  {@link LoomcycleClient.runStreaming} with `interactive: true` +
   *  {@link LoomcycleClient.sendRunInput}. */
  interactiveSession(opts: Omit<RunOptions, "interactive">): InteractiveSession {
    const source = this.runStreaming({ ...opts, interactive: true });
    return new InteractiveSession(source, {
      sendRunInput: (rid, t, o) => this.sendRunInput(rid, t, o),
      retuneRun: (rid, ov) => this.retuneRun(rid, ov),
      cancelAgent: (aid) => this.cancelAgent(aid),
    });
  }

  /** Re-attach to a detached interactive run by `run_id` as a high-level
   *  {@link InteractiveSession} — e.g. resuming on another device. The run_id
   *  is known up front, so `send()` works immediately. Replays the
   *  conversation (incl. the operator's prior turns, RFC AI) then live-tails. */
  attachInteractiveSession(
    runId: string,
    opts?: { fromSeq?: number; signal?: AbortSignal },
  ): InteractiveSession {
    const source = this.streamRunByID(runId, opts);
    const session = new InteractiveSession(source, {
      sendRunInput: (rid, t, o) => this.sendRunInput(rid, t, o),
      retuneRun: (rid, ov) => this.retuneRun(rid, ov),
      cancelAgent: (aid) => this.cancelAgent(aid),
    });
    session.runId = runId;
    return session;
  }

  /**
   * Spawn N fresh runs concurrently in ONE call (RFC Y external fan-out) and
   * resolve once they ALL settle, returning the combined index-aligned
   * envelope. A per-child failure is captured in that child's result
   * (`status` + `error`) and never rejects the batch. Prefer this over firing
   * N parallel {@link LoomcycleClient.runStreaming} calls.
   *
   * Each spawn is a fresh run (its `sessionId` is ignored). Capped at 32 —
   * an over-cap batch rejects with InvalidArgumentError.
   *
   * Blocking by default: resolves only when the slowest child finishes (or
   * `timeoutMs` elapses). With `mode: "detach"` it resolves once every child
   * has started or queued — each with `status: "running"` (or `"queued"` while
   * it waits for a run slot; it starts by itself when one frees) and its
   * `run_id` — and the runs go on; read one with
   * {@link LoomcycleClient.getRun}. Mirrors
   * POST /v1/runs:batch.
   */
  async spawnRunBatch(opts: RunBatchOptions): Promise<RunBatchResult> {
    const body: Record<string, unknown> = {
      spawns: opts.spawns.map(runBody),
    };
    if (opts.mode !== undefined) body.mode = opts.mode;
    if (opts.timeoutMs !== undefined) body.timeout_ms = opts.timeoutMs;
    return postJSON<RunBatchResult>(this.ctx, "/v1/runs:batch", body, {
      signal: opts.signal,
    });
  }

  /**
   * Compact a run's conversation: summarize the history to free context and
   * continue from the summary. Targets the run by `runId`. A live run must be
   * PARKED (awaiting input) — a mid-turn run rejects (RunBusyError / 409).
   * Returns `{ compacted, before_tokens, after_tokens, applied }`, where
   * `applied` is "live", "marker", or "noop". Mirrors
   * POST /v1/runs/{run_id}/compact.
   */
  /**
   * List the subjects with activity in a tenant (loomcycle v1.46.0+).
   *
   * A "user" here is DERIVED from run activity, not a stored record — there is no
   * create or update, and removing a subject's footprint is
   * {@link erasureExecute}.
   */
  async directoryUsers(
    opts?: { tenant?: string; signal?: AbortSignal },
  ): Promise<{ users: DirectoryUser[] }> {
    const q = opts?.tenant !== undefined
      ? `?tenant=${encodeURIComponent(opts.tenant)}`
      : "";
    return jsonFetch<{ users: DirectoryUser[] }>(this.ctx, `/v1/_users${q}`, opts);
  }

  /**
   * Aggregate one subject's activity, chats, memory, documents, budget and usage
   * in a single call (loomcycle v1.46.0+).
   *
   * An admin token MUST pass `tenant` — a subject id is only unique within one, so
   * the server refuses rather than guessing (`""` selects the default tenant on a
   * single-tenant install). A tenant-scoped principal is confined regardless.
   */
  async directoryInspect(
    subject: string,
    opts?: { tenant?: string; signal?: AbortSignal },
  ): Promise<DirectoryInspection> {
    const q = opts?.tenant !== undefined
      ? `?tenant=${encodeURIComponent(opts.tenant)}`
      : "";
    return jsonFetch<DirectoryInspection>(
      this.ctx,
      `/v1/_users/${encodeURIComponent(subject)}${q}`,
      opts,
    );
  }

  /**
   * Enumerate tenants with derived counts (loomcycle v1.46.0+). **Requires an
   * operator-admin token** — the list of tenants is itself cross-tenant
   * information, so a scoped token is refused rather than given a filtered list.
   *
   * Derived from runs: a tenant that has never started a run does not appear, so
   * an empty list means no ACTIVITY, not no tenants.
   */
  async directoryTenants(
    opts?: { signal?: AbortSignal },
  ): Promise<{ tenants: DirectoryTenant[]; notes?: string[] }> {
    return jsonFetch<{ tenants: DirectoryTenant[]; notes?: string[] }>(
      this.ctx,
      "/v1/_tenants",
      opts,
    );
  }

  /**
   * Report what this deployment holds about one SUBJECT, in three tiers
   * separated by what they guarantee (RFC BL P5). Read-only.
   *
   * `tenant` is optional and honored only for an admin token; a tenant-scoped
   * principal is confined to its own tenant regardless. An admin MUST pass it —
   * a subject id is only unique within one tenant, so the server refuses rather
   * than guessing (pass `""` for the default tenant on a single-tenant install).
   */
  async erasureReport(
    subject: string,
    opts?: { tenant?: string; signal?: AbortSignal },
  ): Promise<ErasureReport> {
    const q = new URLSearchParams({ subject });
    if (opts?.tenant !== undefined) q.set("tenant", opts.tenant);
    return jsonFetch<ErasureReport>(this.ctx, `/v1/_erasure?${q}`, opts);
  }

  /**
   * Erase tiers 1 and 2 for a SUBJECT, reporting what could not be reached.
   *
   * DEFAULTS TO A DRY RUN. `dryRun` defaults to true and a live run additionally
   * requires `confirm` to equal `subject`; omitting either deletes nothing.
   *
   * IMPORTANT: tier-3 residue is traceable only through the subject's chats,
   * which a live run deletes — so a report afterwards shows `rows: 0` while
   * those facts remain. The returned object is the ONLY durable record of what
   * was not reached; persist it.
   */
  async erasureExecute(
    subject: string,
    opts?: {
      dryRun?: boolean;
      confirm?: string;
      tenant?: string;
      signal?: AbortSignal;
    },
  ): Promise<ErasureResult> {
    const body: Record<string, unknown> = { subject };
    // Sent explicitly rather than relying on the server default, so a caller
    // reading this code sees which mode the request is in.
    body.dry_run = opts?.dryRun ?? true;
    if (opts?.confirm !== undefined) body.confirm = opts.confirm;
    const q = opts?.tenant !== undefined
      ? `?tenant=${encodeURIComponent(opts.tenant)}`
      : "";
    return postJSON<ErasureResult>(this.ctx, `/v1/_erasure${q}`, body, opts);
  }

  async compactRun(
    runId: string,
    opts?: { reason?: string; signal?: AbortSignal },
  ): Promise<CompactRunResult> {
    const body: Record<string, unknown> = {};
    if (opts?.reason !== undefined) body.reason = opts.reason;
    return postJSON<CompactRunResult>(
      this.ctx,
      `/v1/runs/${encodeURIComponent(runId)}/compact`,
      body,
      opts,
    );
  }

  /** Permanently delete a run and every run it spawned, with what is stored
   *  about them: transcripts, their sessions when no other run is left in
   *  them, interrupts, evaluations of them, queued memory rows and indexed
   *  turns. The usage ledger (tokens, cost) is kept. Mirrors
   *  `DELETE /v1/runs/{run_id}/tree`.
   *
   *  Needs the `runs:delete` scope (an admin and a tenant operator have it;
   *  any other token only if it was granted at mint) — a token without it
   *  raises `LoomcycleError` with status 403. A run that does not exist, was
   *  already deleted, or is not yours to read resolves with `deleted_runs: 0`
   *  rather than raising, so repeating the call is safe. A tree holding a run
   *  that is still running, awaiting input or paused deletes nothing and
   *  raises `LoomcycleError` with `code: "run_tree_live"` (status 409): cancel
   *  it first. `not_reached` names where a copy may remain that this call
   *  cannot remove. To discard a configured run that never started, use
   *  {@link LoomcycleClient.deleteConfiguredRun}. */
  async deleteRunTree(runId: string, opts?: { signal?: AbortSignal }): Promise<RunTreeDeletion> {
    return deleteJSON<RunTreeDeletion>(this.ctx, `/v1/runs/${encodeURIComponent(runId)}/tree`, opts);
  }

  /** Permanently delete every run of a session, each with the runs it
   *  spawned — {@link LoomcycleClient.deleteRunTree} for all of them at once.
   *  `cascade: "children"` is required and is the only mode: there is no
   *  delete that leaves sub-agent runs behind. Mirrors
   *  `DELETE /v1/sessions/{session_id}?cascade=children`. Same scope, same
   *  zero-count answer for a session you cannot read, same `run_tree_live`
   *  refusal. */
  async deleteSession(
    sessionId: string,
    opts: { cascade: "children"; signal?: AbortSignal },
  ): Promise<RunTreeDeletion> {
    return deleteJSON<RunTreeDeletion>(
      this.ctx,
      `/v1/sessions/${encodeURIComponent(sessionId)}?cascade=${encodeURIComponent(opts.cascade)}`,
      opts,
    );
  }

  /**
   * Replay a session's transcript into a NEW session bound to a (possibly
   * different) target agent, so that agent continues from the same context
   * (RFC BJ Phase 4). `sourceSessionId` is the source; the returned
   * `new_session_id` is a fresh session you continue with the normal message
   * path — the carried context replays automatically. Pass `compress` to
   * collapse the carried history to a summary + recent tail.
   */
  async replaySession(
    sourceSessionId: string,
    opts: { agent: string; compress?: boolean; signal?: AbortSignal },
  ): Promise<ReplaySessionResult> {
    const body: Record<string, unknown> = { agent: opts.agent };
    if (opts.compress !== undefined) body.compress = opts.compress;
    return postJSON<ReplaySessionResult>(
      this.ctx,
      `/v1/sessions/${encodeURIComponent(sourceSessionId)}/replay`,
      body,
      opts,
    );
  }

  /**
   * Stop the CURRENT turn of a live interactive run (its in-flight generation +
   * the tool calls it started) and park it at awaiting_input — session +
   * transcript intact, ready for the next message. This is the "Esc" gesture,
   * NOT whole-run cancel ({@link LoomcycleClient.cancelAgent}), which terminates
   * the run. Returns `{ run_id, stopped, parked }`.
   *
   * A team walk's run (the `run_id` a detached `runTeam` returns) has no turns,
   * so this ENDS the walk and the runs it spawned: `{ stopped: true, parked:
   * false }`, recorded as cancelled. Walks are reachable on the replica running
   * them only.
   *
   * Rejects with RunBusyError (409) when the run isn't mid-turn or isn't
   * interactive, and AgentNotFoundError (404) for an unknown / cross-tenant run.
   * Mirrors POST /v1/runs/{run_id}/cancel. (RFC BH)
   */
  async cancelTurn(
    runId: string,
    opts?: { reason?: string; signal?: AbortSignal },
  ): Promise<CancelTurnResult> {
    const body: Record<string, unknown> = {};
    if (opts?.reason !== undefined) body.reason = opts.reason;
    return postJSON<CancelTurnResult>(
      this.ctx,
      `/v1/runs/${encodeURIComponent(runId)}/cancel`,
      body,
      opts,
    );
  }

  /**
   * Deliver a verdict on a run held for review (one started or retuned with
   * `review: true`, whose stream has emitted `awaiting_review`).
   *
   * - `approve` — the run completes on the answer it was held on.
   * - `reject` with `feedback` — the feedback becomes the run's next user turn;
   *   it revises and is held again.
   * - `reject` without feedback — the run ends with status `rejected`.
   *
   * Rejects with a LoomcycleError of status 409 (body code `not_held`) when
   * the run is live but not held, NotFoundError (404) for an unknown, finished
   * or cross-tenant run, and InvalidArgumentError (400) for feedback on an
   * approval. Mirrors POST /v1/runs/{run_id}/review.
   */
  async reviewRun(
    runId: string,
    decision: "approve" | "reject",
    opts?: { feedback?: string; signal?: AbortSignal },
  ): Promise<ReviewRunResult> {
    const body: Record<string, unknown> = { decision };
    if (opts?.feedback !== undefined) body.feedback = opts.feedback;
    return postJSON<ReviewRunResult>(
      this.ctx,
      `/v1/runs/${encodeURIComponent(runId)}/review`,
      body,
      opts,
    );
  }

  // ---- Agent metadata ----

  /** Read one agent's status + usage stats. Raises AgentNotFoundError
   *  when the agent_id is unknown. */
  async getAgent(agentId: string, opts?: { signal?: AbortSignal }): Promise<Agent> {
    return jsonFetch<Agent>(this.ctx, `/v1/agents/${encodeURIComponent(agentId)}`, opts);
  }

  /** Read one run by its run id — the same {@link Agent} shape as
   *  {@link getAgent}. An agent id can name many runs (every walk of a team
   *  is filed under `team:<name>`, which getAgent cannot address) and
   *  getAgent answers the latest; a run id names one. Raises NotFoundError
   *  for an unknown run, or one the caller may not see. */
  async getRun(runId: string, opts?: { signal?: AbortSignal }): Promise<Agent> {
    return jsonFetch<Agent>(this.ctx, `/v1/runs/${encodeURIComponent(runId)}`, opts);
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

  /** List a user's recent agent runs (at most 100), optionally filtered by
   *  status. With no `status` only running runs are returned; `"all"` returns
   *  every status; several statuses list a run in any of them.
   *
   *  `awaitedState` lists only running runs blocked on one of the named waits
   *  (`"review"`, `"input"`, …). The state is derived per run, so the server
   *  reads at most 1,000 running runs; the returned array carries
   *  `truncated: true` when it stopped with some unread.
   *
   *  v0.9.x — `parentAgentId` narrows the result CLIENT-SIDE to runs
   *  whose `parent_agent_id` matches. The server still returns the
   *  full set (server-side `?parent_agent_id=` filter is a future
   *  request); the adapter trims before returning. Useful for the
   *  n8n trigger pattern "show me all sub-runs spawned by parent X." */
  async listUserAgents(userId: string, opts?: ListUserAgentsOptions): Promise<AgentList> {
    const params = new URLSearchParams();
    for (const s of ([] as string[]).concat(opts?.status ?? [])) params.append("status", s);
    for (const s of ([] as string[]).concat(opts?.awaitedState ?? [])) params.append("awaited_state", s);
    if (opts?.tenant) params.set("tenant", opts.tenant);
    const q = params.toString() ? `?${params.toString()}` : "";
    const resp = await jsonFetch<ListAgentsResponse>(
      this.ctx,
      `/v1/users/${encodeURIComponent(userId)}/agents${q}`,
      opts,
    );
    let out: AgentList = resp.agents ?? [];
    if (opts?.parentAgentId !== undefined && opts.parentAgentId !== "") {
      out = out.filter((a) => a.parent_agent_id === opts.parentAgentId);
    }
    if (resp.truncated) out.truncated = true;
    return out;
  }

  /** One page of a team walk's runs — the walk's own run (its run id IS the
   *  walk id) and every member it spawned, oldest first. The walk's own row
   *  carries its `result` (with the `terminal` it reached); member rows do
   *  not. Pass the page's `next_cursor` back as `cursor` for the next one; it
   *  is "" on the last. `limit` defaults to 100, at most 1000. Mirrors
   *  `GET /v1/runs?walk_id=`. Raises NotFoundError for an unknown walk, or
   *  one the caller may not see. */
  async listWalkRuns(
    walkId: string,
    opts?: { limit?: number; cursor?: string; signal?: AbortSignal },
  ): Promise<WalkRunsPage> {
    const params = new URLSearchParams({ walk_id: walkId });
    if (opts?.limit !== undefined) params.set("limit", String(opts.limit));
    if (opts?.cursor) params.set("cursor", opts.cursor);
    const resp = await jsonFetch<WalkRunsPage>(this.ctx, `/v1/runs?${params.toString()}`, opts);
    return { agents: resp.agents ?? [], next_cursor: resp.next_cursor ?? "" };
  }

  /** One page of a run's direct children — the runs it started itself, for
   *  example through the Agent tool — of every status, oldest first. Not the
   *  run itself, and not its grandchildren: call it again on a child for
   *  those. Rows are {@link LoomcycleClient.listWalkRuns}' rows, and none
   *  carries a `result`. Pass the page's `next_cursor` back as `cursor` for
   *  the next one; it is "" on the last. `limit` defaults to 100, at most
   *  1000. Mirrors `GET /v1/runs?parent_run_id=`. Raises NotFoundError for an
   *  unknown run, or one the caller may not see. */
  async listChildRuns(
    parentRunId: string,
    opts?: { limit?: number; cursor?: string; signal?: AbortSignal },
  ): Promise<WalkRunsPage> {
    const params = new URLSearchParams({ parent_run_id: parentRunId });
    if (opts?.limit !== undefined) params.set("limit", String(opts.limit));
    if (opts?.cursor) params.set("cursor", opts.cursor);
    const resp = await jsonFetch<WalkRunsPage>(this.ctx, `/v1/runs?${params.toString()}`, opts);
    return { agents: resp.agents ?? [], next_cursor: resp.next_cursor ?? "" };
  }

  /** Aggregated token-usage + cost report (RFC AV). Group by any of
   *  tenant/user/provider/model/source over an optional time window; group by
   *  `source` for the operator-vs-tenant split. Tenant-scoped server-side: a
   *  tenant principal sees only its own tenant (the `tenant` focus is
   *  admin-only). Mirrors `GET /v1/_usage`. */
  async usageReport(opts?: {
    /** Dimensions to group by (default server-side: tenant,source). */
    groupBy?: UsageDimension[];
    /** RFC3339 window bounds (inclusive); omit for unbounded. */
    from?: string;
    to?: string;
    /** Super-admin tenant focus (?tenant=); ignored for a tenant principal. */
    tenant?: string;
    signal?: AbortSignal;
  }): Promise<UsageReportResponse> {
    const params = new URLSearchParams();
    if (opts?.groupBy && opts.groupBy.length > 0)
      params.set("group_by", opts.groupBy.join(","));
    if (opts?.from) params.set("from", opts.from);
    if (opts?.to) params.set("to", opts.to);
    if (opts?.tenant) params.set("tenant", opts.tenant);
    const q = params.toString() ? `?${params.toString()}` : "";
    return jsonFetch<UsageReportResponse>(this.ctx, `/v1/_usage${q}`, opts);
  }

  /** Instance configuration: build identity, the feature matrix, and the live
   *  provider/model/search cascade with coarse active/inactive status. The
   *  out-of-band twin of the in-band `Context op=capabilities`, sharing its
   *  probe server-side so the two cannot disagree. Mirrors `GET /v1/config`.
   *
   *  `view` names the disclosure level the response was rendered at, so you can
   *  tell "this deployment has none" from "you weren't shown them". A deployment
   *  running with `LOOMCYCLE_PUBLIC_CONFIG=1` also serves this **with no
   *  bearer**, returning the narrower `"public"` view — which is what a landing
   *  page fetches directly. `models` is one entry per (provider, model),
   *  deduplicated across plans, with `selected` marking what actually runs. */
  async getConfig(opts?: { signal?: AbortSignal }): Promise<ConfigResponse> {
    return jsonFetch<ConfigResponse>(this.ctx, "/v1/config", opts);
  }

  /** List the per-scope token budgets visible to the caller (RFC AW), each with
   *  its live month-to-date usage. Tenant-scoped server-side: a tenant operator
   *  sees only its own tenant's budgets; an admin sees all (or focuses one via
   *  `tenant`). Mirrors `GET /v1/_limits`. */
  async listLimits(opts?: {
    /** Admin-only tenant focus (?tenant=); ignored for a tenant principal. */
    tenant?: string;
    signal?: AbortSignal;
  }): Promise<TokenLimitsResponse> {
    const q = opts?.tenant ? `?tenant=${encodeURIComponent(opts.tenant)}` : "";
    return jsonFetch<TokenLimitsResponse>(this.ctx, `/v1/_limits${q}`, opts);
  }

  /** Upsert one token budget (RFC AW). A present `soft_limit`/`hard_limit` sets
   *  that tier; omitting it clears the tier (unlimited on that axis) — a
   *  full-row upsert. The operator-global scope + any cross-tenant `tenant_id`
   *  are admin-only (403 otherwise). Mirrors `PUT /v1/_limits`. */
  async setLimit(
    body: SetTokenLimitRequest,
    opts?: { signal?: AbortSignal },
  ): Promise<TokenLimit> {
    return putJSON<TokenLimit>(this.ctx, "/v1/_limits", body, opts);
  }

  /** Delete a token budget → the scope is unlimited again (RFC AW). Same
   *  tenant-confinement as `setLimit`. Mirrors `DELETE /v1/_limits`. */
  async deleteLimit(
    scope: string,
    opts?: {
      /** Required for scope=user (the subject); empty for scope=tenant. */
      scopeId?: string;
      /** Admin-only target tenant; ignored for a tenant principal. */
      tenant?: string;
      signal?: AbortSignal;
    },
  ): Promise<void> {
    const params = new URLSearchParams({ scope });
    if (opts?.scopeId) params.set("scope_id", opts.scopeId);
    if (opts?.tenant) params.set("tenant", opts.tenant);
    return deleteRequest(this.ctx, `/v1/_limits?${params.toString()}`, opts);
  }

  /** The timed no-trace overrides in force: agents whose runs are being
   *  stored although they normally store none. An admin sees every tenant's
   *  (or one with `tenant`); a tenant operator sees its own. Mirrors
   *  `GET /v1/_retention/overrides`. */
  async listRetentionOverrides(opts?: {
    /** Admin-only tenant focus; ignored for a tenant operator. */
    tenant?: string;
    signal?: AbortSignal;
  }): Promise<{ overrides: RetentionOverride[] }> {
    const q = opts?.tenant ? `?tenant=${encodeURIComponent(opts.tenant)}` : "";
    return jsonFetch<{ overrides: RetentionOverride[] }>(this.ctx, `/v1/_retention/overrides${q}`, opts);
  }

  /** Store the runs of one no-trace agent until `until`, to debug it. At
   *  most 24 hours ahead; setting one replaces the last. It lifts a mode
   *  that came from the agent's definition or the run request, never one a
   *  token requires. Audited. `tenant` is admin-only: a tenant operator's
   *  override is always for its own tenant. Mirrors
   *  `PUT /v1/_retention/overrides`. */
  async setRetentionOverride(
    body: { agent: string; until: string | Date; tenant?: string },
    opts?: { signal?: AbortSignal },
  ): Promise<RetentionOverride> {
    const until = body.until instanceof Date ? body.until.toISOString() : body.until;
    const wire: Record<string, unknown> = { agent: body.agent, until };
    if (body.tenant !== undefined) wire.tenant = body.tenant;
    return putJSON<RetentionOverride>(this.ctx, "/v1/_retention/overrides", wire, opts);
  }

  /** End an agent's no-trace override before it runs out. Clearing one that
   *  is not there is not an error. Mirrors
   *  `DELETE /v1/_retention/overrides`. */
  async clearRetentionOverride(
    agent: string,
    opts?: {
      /** Admin-only target tenant; ignored for a tenant operator. */
      tenant?: string;
      signal?: AbortSignal;
    },
  ): Promise<void> {
    const params = new URLSearchParams({ agent });
    if (opts?.tenant) params.set("tenant", opts.tenant);
    return deleteRequest(this.ctx, `/v1/_retention/overrides?${params.toString()}`, opts);
  }

  /** Ask a decision model typed questions (`choice` / `noul` / `score`) about
   *  `state` and get each answer with its probabilities. A decision model does
   *  not write: there is no reply to parse and the call costs a few output
   *  tokens. No run is involved; the tokens are charged to the caller and count
   *  against its token budget. Mirrors `POST /v1/_decide`.
   *
   *  A refusal throws with the decision's own code on `e.code` and the HTTP
   *  status on `e.status`: 400 `invalid_input` / `bad_question` / `bad_options`
   *  / `too_many_questions` / `model_not_allowed`, 403
   *  `operator_key_restricted`, 413 `prompt_too_large`, 429
   *  `token_limit_exceeded`, 502 `model_not_found` / `call_failed`, 503
   *  `decision_not_configured`, 504 `timeout`. Branch on the code: apart from
   *  `PromptTooLargeError` (413) and `TokenLimitExceededError` (429), the
   *  error class follows the status alone, and a status is shared with
   *  unrelated conditions. */
  async decide(
    req: DecideRequest,
    opts?: { signal?: AbortSignal },
  ): Promise<DecideResponse> {
    const body: Record<string, unknown> = {
      state: req.state,
      questions: req.questions,
    };
    if (req.model !== undefined) body.model = req.model;
    return postJSON<DecideResponse>(this.ctx, "/v1/_decide", body, opts);
  }

  /** The decision models a {@link decide} call may name, with each one's
   *  limits, and the default. Throws with `code: "decision_not_configured"`
   *  (503) on a deployment that declares none. Mirrors `GET /v1/_decide/models`. */
  async listDecisionModels(opts?: {
    signal?: AbortSignal;
  }): Promise<DecisionModelsResponse> {
    return jsonFetch<DecisionModelsResponse>(this.ctx, "/v1/_decide/models", opts);
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

  /** List known users with running-count summary. Drives the Web UI's
   *  user picker. Tenant-scoped server-side since v0.17.0 (RFC L): a
   *  tenant principal sees only its own tenant's users; an admin sees all,
   *  or focuses one tenant via `tenant` (?tenant=). */
  async listUsers(opts?: {
    tenant?: string;
    signal?: AbortSignal;
  }): Promise<ListUsersResponse> {
    const q = opts?.tenant ? `?tenant=${encodeURIComponent(opts.tenant)}` : "";
    return jsonFetch<ListUsersResponse>(this.ctx, `/v1/_users${q}`, opts);
  }

  // ---- RFC BX Phase 2: tenant-owned users + delegated per-user tokens (v1.50.0+) ----
  //
  // A tenant operator (substrate:tenant) manages the first-class users in its
  // OWN tenant and mints/lists/revokes bearer tokens for them. The tenant is
  // always server-derived from the authenticated principal — never sent — so
  // there is no tenant field on any of these calls. Requires a persistent store
  // (503 otherwise).

  /** Register a first-class user in the caller's own tenant. `access_mode`
   *  defaults to "tenant" (collaborates on tenant-shared primitives);
   *  "isolated" confines the member to its own user scope. 409 on a duplicate
   *  (tenant, subject). */
  async createUser(
    body: CreateUserBody,
    opts?: { signal?: AbortSignal },
  ): Promise<UserRecord> {
    return postJSON<UserRecord>(this.ctx, "/v1/_users", body, opts);
  }

  /** Patch mutable fields on a user in the caller's own tenant. An omitted key
   *  leaves the column unchanged. Disabling a user (status:"disabled") also
   *  revokes its delegated tokens server-side. Cross-tenant target → 404. */
  async updateUser(
    subject: string,
    body: UpdateUserBody,
    opts?: { signal?: AbortSignal },
  ): Promise<UserRecord> {
    return patchJSON<UserRecord>(
      this.ctx,
      `/v1/_users/${encodeURIComponent(subject)}`,
      body,
      opts,
    );
  }

  /** Delete the user identity row in the caller's own tenant (204). Its
   *  delegated tokens are retired first so the bearer cannot outlive the row.
   *  Owned data (runs / sessions / memory) is left intact — this is not
   *  erasure ({@link erasureExecute}). Cross-tenant target → 404. */
  async deleteUser(subject: string, opts?: { signal?: AbortSignal }): Promise<void> {
    return deleteRequest(
      this.ctx,
      `/v1/_users/${encodeURIComponent(subject)}`,
      opts,
    );
  }

  /** Mint a bearer token for one of the caller's own ACTIVE users. The granted
   *  scopes are DERIVED server-side from the member's access_mode (never sent),
   *  and the plaintext `token` is returned exactly ONCE — store it now, it is
   *  never retrievable again. 404 for an unknown/cross-tenant subject; 409 for
   *  a disabled user.
   *
   *  `ttlSeconds` gives the token a fixed lifetime: it stops authenticating
   *  when that runs out, and the answer carries `expires_at`. It cannot run
   *  past the end of the token making the call. `name` labels it (stored as
   *  `u-<name>`).
   *  `maxActive` keeps at most that many of the user's tokens alive — the
   *  oldest beyond it are retired at once and listed in `retired`.
   *
   *  `agents` limits the token to starting those agents as top-level runs, and
   *  to the run endpoints: it is refused everywhere else (403
   *  `restricted_token`), and refused any other agent (403
   *  `agent_not_allowed`). An agent that a listed one spawns is not limited.
   *  An empty list is refused; omit the option for no limit.
   *
   *  `overrides` locks the per-run settings for runs the token starts, and
   *  for retunes it makes: `"none"` locks them all (model, provider, tier,
   *  effort, sampling, limits, output format and the rest), a list names the
   *  ones it may still set. A request that sets a locked one is refused with
   *  403 `override_not_allowed`.
   *
   *  `rate` bounds how often the token may start runs: `perMinute` runs a
   *  minute, with `burst` allowed at once from idle (default: `perMinute`).
   *  A start past the rate is refused with 429 `rate_limited` and a
   *  `Retry-After` header.
   *
   *  With none of the options the request has no body, as before. */
  async mintUserToken(
    subject: string,
    opts?: {
      signal?: AbortSignal;
      ttlSeconds?: number;
      name?: string;
      maxActive?: number;
      agents?: string[];
      overrides?: "none" | string[];
      rate?: { perMinute: number; burst?: number };
      /** `"none"`: every run the token starts is a no-trace run, whatever
       *  the request or the agent says. */
      retention?: "none";
    },
  ): Promise<MintedUserToken> {
    const body: Record<string, unknown> = {};
    if (opts?.ttlSeconds !== undefined) body.ttl_seconds = opts.ttlSeconds;
    if (opts?.name !== undefined) body.name = opts.name;
    if (opts?.maxActive !== undefined) body.max_active = opts.maxActive;
    if (opts?.agents !== undefined) body.agents = opts.agents;
    if (opts?.overrides !== undefined) body.overrides = opts.overrides;
    if (opts?.rate !== undefined) {
      body.rate = { per_minute: opts.rate.perMinute, burst: opts.rate.burst };
    }
    if (opts?.retention !== undefined) body.retention = opts.retention;
    return postJSON<MintedUserToken>(
      this.ctx,
      `/v1/_users/${encodeURIComponent(subject)}/tokens`,
      Object.keys(body).length > 0 ? body : undefined,
      { signal: opts?.signal },
    );
  }

  /** List the caller's own user's delegated tokens — METADATA ONLY (never the
   *  plaintext or hash). `active` applies the auth-layer validity rule so a
   *  revoked token reads as inactive. */
  async listUserTokens(
    subject: string,
    opts?: { signal?: AbortSignal },
  ): Promise<ListUserTokensResponse> {
    return jsonFetch<ListUserTokensResponse>(
      this.ctx,
      `/v1/_users/${encodeURIComponent(subject)}/tokens`,
      opts,
    );
  }

  /** Revoke (retire, immediate) one of the caller's own user's delegated
   *  tokens by def_id. A def_id that is not this (tenant, subject)'s member
   *  token is an opaque 404. Returns {def_id, retired_at}. */
  async revokeUserToken(
    subject: string,
    defId: string,
    opts?: { signal?: AbortSignal },
  ): Promise<{ def_id: string; retired_at: string }> {
    return deleteJSON<{ def_id: string; retired_at: string }>(
      this.ctx,
      `/v1/_users/${encodeURIComponent(subject)}/tokens/${encodeURIComponent(defId)}`,
      opts,
    );
  }

  /** List the agents the caller may run (RFC BY, v1.51.0+), tiered by access
   *  mode: bundled/system always, the tenant's shared agents for a non-isolated
   *  caller, own reserved. Member-read (runs:read), so a delegated user token
   *  reaches it — the tiering is decided server-side from the caller's access
   *  mode. Lean entries (name + source); operator metadata stays in the
   *  Library. */
  async runnableAgents(opts?: {
    signal?: AbortSignal;
  }): Promise<RunnableAgentsResponse> {
    return jsonFetch<RunnableAgentsResponse>(
      this.ctx,
      "/v1/_runnable-agents",
      opts,
    );
  }

  /** Whoami — the authenticated principal (RFC L v0.17.0). Any
   *  authenticated bearer; returns its authoritative tenant / subject /
   *  scopes + `is_admin`. `open_mode: true` when the server runs without
   *  the token substrate (single shared LOOMCYCLE_AUTH_TOKEN). */
  async whoami(opts?: { signal?: AbortSignal }): Promise<WhoamiResponse> {
    return jsonFetch<WhoamiResponse>(this.ctx, "/v1/_me", opts);
  }

  // ---- v0.8.17/8.18 Pause / Resume / State ----

  /** Quiesce the runtime: new runs are refused and each in-flight run
   *  parks at its next iteration boundary, after the model or tool call it
   *  is in finishes — no tool call is cancelled. Waits up to `timeoutMs`
   *  (default 30 s, max 5 min) for runs to park; any that did not are named
   *  in `warnings`. Raises AlreadyPausingError on 409,
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

  /** Trigger an immediate re-probe of every configured provider and
   *  return the refreshed availability matrix. Operator-only escape
   *  hatch when a transient outage stalls every provider and the
   *  runtime would otherwise 503 until the next periodic probe.
   *  Mirrors POST /v1/_resolve/probe. */
  async resolveProbe(opts?: {
    signal?: AbortSignal;
  }): Promise<ResolverMatrix> {
    return postJSON<ResolverMatrix>(
      this.ctx,
      "/v1/_resolve/probe",
      undefined,
      opts,
    );
  }

  /** The configured model aliases — the operator's top-level `models:` map,
   *  alias name → `{provider, model, kind?}` (`GET /v1/_models`). Non-secret and
   *  global (the same set for every tenant); tenant-readable. Store an alias on
   *  an agent or fork rather than the concrete model it names, and the agent
   *  follows when the operator retargets the alias. */
  async listModels(opts?: { signal?: AbortSignal }): Promise<ModelAliasesResponse> {
    return jsonFetch<ModelAliasesResponse>(this.ctx, "/v1/_models", opts);
  }

  /** For each user tier × tier, the ordered provider/model cascade a run
   *  resolves to now, top first, with live availability and the candidate
   *  `selected` (`GET /v1/_routing`). Tenant-readable. A non-admin caller gets
   *  `admin: false`, no raw `last_error`, and — when the operator-key gate
   *  restricts it — only the providers it can key itself
   *  (`operator_key_restricted: true`). */
  async getRouting(opts?: { signal?: AbortSignal }): Promise<RoutingResponse> {
    return jsonFetch<RoutingResponse>(this.ctx, "/v1/_routing", opts);
  }

  /** List the models a provider serves right now, by a live round-trip to it
   *  (`GET /v1/_providers/{id}/models`) rather than the cached probe matrix.
   *
   *  **Requires an admin token.** A `substrate:tenant` token is refused with a
   *  {@link LoomcycleError} whose `status` is 403 ("insufficient scope"); a
   *  model picker built on a tenant token should fall back to
   *  {@link LoomcycleClient.listModels} plus free-text model entry.
   *
   *  Errors carry the server's code on `e.code`: 404 `provider_unknown` (no such
   *  driver), 503 `provider_unavailable` (known, but not configured — e.g. no
   *  key), 502 `provider_list_failed` (the round-trip failed; bounded at 10 s
   *  server-side). */
  async listProviderModels(
    providerId: string,
    opts?: { signal?: AbortSignal },
  ): Promise<ProviderModelsResponse> {
    return jsonFetch<ProviderModelsResponse>(
      this.ctx,
      `/v1/_providers/${encodeURIComponent(providerId)}/models`,
      opts,
    );
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

  /** Open a client-tool host (RFC BC): a persistent WebSocket over which this
   *  client registers tools it runs on the user's machine + answers the
   *  agent's tool calls. Returns a started {@link ClientToolHost}; call
   *  `.close()` to stop. Uses the global WebSocket (browsers, Node 22+); pass
   *  `WebSocketImpl` (e.g. the `ws` package) on older Node. */
  connectClientTools(opts: ConnectClientToolsOptions): ClientToolHost {
    const host = new ClientToolHost(clientToolsURL(this.ctx.baseUrl), this.ctx.authToken, opts);
    host.start();
    return host;
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

  /** memoryFocusQuery renders the super-admin tenant focus for the memory
   *  browse routes.
   *
   *  Only a super-admin's focus widens; the server IGNORES the value for a
   *  tenant-scoped principal rather than honouring-then-checking it, so sending
   *  it is always safe and never escalates. Omitting it resolves to the
   *  caller's own tenant, which is what every call did before this existed. */
  private static memoryFocusQuery(tenant?: string): URLSearchParams {
    const params = new URLSearchParams();
    if (tenant && tenant.trim() !== "") params.set("tenant", tenant.trim());
    return params;
  }

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
    opts?: { tenant?: string; signal?: AbortSignal },
  ): Promise<MemoryScopeIDsResponse> {
    const qs = LoomcycleClient.memoryFocusQuery(opts?.tenant).toString();
    return jsonFetch<MemoryScopeIDsResponse>(
      this.ctx,
      `/v1/_memory/scopes/${encodeURIComponent(scope)}${qs ? "?" + qs : ""}`,
      opts,
    );
  }

  /** List memory entries under a (scope, scope_id) tuple.
   *  Optional prefix narrows by key prefix. */
  async listMemoryEntries(
    scope: string,
    scopeID: string,
    opts?: { prefix?: string; limit?: number; tenant?: string; signal?: AbortSignal },
  ): Promise<MemoryEntriesResponse> {
    const params = LoomcycleClient.memoryFocusQuery(opts?.tenant);
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
    opts?: { tenant?: string; signal?: AbortSignal },
  ): Promise<MemoryEntryResponse> {
    const qs = LoomcycleClient.memoryFocusQuery(opts?.tenant).toString();
    return jsonFetch<MemoryEntryResponse>(
      this.ctx,
      `/v1/_memory/scopes/${encodeURIComponent(scope)}/${encodeURIComponent(scopeID)}/keys/${encodeURIComponent(key)}${qs ? "?" + qs : ""}`,
      opts,
    );
  }

  // ---- RFC BV memory-view: off-run search + embed-admin reads ----

  /** Off-run unified semantic search over one scope's memory
   *  (POST /v1/_memory/search). Spans BOTH plain k/v entries AND
   *  document-chunk bodies in one ranked list — each hit is tagged
   *  `kind: "memory" | "document"`, and a document hit carries
   *  `chunk_id` so the caller can fetch its entity block via
   *  document({ op: "get_chunk" }). The in-band Memory tool's `search`
   *  op is prefix-scoped + run-bound; this is its off-run twin for the
   *  admin/operator surface. HTTP-only (no gRPC RPC). `rank` / `dedup`
   *  are opaque ranking-config objects passed through as-is. */
  async memorySearch(
    input: MemorySearchInput,
    opts?: { signal?: AbortSignal },
  ): Promise<MemorySearchResponse> {
    const body: Record<string, unknown> = {
      query: input.query,
      scope: input.scope,
      scope_id: input.scopeId,
    };
    // Only send top_k when set — the server defaults + caps it (1..50).
    if (input.topK !== undefined) body.top_k = input.topK;
    if (input.rank !== undefined) body.rank = input.rank;
    if (input.dedup !== undefined) body.dedup = input.dedup;
    // Sent only when set, so an omitted selector keeps the endpoint's
    // span-everything default rather than encoding one here (RFC BW).
    if (input.sources !== undefined) body.sources = input.sources;
    // Sent only when set: an absent predicate leaves the search untouched, and
    // the server rejects a malformed one rather than silently ignoring it.
    if (input.when !== undefined) body.when = input.when;
    return postJSON<MemorySearchResponse>(this.ctx, "/v1/_memory/search", body, {
      signal: opts?.signal,
    });
  }

  /** Per-(provider, model, dimension) row counts + total embedding
   *  bytes for one scope (GET /v1/_memory/embed_stats). The memory-view
   *  console shows this to spot a multi-embedder scope BEFORE a reembed
   *  migration. */
  async memoryEmbedStats(
    scope: string,
    opts?: { signal?: AbortSignal },
  ): Promise<MemoryEmbedStatsResponse> {
    return jsonFetch<MemoryEmbedStatsResponse>(
      this.ctx,
      `/v1/_memory/embed_stats?scope=${encodeURIComponent(scope)}`,
      opts,
    );
  }

  /** Re-embed a scope's rows whose stored embedder differs from the
   *  configured one (POST /v1/_memory/reembed). `dry_run` defaults TRUE
   *  server-side, so an omitted `dryRun` returns the planned migration
   *  WITHOUT writing; pass `dryRun: false` to commit. `limit` caps the
   *  rows processed per call (server default 1000). The response is a
   *  discriminated union on `dry_run` — narrow before reading its
   *  arm-specific fields. */
  // backfillEmbeddings embeds rows that carry none — what an embedder added AFTER the
  // rows were written leaves behind.
  //
  // dry_run defaults TRUE server-side and this only ever sends `false` to commit, the
  // same posture as reembedMemory: an omitted flag cannot accidentally spend thousands
  // of embedder calls.
  async backfillEmbeddings(
    scope: string,
    scopeId: string,
    opts?: { dryRun?: boolean; limit?: number; prefix?: string; tenant?: string; signal?: AbortSignal },
  ): Promise<MemoryBackfillResponse> {
    const query: Record<string, string> = { scope, scope_id: scopeId };
    if (opts?.dryRun === false) query.dry_run = "false";
    if (opts?.limit !== undefined && opts.limit > 0) query.limit = String(opts.limit);
    if (opts?.prefix) query.prefix = opts.prefix;
    // An ADMIN token must name the tenant: memory rows are keyed on it, so omitting it
    // silently targets the default tenant and reports a truthful-looking zero.
    if (opts?.tenant !== undefined) query.tenant = opts.tenant;
    return postJSON<MemoryBackfillResponse>(
      this.ctx,
      "/v1/_memory/backfill_embeddings",
      undefined,
      { query, signal: opts?.signal },
    );
  }

  // purgeStaleEmbeddings drops embeddings from rows that have no indexable text.
  //
  // THIS ONE DELETES, so the dry-run default matters more than on its siblings — and a
  // zero `stale` with `truncated: true` means the scan was cut short, not that the scope
  // is clean.
  async purgeStaleEmbeddings(
    scope: string,
    scopeId: string,
    opts?: { dryRun?: boolean; limit?: number; prefix?: string; tenant?: string; signal?: AbortSignal },
  ): Promise<MemoryPurgeResponse> {
    const query: Record<string, string> = { scope, scope_id: scopeId };
    if (opts?.dryRun === false) query.dry_run = "false";
    if (opts?.limit !== undefined && opts.limit > 0) query.limit = String(opts.limit);
    if (opts?.prefix) query.prefix = opts.prefix;
    if (opts?.tenant !== undefined) query.tenant = opts.tenant;
    return postJSON<MemoryPurgeResponse>(
      this.ctx,
      "/v1/_memory/purge_stale_embeddings",
      undefined,
      { query, signal: opts?.signal },
    );
  }

  async reembedMemory(
    scope: string,
    scopeId: string,
    opts?: { dryRun?: boolean; limit?: number; signal?: AbortSignal },
  ): Promise<MemoryReembedResponse> {
    const query: Record<string, string> = { scope, scope_id: scopeId };
    // Only send dry_run=false to commit; an omitted flag leaves the
    // server-side default (true) so a caller can't accidentally reembed.
    if (opts?.dryRun === false) query.dry_run = "false";
    if (opts?.limit !== undefined && opts.limit > 0) {
      query.limit = String(opts.limit);
    }
    return postJSON<MemoryReembedResponse>(
      this.ctx,
      "/v1/_memory/reembed",
      undefined,
      { query, signal: opts?.signal },
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
   *  backend. Pass `disposition: "declined"` (or use
   *  {@link LoomcycleClient.cancelInterrupt}) to decline WITHOUT an
   *  answer so the waiting Question tool proceeds (RFC BH). */
  async resolveInterrupt(
    runId: string,
    interruptId: string,
    opts: ResolveInterruptOptions & { signal?: AbortSignal },
  ): Promise<unknown> {
    const body: Record<string, unknown> = {
      kind: opts.kind ?? "question",
      resolved_by: opts.resolvedBy ?? "client",
    };
    // A decline carries no answer; omit it rather than sending "" so the
    // server's "declined must not carry an answer" gate isn't tripped.
    if (opts.answer !== undefined) body.answer = opts.answer;
    if (opts.disposition !== undefined) body.disposition = opts.disposition;
    return postJSON<unknown>(
      this.ctx,
      `/v1/runs/${encodeURIComponent(runId)}/interrupts/${encodeURIComponent(interruptId)}/resolve`,
      body,
      opts,
    );
  }

  /** Decline a pending Interruption.ask WITHOUT answering it (RFC BH) — the
   *  waiting Question tool returns a non-error "declined" result so the agent
   *  proceeds, and the run keeps going. A thin wrapper over
   *  {@link LoomcycleClient.resolveInterrupt} with `disposition: "declined"`
   *  (no answer). Mirrors POST .../resolve `{ disposition: "declined" }`. */
  async cancelInterrupt(
    runId: string,
    interruptId: string,
    opts?: { resolvedBy?: string; signal?: AbortSignal },
  ): Promise<unknown> {
    return this.resolveInterrupt(runId, interruptId, {
      disposition: "declined",
      resolvedBy: opts?.resolvedBy,
      signal: opts?.signal,
    });
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

  /** Register (or refresh) a code-js agent idempotently from an INLINE JS body
   *  — the typed "ship a code agent with no host filesystem" convenience
   *  (RFC J), mirror of {@link LoomcycleClient.ensureMcpServer}.
   *
   *  Runs `agentDef({op:"create", overlay:{provider:"code-js", code_body, …}})`,
   *  which is content-addressed-idempotent in loomcycle: a byte-identical
   *  re-registration is a no-op (`changed:false`), not a new version. This is
   *  what makes code agents work across container boundaries / pure-cloud —
   *  the JS no longer has to exist at `agent_code/<name>/index.js` on the
   *  sidecar's disk.
   *
   *  Requires `LOOMCYCLE_CODE_AGENTS_ENABLED=1` on the sidecar — the same
   *  switch that registers the code-js provider. Without it the create is
   *  refused (raises {@link SubstrateToolRefusedError}). Keep `${run.*}` /
   *  `${LOOMCYCLE_*}` placeholders in the body LITERAL so the content stays
   *  stable and dedup engages on re-register. */
  async ensureCodeAgent(
    opts: EnsureCodeAgentOptions,
    callOpts?: { signal?: AbortSignal },
  ): Promise<EnsureCodeAgentResult> {
    const overlay: AgentDefOverlay = { provider: "code-js", code_body: opts.code };
    if (opts.tools) overlay.tools = opts.tools;
    if (opts.tier) overlay.tier = opts.tier;
    if (opts.model) overlay.model = opts.model;
    const input: SubstrateToolInput = { op: "create", name: opts.name, overlay };
    if (opts.description) input.description = opts.description;

    const row = (await this.agentDef(input, callOpts)) as AgentDefRowResponse;
    return {
      name: opts.name,
      defId: row.def_id,
      version: row.version,
      changed: row.deduplicated !== true,
    };
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
   *  callable from any agent's `tools` list without a yaml
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
   *  - URL hostname must be in LOOMCYCLE_HTTP_HOST_ALLOWLIST or (since
   *    v0.17.x) LOOMCYCLE_HTTP_PRIVATE_HOST_ALLOWLIST — the latter is where
   *    a self-hosted loopback callback like `http://localhost:3000/api/mcp`
   *    belongs (SSRF defence at the registration boundary).
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

  /** Typed `MCPServerDef verify` — "is `contentSha256` the active version's
   *  hash for `name`?" `matches: true` is the no-op signal (the analog of
   *  the agent/skill verify-before-create dedup). Thin typed wrapper over
   *  {@link mcpServerDef}. */
  async mcpServerDefVerify(
    name: string,
    contentSha256: string,
    opts?: { signal?: AbortSignal },
  ): Promise<MCPServerDefVerifyResult> {
    return (await this.mcpServerDef(
      { op: "verify", name, content_sha256: contentSha256 },
      opts,
    )) as MCPServerDefVerifyResult;
  }

  /** Register (or refresh) a dynamic MCP server idempotently — the typed
   *  "register-if-changed" convenience for a consumer that re-registers its
   *  own callback server on every startup.
   *
   *  Runs `create`, which is content-addressed-idempotent (a byte-identical
   *  re-registration is a no-op, not a new version) AND auto-discovers the
   *  upstream's tools at ingestion — so {@link EnsureMcpServerResult.changed}
   *  is false on a stable-content re-register, and
   *  {@link EnsureMcpServerResult.discoveredToolCount} is populated straight
   *  from the `create` (no separate rediscover needed for the count). On a
   *  deduplicated re-register the count is absent (the tool surface was
   *  unchanged), so it reflects only the freshly-discovered case.
   *
   *  Pass {@link EnsureMcpServerOptions.rediscover} only to FORCE a refresh
   *  when the upstream's tools changed but the registration content didn't.
   *
   *  Keep `${run.*}` / `${LOOMCYCLE_*}` header placeholders LITERAL (don't
   *  resolve a per-restart token) or the content varies each boot and dedup
   *  can't engage. */
  async ensureMcpServer(
    opts: EnsureMcpServerOptions,
    callOpts?: { signal?: AbortSignal },
  ): Promise<EnsureMcpServerResult> {
    const overlay: Record<string, unknown> = {
      transport: opts.transport ?? "http",
      url: opts.url,
    };
    if (opts.headers) overlay.headers = opts.headers;
    const createInput: SubstrateToolInput = { op: "create", name: opts.name, overlay };
    if (opts.description) createInput.description = opts.description;

    const created = (await this.mcpServerDef(createInput, callOpts)) as MCPServerDefRowResponse;
    let row = created;
    let changed = created.deduplicated !== true;
    // create auto-discovers at ingestion → the count is on the create response.
    let discoveredToolCount = created.discovered;

    if (opts.rediscover) {
      const red = (await this.mcpServerDef(
        { op: "rediscover", name: opts.name },
        callOpts,
      )) as MCPServerDefRowResponse;
      row = red;
      if (red.deduplicated !== true) changed = true;
      if (red.discovered !== undefined) discoveredToolCount = red.discovered;
    }

    const result: EnsureMcpServerResult = {
      name: opts.name,
      defId: row.def_id,
      version: row.version,
      changed,
    };
    if (discoveredToolCount !== undefined) result.discoveredToolCount = discoveredToolCount;
    return result;
  }

  /** Invoke the v1.x RFC E ScheduleDef substrate tool over HTTP.
   *  Author + fork + retire scheduled-run definitions at runtime.
   *  Mirror of {@link LoomcycleClient.agentDef} for schedules — the
   *  primary use case is JobEmber-style "fork a yaml template
   *  per-user with their bearer + tier cron" workflows.
   *
   *  Operator-admin-only: this endpoint requires the bearer token.
   *
   *  Op-discriminated input: `{op: "create" | "fork" | "get" |
   *  "list" | "retire", ...}`. Note that ScheduleDef has 5 ops
   *  (no separate `promote` — forks auto-promote by default per
   *  RFC E's worked example; no `verify` — no content_sha256 in
   *  v1.x).
   *
   *  Sharp edges (substrate refuses these):
   *  - Name colliding with a static cfg.ScheduledRuns entry is
   *    refused on `create` (yaml is ground truth; use `fork` to
   *    derive a new version).
   *  - Fork against a template with `required_credentials` must
   *    supply all keys in `user_credentials` — loud-fail at fork
   *    time rather than silent ingestion-time failure when the
   *    sweeper fires.
   *  - Cron syntax is validated server-side; invalid expressions
   *    refuse with a parse error.
   *
   *  Raises {@link SubstrateToolRefusedError} on tool-level refusals
   *  (static-name collision, missing required credential, invalid
   *  cron, etc.); {@link InvalidArgumentError} on 400 (malformed
   *  JSON); {@link AuthError} on 401.
   *
   *  Annotate the input as {@link ScheduleDefInput} to have the overlay and
   *  each op's fields checked (including `add_hook` / `remove_hook`, which the
   *  loose {@link SubstrateToolInput} does not list); a loose input is sent
   *  as it is. For create / fork, {@link LoomcycleClient.createScheduleDef} /
   *  {@link LoomcycleClient.forkScheduleDef} check a bare literal too. */
  async scheduleDef(
    input: ScheduleDefInput | SubstrateToolInput,
    opts?: { signal?: AbortSignal },
  ): Promise<SubstrateToolResponse> {
    return postJSON<SubstrateToolResponse>(this.ctx, "/v1/_scheduledef", input, opts);
  }

  /** `scheduleDef({op: "create"})` with the overlay typed and no loose
   *  fallback. {@link ScheduleDefOverlay} stays OPEN (the server drops keys it
   *  does not know), so a misspelt key still compiles here; what fails is a
   *  wrong type or value on a key it declares (`concurrency_policy: "skip"`,
   *  an `on_complete` entry missing its `tool`). New versions are promoted
   *  unless `promote: false`. */
  async createScheduleDef(
    name: string,
    overlay: ScheduleDefOverlay,
    opts?: CreateDefOptions,
  ): Promise<ScheduleDefRowResponse> {
    return (await this.scheduleDef(
      defWriteInput("create", name, overlay, opts),
      signalOpts(opts),
    )) as ScheduleDefRowResponse;
  }

  /** `scheduleDef({op: "fork"})`, typed like
   *  {@link LoomcycleClient.createScheduleDef}. A field the overlay leaves out
   *  keeps the parent's; the credential maps merge key by key. Forks the
   *  active version of `name` (or its yaml template) unless `parentDefId` is
   *  given; promoted unless `promote: false`. */
  async forkScheduleDef(
    name: string,
    overlay: ScheduleDefOverlay,
    opts?: ForkDefOptions,
  ): Promise<ScheduleDefRowResponse> {
    return (await this.scheduleDef(
      defWriteInput("fork", name, overlay, opts),
      signalOpts(opts),
    )) as ScheduleDefRowResponse;
  }

  /** Invoke the v1.x RFC G A2AServerCardDef substrate tool over HTTP.
   *  Author + fork + retire A2A server-card definitions at runtime.
   *  Mirror of {@link LoomcycleClient.scheduleDef} for A2A server cards.
   *
   *  Operator-admin-only: this endpoint requires the bearer token.
   *
   *  Op-discriminated input: `{op: "create" | "fork" | "get" |
   *  "list" | "retire", ...}`.
   *
   *  Raises {@link SubstrateToolRefusedError} on tool-level refusals;
   *  {@link InvalidArgumentError} on 400 (malformed JSON);
   *  {@link AuthError} on 401. */
  async a2aServerCardDef(
    input: SubstrateToolInput,
    opts?: { signal?: AbortSignal },
  ): Promise<SubstrateToolResponse> {
    return postJSON<SubstrateToolResponse>(this.ctx, "/v1/_a2aservercarddef", input, opts);
  }

  /** Invoke the v1.x RFC G A2AAgentDef substrate tool over HTTP.
   *  Author + fork + retire A2A agent definitions at runtime.
   *  Mirror of {@link LoomcycleClient.scheduleDef} for A2A agents.
   *
   *  Operator-admin-only: this endpoint requires the bearer token.
   *
   *  Op-discriminated input: `{op: "create" | "fork" | "get" |
   *  "list" | "retire", ...}`.
   *
   *  Raises {@link SubstrateToolRefusedError} on tool-level refusals;
   *  {@link InvalidArgumentError} on 400 (malformed JSON);
   *  {@link AuthError} on 401. */
  async a2aAgentDef(
    input: SubstrateToolInput,
    opts?: { signal?: AbortSignal },
  ): Promise<SubstrateToolResponse> {
    return postJSON<SubstrateToolResponse>(this.ctx, "/v1/_a2aagentdef", input, opts);
  }

  /** Invoke the v1.x RFC H WebhookDef substrate tool over HTTP.
   *  Author + fork + retire inbound webhook definitions at runtime.
   *  Mirror of {@link LoomcycleClient.a2aAgentDef} for inbound webhooks.
   *
   *  Operator-admin-only: this endpoint requires the bearer token.
   *
   *  Op-discriminated input: `{op: "create" | "fork" | "get" |
   *  "list" | "retire", ...}`.
   *
   *  Raises {@link SubstrateToolRefusedError} on tool-level refusals;
   *  {@link InvalidArgumentError} on 400 (malformed JSON);
   *  {@link AuthError} on 401.
   *
   *  Annotate the input as {@link WebhookDefInput} to have the overlay and
   *  each op's fields checked; a loose input is sent as it is. For create /
   *  fork, {@link LoomcycleClient.createWebhookDef} /
   *  {@link LoomcycleClient.forkWebhookDef} check a bare literal too. */
  async webhookDef(
    input: WebhookDefInput | SubstrateToolInput,
    opts?: { signal?: AbortSignal },
  ): Promise<SubstrateToolResponse> {
    return postJSON<SubstrateToolResponse>(this.ctx, "/v1/_webhookdef", input, opts);
  }

  /** `webhookDef({op: "create"})` with the overlay typed and no loose
   *  fallback. {@link WebhookDefOverlay} stays OPEN for now (the server drops
   *  keys it does not know), so a misspelt key still compiles here; what
   *  fails is a wrong type or value on a key it declares (`delivery: "run"`,
   *  `auth.kind: "basic"`, a misspelt key inside `rate_limit`). Promoted
   *  unless `promote: false`. */
  async createWebhookDef(
    name: string,
    overlay: WebhookDefOverlay,
    opts?: CreateDefOptions,
  ): Promise<WebhookDefRowResponse> {
    return (await this.webhookDef(
      defWriteInput("create", name, overlay, opts),
      signalOpts(opts),
    )) as WebhookDefRowResponse;
  }

  /** `webhookDef({op: "fork"})`, typed like
   *  {@link LoomcycleClient.createWebhookDef}. A field the overlay leaves out
   *  keeps the parent's; naming exactly one of `agent` / `channel` / `team`
   *  clears the other two. Forks the active version of `name` (or its yaml
   *  template) unless `parentDefId` is given; promoted unless
   *  `promote: false`. */
  async forkWebhookDef(
    name: string,
    overlay: WebhookDefOverlay,
    opts?: ForkDefOptions,
  ): Promise<WebhookDefRowResponse> {
    return (await this.webhookDef(
      defWriteInput("fork", name, overlay, opts),
      signalOpts(opts),
    )) as WebhookDefRowResponse;
  }

  /** Invoke the RFC I MR-3a MemoryBackendDef substrate tool over HTTP.
   *  Author + fork + retire named memory backend definitions at runtime.
   *  Mirror of {@link LoomcycleClient.webhookDef} for memory backends.
   *
   *  Operator-admin-only: this endpoint requires the bearer token.
   *
   *  Op-discriminated input: `{op: "create" | "fork" | "get" |
   *  "list" | "retire", ...}`.
   *
   *  Raises {@link SubstrateToolRefusedError} on tool-level refusals;
   *  {@link InvalidArgumentError} on 400 (malformed JSON);
   *  {@link AuthError} on 401. */
  async memoryBackendDef(
    input: SubstrateToolInput,
    opts?: { signal?: AbortSignal },
  ): Promise<SubstrateToolResponse> {
    return postJSON<SubstrateToolResponse>(this.ctx, "/v1/_memorybackenddef", input, opts);
  }

  /** DocumentSourceDef substrate ops (create/fork/get/list/retire) — RFC CE
   *  remote-document-source registration. Operator-admin only. Authors + forks
   *  named peer document sources the Document tool's set_remote/sync consume.
   *  Static document_sources.<name>: yaml stays immutable ground truth; this
   *  produces the derived layer. Mirrors POST /v1/_documentsourcedef. */
  async documentSourceDef(
    input: SubstrateToolInput,
    opts?: { signal?: AbortSignal },
  ): Promise<SubstrateToolResponse> {
    return postJSON<SubstrateToolResponse>(this.ctx, "/v1/_documentsourcedef", input, opts);
  }

  /** HookDef substrate ops (create/fork/get/list/promote/retire/verify/delete)
   *  — reusable hook definitions: the event a hook answers, the tools it
   *  matches, its body (code-js or a webhook URL), fail mode and timeout.
   *  Tenant-confined like agentDef. A HookDef fires on nothing by itself; an
   *  agent definition, a team definition or a run names it. Mirrors
   *  POST /v1/_hookdef.
   *
   *  Annotate the input as {@link HookDefInput} to have the overlay checked
   *  against the definition the server decodes STRICTLY — an unknown overlay
   *  key is then a compile error instead of a refusal. A loose
   *  {@link SubstrateToolInput} is sent as it is — including a bare literal
   *  with a misspelt key, which falls through to that loose shape. To have a
   *  bare literal checked, use {@link LoomcycleClient.createHookDef} /
   *  {@link LoomcycleClient.forkHookDef}. */
  async hookDef(
    input: HookDefInput | SubstrateToolInput,
    opts?: { signal?: AbortSignal },
  ): Promise<SubstrateToolResponse> {
    return postJSON<SubstrateToolResponse>(this.ctx, "/v1/_hookdef", input, opts);
  }

  /** `hookDef({op: "create"})` with no loose fallback: `overlay` is the
   *  CLOSED {@link HookDefOverlay}, so any key the server would refuse is a
   *  compile error, even in a bare literal. Every create mints a new version
   *  (use {@link LoomcycleClient.ensureHookDef} to register only on change);
   *  promoted unless `promote: false`. */
  async createHookDef(
    name: string,
    overlay: HookDefOverlay,
    opts?: CreateDefOptions,
  ): Promise<HookDefRowResponse> {
    return (await this.hookDef(
      defWriteInput("create", name, overlay, opts),
      signalOpts(opts),
    )) as HookDefRowResponse;
  }

  /** `hookDef({op: "fork"})`, closed like
   *  {@link LoomcycleClient.createHookDef}. Each top-level field given replaces
   *  the parent's; `null` clears one. Forks the active version of `name` in
   *  your tenant unless `parentDefId` is given. NOT promoted unless
   *  `promote: true` — authoring and putting in force are two steps. */
  async forkHookDef(
    name: string,
    overlay: HookDefForkOverlay,
    opts?: ForkDefOptions,
  ): Promise<HookDefRowResponse> {
    return (await this.hookDef(
      defWriteInput("fork", name, overlay, opts),
      signalOpts(opts),
    )) as HookDefRowResponse;
  }

  /** Register a HookDef idempotently — the register-on-every-boot
   *  convenience, sibling of {@link LoomcycleClient.ensureMcpServer}.
   *
   *  Unlike MCPServerDef and AgentDef, a HookDef `create` is NOT
   *  content-addressed: every create mints a new version. And the hash
   *  `verify` compares is computed over the server's own JSON encoding of the
   *  definition, which this client cannot reproduce byte for byte. So instead
   *  this reads the active version (`verify` for its def_id — it answers
   *  `deployed: false` rather than raising — then `get`) and compares its
   *  stored definition with `overlay`, both normalized the way the server
   *  normalizes (`fail_mode` defaults to `"open"`, an empty `match.tools`,
   *  description, header map or timeout is dropped). Equal → no write,
   *  `changed: false`. Otherwise `create` with the default promote, which
   *  makes the new version active.
   *
   *  The normalization only collapses what the server collapses, so a real
   *  change is never skipped; should the server come to normalize something
   *  more, the cost is a redundant version, not a lost one. The rationale
   *  in {@link EnsureHookDefOptions.description} is not part of the
   *  definition and does not count as a change. */
  async ensureHookDef(
    name: string,
    overlay: HookDefOverlay,
    opts?: EnsureHookDefOptions,
  ): Promise<EnsureHookDefResult> {
    const callOpts = signalOpts(opts);
    const verified = (await this.hookDef(
      { op: "verify", name },
      callOpts,
    )) as HookDefVerifyResult;
    if (verified.deployed && verified.current_def_id) {
      // By def_id, not name: the version verify reported, even if another
      // writer promotes a different one in between.
      const active = (await this.hookDef(
        { op: "get", def_id: verified.current_def_id },
        callOpts,
      )) as HookDefRowResponse;
      if (sameJSON(normalizeHookDef(active.definition), normalizeHookDef(overlay))) {
        return { name, defId: active.def_id, version: active.version, changed: false };
      }
    }
    const input: HookDefInput = { op: "create", name, overlay };
    if (opts?.description) input.description = opts.description;
    const row = (await this.hookDef(input, callOpts)) as HookDefRowResponse;
    return { name, defId: row.def_id, version: row.version, changed: true };
  }

  /** OperatorTokenDef substrate ops (create/rotate/retire/get/list) —
   *  RFC L OSS multi-tenant authorization. Operator-admin only. The
   *  token plaintext is returned ONCE in the create/rotate response.
   *  Mirrors POST /v1/_operatortokendef. */
  async operatorTokenDef(
    input: SubstrateToolInput,
    opts?: { signal?: AbortSignal },
  ): Promise<SubstrateToolResponse> {
    return postJSON<SubstrateToolResponse>(this.ctx, "/v1/_operatortokendef", input, opts);
  }

  // ---- RFC AH (v0.35.0) dynamic filesystem volumes ----

  /** Invoke the RFC AH VolumeDef substrate tool over HTTP. Provision +
   *  manage CONFINED, per-tenant filesystem volumes at runtime.
   *  Tenant-confined (ScopeTenant, not operator-admin-only): the runtime
   *  stamps the caller's authoritative tenant and opaque-404s cross-tenant.
   *
   *  Op-discriminated input. A VolumeDef is FLAT (a pointer to mutable
   *  on-disk state, not a versioned definition), so the op set is
   *  `{op: "create" | "get" | "list" | "delete" | "purge", ...}` — NOT the
   *  content-addressed retire/promote/fork of the other Def families:
   *  - `create` — `{op, name, mode}`. The runtime DERIVES the path inside the
   *    operator-blessed `dynamic_root` (`<root>/<tenant>/<name>`) and
   *    `MkdirAll`s it; you never supply a host path.
   *  - `delete` — non-destructive: drops the mapping, leaves files on disk.
   *  - `purge`  — destructive: drops the mapping AND `RemoveAll`s the tree.
   *
   *  Raises {@link SubstrateToolRefusedError} on tool-level refusals
   *  (collision with a static volume name, no `dynamic_root` configured,
   *  cross-tenant); {@link InvalidArgumentError} on 400; {@link AuthError}
   *  on 401. */
  async volumeDef(
    input: SubstrateToolInput,
    opts?: { signal?: AbortSignal },
  ): Promise<SubstrateToolResponse> {
    return postJSON<SubstrateToolResponse>(this.ctx, "/v1/_volumedef", input, opts);
  }

  // ---- Def name enumeration (GET /v1/_<family>def/names) ----
  //
  // Every name declared in one substrate family, each with its version
  // roll-up — what the op-dispatched `list` op cannot give (it lists the
  // versions of ONE name). Tenant-scoped server-side: a substrate:tenant token
  // sees only its own tenant's names; an admin sees all, or focuses one tenant
  // with `tenant` (ignored for a tenant token). The TeamDef family is
  // {@link LoomcycleClient.listTeams}.

  private listDefNames<T>(
    family: string,
    opts?: { tenant?: string; signal?: AbortSignal },
  ): Promise<T> {
    const q = opts?.tenant ? `?tenant=${encodeURIComponent(opts.tenant)}` : "";
    return jsonFetch<T>(this.ctx, `/v1/_${family}/names${q}`, opts);
  }

  /** Every AgentDef name (`GET /v1/_agentdef/names`). */
  async listAgentDefNames(opts?: { tenant?: string; signal?: AbortSignal }): Promise<DefNamesResponse> {
    return this.listDefNames<DefNamesResponse>("agentdef", opts);
  }

  /** Every SkillDef name (`GET /v1/_skilldef/names`). */
  async listSkillDefNames(opts?: { tenant?: string; signal?: AbortSignal }): Promise<DefNamesResponse> {
    return this.listDefNames<DefNamesResponse>("skilldef", opts);
  }

  /** Every HookDef name (`GET /v1/_hookdef/names`). */
  async listHookDefNames(opts?: { tenant?: string; signal?: AbortSignal }): Promise<DefNamesResponse> {
    return this.listDefNames<DefNamesResponse>("hookdef", opts);
  }

  /** Every MCPServerDef name (`GET /v1/_mcpserverdef/names`). */
  async listMcpServerDefNames(opts?: { tenant?: string; signal?: AbortSignal }): Promise<DefNamesResponse> {
    return this.listDefNames<DefNamesResponse>("mcpserverdef", opts);
  }

  /** Every ScheduleDef name (`GET /v1/_scheduledef/names`). Substrate only —
   *  {@link LoomcycleClient.listAllSchedules} merges in the static yaml ones. */
  async listScheduleDefNames(opts?: { tenant?: string; signal?: AbortSignal }): Promise<DefNamesResponse> {
    return this.listDefNames<DefNamesResponse>("scheduledef", opts);
  }

  /** Every A2AServerCardDef name (`GET /v1/_a2aservercarddef/names`). */
  async listA2AServerCardDefNames(opts?: { tenant?: string; signal?: AbortSignal }): Promise<DefNamesResponse> {
    return this.listDefNames<DefNamesResponse>("a2aservercarddef", opts);
  }

  /** Every A2AAgentDef name (`GET /v1/_a2aagentdef/names`). */
  async listA2AAgentDefNames(opts?: { tenant?: string; signal?: AbortSignal }): Promise<DefNamesResponse> {
    return this.listDefNames<DefNamesResponse>("a2aagentdef", opts);
  }

  /** Every WebhookDef name (`GET /v1/_webhookdef/names`). */
  async listWebhookDefNames(opts?: { tenant?: string; signal?: AbortSignal }): Promise<DefNamesResponse> {
    return this.listDefNames<DefNamesResponse>("webhookdef", opts);
  }

  /** Every MemoryBackendDef name (`GET /v1/_memorybackenddef/names`). */
  async listMemoryBackendDefNames(opts?: { tenant?: string; signal?: AbortSignal }): Promise<DefNamesResponse> {
    return this.listDefNames<DefNamesResponse>("memorybackenddef", opts);
  }

  /** Every DocumentSourceDef name (`GET /v1/_documentsourcedef/names`). */
  async listDocumentSourceDefNames(opts?: { tenant?: string; signal?: AbortSignal }): Promise<DefNamesResponse> {
    return this.listDefNames<DefNamesResponse>("documentsourcedef", opts);
  }

  /** Every OperatorTokenDef name (`GET /v1/_operatortokendef/names`), one row
   *  per (tenant, name), with its subject, token counts and current def_id —
   *  never a token. **Admin token only**: token
   *  minting stays operator-only, so a `substrate:tenant` token is refused
   *  with 403. */
  async listOperatorTokenDefNames(
    opts?: { tenant?: string; signal?: AbortSignal },
  ): Promise<OperatorTokenDefNamesResponse> {
    return this.listDefNames<OperatorTokenDefNamesResponse>("operatortokendef", opts);
  }

  // ---- Access control: groups, rule lists, bindings, decisions (RFC EY / EZ) ----
  //
  // Groups, lists and bindings are the tenant operator's: a `substrate:tenant`
  // token, an admin, or the legacy shared token. Any other principal is
  // refused with 403 (InsufficientScopeError). Every call acts in the caller's
  // own tenant; `tenant` is an admin's focus on another one and is ignored for
  // a tenant operator. Rows the config declares are listed beside the stored
  // ones with `source: "config"`; they cannot be written here. Every write
  // takes effect on every replica at once. A rule that refuses a call throws
  // AclDeniedError (403 `acl_denied`) naming the rules that decided.
  //
  // The decision questions (authzEvaluation / authzEvaluations / authzExplain
  // / authzSimulate) are open to any principal about itself; naming another
  // subject of the tenant needs a tenant operator (403 `authz_subject`).
  // Every call here needs a persistent store (503 otherwise) and is refused
  // from inside a run.

  /** `path` with the admin tenant focus and any extra query parameters. */
  private aclPath(path: string, tenant?: string, extra?: Record<string, string>): string {
    const q = new URLSearchParams(extra);
    if (tenant) q.set("tenant", tenant);
    const qs = q.toString();
    return qs ? `${path}?${qs}` : path;
  }

  /** Every group of the tenant, stored and config-declared, sorted by name
   *  (`GET /v1/_groups`). */
  async listGroups(opts?: { tenant?: string; signal?: AbortSignal }): Promise<GroupListResponse> {
    return jsonFetch<GroupListResponse>(this.ctx, this.aclPath("/v1/_groups", opts?.tenant), opts);
  }

  /** One stored group with its direct members and the groups it is inside of
   *  (`GET /v1/_groups/{name}`). A config-only group is not stored and raises
   *  NotFoundError; {@link LoomcycleClient.getGroupEffective} covers it. */
  async getGroup(name: string, opts?: { tenant?: string; signal?: AbortSignal }): Promise<GroupDetail> {
    return jsonFetch<GroupDetail>(
      this.ctx,
      this.aclPath(`/v1/_groups/${encodeURIComponent(name)}`, opts?.tenant),
      opts,
    );
  }

  /** Every user the group holds, through nested groups at any depth and
   *  config-declared members included (`GET /v1/_groups/{name}/effective`). */
  async getGroupEffective(
    name: string,
    opts?: { tenant?: string; signal?: AbortSignal },
  ): Promise<GroupEffective> {
    return jsonFetch<GroupEffective>(
      this.ctx,
      this.aclPath(`/v1/_groups/${encodeURIComponent(name)}/effective`, opts?.tenant),
      opts,
    );
  }

  /** Create a group, or update its description and `external_id` if the name
   *  exists (`POST /v1/_groups`). A group with an `external_id` is owned by an
   *  identity provider: a later write to it raises a 409 LoomcycleError with
   *  code `group_external`. */
  async createGroup(
    body: CreateGroupBody,
    opts?: { tenant?: string; signal?: AbortSignal },
  ): Promise<GroupRow> {
    return postJSON<GroupRow>(this.ctx, this.aclPath("/v1/_groups", opts?.tenant), body, opts);
  }

  /** Delete a group and every membership edge naming it
   *  (`DELETE /v1/_groups/{name}`). Bindings to it stay and apply to no one.
   *  NotFoundError when there is no such group. */
  async deleteGroup(
    name: string,
    opts?: { tenant?: string; signal?: AbortSignal },
  ): Promise<{ deleted: string }> {
    return deleteJSON<{ deleted: string }>(
      this.ctx,
      this.aclPath(`/v1/_groups/${encodeURIComponent(name)}`, opts?.tenant),
      opts,
    );
  }

  /** Add a user or a group to a group (`POST /v1/_groups/{name}/members`). A
   *  group member nests: its members are in this group too. The server
   *  refuses an edge that would close a cycle or nest too deep (400), one
   *  that exists (409 `group_member_exists`), and a delegated writer adding
   *  itself (403 `acl_self_elevation`). */
  async addGroupMember(
    name: string,
    member: { kind: GroupMemberKind; id: string },
    opts?: { tenant?: string; signal?: AbortSignal },
  ): Promise<GroupMemberChange> {
    return postJSON<GroupMemberChange>(
      this.ctx,
      this.aclPath(`/v1/_groups/${encodeURIComponent(name)}/members`, opts?.tenant),
      { kind: member.kind, id: member.id },
      opts,
    );
  }

  /** Remove one membership edge
   *  (`DELETE /v1/_groups/{name}/members/{kind}/{id}`). NotFoundError when
   *  there is no such edge. */
  async removeGroupMember(
    name: string,
    kind: GroupMemberKind,
    id: string,
    opts?: { tenant?: string; signal?: AbortSignal },
  ): Promise<GroupMemberChange> {
    const path = `/v1/_groups/${encodeURIComponent(name)}/members/${encodeURIComponent(kind)}/${encodeURIComponent(id)}`;
    return deleteJSON<GroupMemberChange>(this.ctx, this.aclPath(path, opts?.tenant), opts);
  }

  /** The AclDef family, op-dispatched — the same ops the gRPC `AclDef` RPC
   *  and the MCP `acldef` tool take. Every op but `names` posts to
   *  `POST /v1/_acldef`; `names` reads `GET /v1/_acldef/names`, which is
   *  where that op lives on HTTP. The response varies by op; the typed
   *  helpers below return each op's shape. */
  async aclDef(
    input: AclDefInput,
    opts?: { tenant?: string; signal?: AbortSignal },
  ): Promise<unknown> {
    if (input.op === "names") return this.listAclDefNames(opts);
    return postJSON<unknown>(this.ctx, this.aclPath("/v1/_acldef", opts?.tenant), input, opts);
  }

  /** Every rule list of the tenant, stored and config-declared, plus the
   *  built-in baselines (`aclDef` op `list`). Retired lists only with
   *  `includeRetired`. */
  async listAclDefs(opts?: {
    includeRetired?: boolean;
    tenant?: string;
    signal?: AbortSignal;
  }): Promise<AclDefListResponse> {
    const input: AclDefInput = { op: "list" };
    if (opts?.includeRetired) input.include_retired = true;
    return (await this.aclDef(input, opts)) as AclDefListResponse;
  }

  /** One rule list, retired or not (`aclDef` op `get`). A config-declared
   *  list is returned with `source: "config"`. NotFoundError when absent. */
  async getAclDef(name: string, opts?: { tenant?: string; signal?: AbortSignal }): Promise<AclDefRow> {
    return (await this.aclDef({ op: "get", name }, opts)) as AclDefRow;
  }

  /** Create or replace a rule list by name (`aclDef` op `put`): bumps its
   *  version and un-retires it. The rules are validated first; a list the
   *  engine could not enforce is refused with 400. A delegated writer may
   *  not rewrite a list bound to itself (403 `acl_self_elevation`). */
  async putAclDef(
    body: { name: string; rules: AclRule[]; description?: string },
    opts?: { tenant?: string; signal?: AbortSignal },
  ): Promise<AclDefRow> {
    const input: AclDefInput = { op: "put", name: body.name, rules: body.rules };
    if (body.description !== undefined) input.description = body.description;
    return (await this.aclDef(input, opts)) as AclDefRow;
  }

  /** Retire a rule list (`aclDef` op `retire`). Bindings to it stay and match
   *  nothing; a later `putAclDef` of the name brings it back. NotFoundError
   *  when there is no live list of that name. */
  async retireAclDef(
    name: string,
    opts?: { tenant?: string; signal?: AbortSignal },
  ): Promise<{ retired: string }> {
    return (await this.aclDef({ op: "retire", name }, opts)) as { retired: string };
  }

  /** Check rules without storing them (`aclDef` op `validate`). Rules the
   *  engine refuses raise InvalidArgumentError (code `acl_invalid`) naming
   *  the fault. */
  async validateAclRules(
    rules: AclRule[],
    opts?: { tenant?: string; signal?: AbortSignal },
  ): Promise<AclValidateResult> {
    return (await this.aclDef({ op: "validate", rules }, opts)) as AclValidateResult;
  }

  /** The resource types, verbs and transports a rule may name (`aclDef` op
   *  `catalog`). */
  async getAclCatalog(opts?: { tenant?: string; signal?: AbortSignal }): Promise<AclCatalog> {
    return (await this.aclDef({ op: "catalog" }, opts)) as AclCatalog;
  }

  /** Every live rule-list name a binding in the tenant can reach, stored and
   *  config-declared, sorted (`GET /v1/_acldef/names`). */
  async listAclDefNames(opts?: { tenant?: string; signal?: AbortSignal }): Promise<AclDefNamesResponse> {
    return jsonFetch<AclDefNamesResponse>(this.ctx, this.aclPath("/v1/_acldef/names", opts?.tenant), opts);
  }

  /** Every binding of the tenant, stored and config-declared
   *  (`GET /v1/_aclbinding`). Retired ones only with `includeRetired`. */
  async listAclBindings(opts?: {
    includeRetired?: boolean;
    tenant?: string;
    signal?: AbortSignal;
  }): Promise<AclBindingListResponse> {
    const extra = opts?.includeRetired ? { include_retired: "1" } : undefined;
    return jsonFetch<AclBindingListResponse>(this.ctx, this.aclPath("/v1/_aclbinding", opts?.tenant, extra), opts);
  }

  /** Bind a rule list to a subject (`POST /v1/_aclbinding`). The list must
   *  exist and `expires_at` must be in the future, or InvalidArgumentError
   *  (code `acl_binding_invalid`). A delegated writer may not bind itself, a
   *  group it is in, its token or its tenant (403 `acl_self_elevation`). An
   *  empty `subject_id` / `expires_at` is left out of the request. */
  async createAclBinding(
    body: CreateAclBindingBody,
    opts?: { tenant?: string; signal?: AbortSignal },
  ): Promise<AclBinding> {
    const out: CreateAclBindingBody = { acl: body.acl, subject_kind: body.subject_kind };
    if (body.subject_id) out.subject_id = body.subject_id;
    if (body.expires_at) out.expires_at = body.expires_at;
    return postJSON<AclBinding>(this.ctx, this.aclPath("/v1/_aclbinding", opts?.tenant), out, opts);
  }

  /** Retire a binding by its `binding_id` (`DELETE /v1/_aclbinding/{id}`).
   *  NotFoundError when there is no live binding of that id. A config
   *  binding has no id and cannot be retired here. */
  async deleteAclBinding(
    bindingId: string,
    opts?: { tenant?: string; signal?: AbortSignal },
  ): Promise<{ retired: string }> {
    return deleteJSON<{ retired: string }>(
      this.ctx,
      this.aclPath(`/v1/_aclbinding/${encodeURIComponent(bindingId)}`, opts?.tenant),
      opts,
    );
  }

  /** May the subject do this? (`POST /v1/_authz/evaluation`, AuthZEN 1.0).
   *  Omit `subject` to ask about yourself. */
  async authzEvaluation(
    req: AuthzEvaluationRequest,
    opts?: { tenant?: string; signal?: AbortSignal },
  ): Promise<AuthzDecision> {
    return postJSON<AuthzDecision>(this.ctx, this.aclPath("/v1/_authz/evaluation", opts?.tenant), req, opts);
  }

  /** Up to 256 questions for one subject in one call
   *  (`POST /v1/_authz/evaluations`); decisions come back in order. */
  async authzEvaluations(
    req: AuthzEvaluationsRequest,
    opts?: { tenant?: string; signal?: AbortSignal },
  ): Promise<AuthzEvaluationsResponse> {
    return postJSON<AuthzEvaluationsResponse>(
      this.ctx,
      this.aclPath("/v1/_authz/evaluations", opts?.tenant),
      req,
      opts,
    );
  }

  /** A decision with its working: every rule read for the resource's type
   *  and why it did or did not match (`POST /v1/_authz/explain`). */
  async authzExplain(
    req: AuthzEvaluationRequest,
    opts?: { tenant?: string; signal?: AbortSignal },
  ): Promise<AuthzExplainResponse> {
    return postJSON<AuthzExplainResponse>(this.ctx, this.aclPath("/v1/_authz/explain", opts?.tenant), req, opts);
  }

  /** What a draft of lists and bindings would decide, beside what is decided
   *  now (`POST /v1/_authz/simulate`). Nothing is written. */
  async authzSimulate(
    req: AuthzSimulateRequest,
    opts?: { tenant?: string; signal?: AbortSignal },
  ): Promise<AuthzSimulateResponse> {
    return postJSON<AuthzSimulateResponse>(this.ctx, this.aclPath("/v1/_authz/simulate", opts?.tenant), req, opts);
  }

  // ---- Schedules admin (GET/POST /v1/_schedules/...) ----
  //
  // What the Web UI's schedules page drives: the merged list, one schedule's
  // runtime state, and run-now / pause / resume. Authoring stays on
  // {@link LoomcycleClient.scheduleDef}. The per-schedule methods take the
  // ScheduleDef's `active_def_id`; a static yaml schedule has none to give, and
  // a tenant token reaching another tenant's def gets the same 404 as an
  // unknown one ({@link NotFoundError}).

  /** Every schedule: static yaml `scheduled_runs` and ScheduleDefs, merged by
   *  name (`GET /v1/_schedules/list-all`), sorted by name. A tenant token sees
   *  only its own tenant's ScheduleDefs — static schedules are operator
   *  automation and appear only for an admin, who may focus one tenant with
   *  `tenant`. */
  async listAllSchedules(
    opts?: { tenant?: string; signal?: AbortSignal },
  ): Promise<ScheduleListResponse> {
    const q = opts?.tenant ? `?tenant=${encodeURIComponent(opts.tenant)}` : "";
    return jsonFetch<ScheduleListResponse>(this.ctx, `/v1/_schedules/list-all${q}`, opts);
  }

  /** One schedule's runtime state (`GET /v1/_schedules/{def_id}/state`): last
   *  and next fire, pause, the last slot claim, the catch-up backlog and the
   *  runs it has in flight. 404 when the def has no state row (never promoted,
   *  retired, or not yours). */
  async getScheduleState(defId: string, opts?: { signal?: AbortSignal }): Promise<ScheduleStateView> {
    return jsonFetch<ScheduleStateView>(
      this.ctx,
      `/v1/_schedules/${encodeURIComponent(defId)}/state`,
      opts,
    );
  }

  /** Fire a schedule now (`POST /v1/_schedules/{def_id}/run-now`). The run
   *  starts on the scheduler's next tick, not during this call, and the regular
   *  cadence continues after it. A run already in flight is handled by the
   *  schedule's `concurrency_policy`. 404 for an unknown def. */
  async runScheduleNow(defId: string, opts?: { signal?: AbortSignal }): Promise<ScheduleRunNowResult> {
    return postJSON<ScheduleRunNowResult>(
      this.ctx,
      `/v1/_schedules/${encodeURIComponent(defId)}/run-now`,
      undefined,
      opts,
    );
  }

  /** Pause a schedule indefinitely (`POST /v1/_schedules/{def_id}/pause`) until
   *  {@link LoomcycleClient.resumeSchedule}. Takes no duration: the server sets
   *  `paused_until` about 100 years ahead. Distinct from
   *  {@link LoomcycleClient.pauseRuntime}, which stops the whole runtime. */
  async pauseSchedule(defId: string, opts?: { signal?: AbortSignal }): Promise<SchedulePauseResult> {
    return postJSON<SchedulePauseResult>(
      this.ctx,
      `/v1/_schedules/${encodeURIComponent(defId)}/pause`,
      undefined,
      opts,
    );
  }

  /** Clear a schedule's pause (`POST /v1/_schedules/{def_id}/resume`), so the
   *  scheduler considers it due again; `next_run_at` is left as it was. */
  async resumeSchedule(defId: string, opts?: { signal?: AbortSignal }): Promise<SchedulePauseResult> {
    return postJSON<SchedulePauseResult>(
      this.ctx,
      `/v1/_schedules/${encodeURIComponent(defId)}/resume`,
      undefined,
      opts,
    );
  }

  // ---- RFC AP Agent Teams (TeamDef substrate) ----
  //
  // TeamDefs are agent-team workflow graphs (states + transitions + a per-state
  // handler agent). These mirror the HTTP shapes the Web UI drives; the
  // substrate is tenant-confined (the runtime stamps the caller's authoritative
  // tenant, opaque-404s cross-tenant). All op-dispatch through POST /v1/_teamdef
  // except listTeams, which reads GET /v1/_teamdef/names.

  /** List every team the caller may see (GET /v1/_teamdef/names), each with its
   *  version roll-up. Tenant-scoped server-side. `names` is `null` when empty. */
  async listTeams(opts?: { signal?: AbortSignal }): Promise<ListTeamsResponse> {
    return jsonFetch<ListTeamsResponse>(this.ctx, "/v1/_teamdef/names", opts);
  }

  /** Render a team's active version as a Mermaid `stateDiagram-v2` (op=
   *  render_diagram). `highlightState` optionally marks one state with a bold
   *  outline (e.g. a chunk's current state). */
  async renderTeamDiagram(
    name: string,
    opts?: { signal?: AbortSignal; highlightState?: string },
  ): Promise<TeamDiagram> {
    const body: Record<string, unknown> = { op: "render_diagram", name };
    if (opts?.highlightState) body.highlight_state = opts.highlightState;
    return postJSON<TeamDiagram>(this.ctx, "/v1/_teamdef", body, opts);
  }

  /** Fetch one version's full record incl. the editable `definition` graph
   *  (op=get by def_id). */
  async getTeamDef(defId: string, opts?: { signal?: AbortSignal }): Promise<TeamDefDetail> {
    return postJSON<TeamDefDetail>(this.ctx, "/v1/_teamdef", { op: "get", def_id: defId }, opts);
  }

  /** Author a new TeamDef (op=create). `overlay` is the workflow graph
   *  (`{entry, states, transitions, max_iterations?, colors?}`), validated
   *  server-side — an invalid graph raises {@link SubstrateToolRefusedError}.
   *
   *  `opts.promote` and `opts.description` ride beside `signal`, as on
   *  {@link LoomcycleClient.forkTeam}; each is sent only when set. Omitted,
   *  the server promotes a create (unlike a fork); `promote: false` authors the
   *  version without putting it in force. */
  async createTeam(
    name: string,
    overlay: Record<string, unknown>,
    opts?: CreateTeamOptions,
  ): Promise<CreatedTeam> {
    const body: Record<string, unknown> = { op: "create", name, overlay };
    if (opts?.promote !== undefined) body.promote = opts.promote;
    if (opts?.description !== undefined) body.description = opts.description;
    return postJSON<CreatedTeam>(this.ctx, "/v1/_teamdef", body, { signal: opts?.signal });
  }

  /** Save an edited graph as a NEW version of an existing team (op=fork). With
   *  no `parentDefId` the server forks the name's active def; `overlay` (a full
   *  graph) replaces it. Validated server-side ({@link SubstrateToolRefusedError}).
   *
   *  The fork's own fields ride in `opts` beside `signal` — the pattern
   *  {@link LoomcycleClient.setRunBreakpoints} uses — so existing two- and
   *  three-argument calls are unchanged. `promote: true` puts the new version
   *  in force in the same call; omitted, it is authored but not active. */
  async forkTeam(
    name: string,
    overlay: Record<string, unknown>,
    opts?: ForkTeamOptions,
  ): Promise<CreatedTeam> {
    const body: Record<string, unknown> = { op: "fork", name, overlay };
    if (opts?.promote !== undefined) body.promote = opts.promote;
    if (opts?.parentDefId !== undefined) body.parent_def_id = opts.parentDefId;
    if (opts?.description !== undefined) body.description = opts.description;
    return postJSON<CreatedTeam>(this.ctx, "/v1/_teamdef", body, { signal: opts?.signal });
  }

  /** List every version of ONE team, newest first (op=list) — the lineage
   *  behind {@link LoomcycleClient.listTeams}' roll-up. Tenant-scoped
   *  server-side; a name with no versions returns an empty list rather than
   *  raising. */
  async listTeamVersions(
    name: string,
    opts?: { signal?: AbortSignal },
  ): Promise<TeamVersionList> {
    return postJSON<TeamVersionList>(this.ctx, "/v1/_teamdef", { op: "list", name }, opts);
  }

  /** Point the team's active pointer at one version (op=promote) — what a new
   *  run of the team by NAME will execute.
   *
   *  `forkTeam` defaults to promote:false, so authoring a version and putting
   *  it in force are deliberately two steps: an edited graph can be reviewed,
   *  diagrammed, even run by `defId`, before it becomes what the name means. */
  async promoteTeam(
    defId: string,
    opts?: { signal?: AbortSignal },
  ): Promise<PromotedTeam> {
    return postJSON<PromotedTeam>(this.ctx, "/v1/_teamdef", { op: "promote", def_id: defId }, opts);
  }

  /** Soft-retire one version, or un-retire it (op=retire, `retired` required).
   *
   *  Retiring is REVERSIBLE and version-scoped: the row stays, its history stays
   *  readable, and passing `false` brings it back. Removing a team is
   *  {@link LoomcycleClient.deleteTeam}, which is neither. */
  async retireTeam(
    defId: string,
    retired: boolean,
    opts?: { signal?: AbortSignal },
  ): Promise<RetiredTeam> {
    return postJSON<RetiredTeam>(
      this.ctx,
      "/v1/_teamdef",
      { op: "retire", def_id: defId, retired },
      opts,
    );
  }

  /** Compare a locally computed content hash against the deployed active
   *  version (op=verify) — the drift check for a team kept in source control
   *  and pushed to several deployments.
   *
   *  Never raises for an absent team: a name with no active version answers
   *  `{deployed: false, matches: false}`, which a caller distinguishes from a
   *  deployed version whose hash differs.
   *
   *  Given a {@link TeamDraft} instead of a hash, it checks that UNSAVED team
   *  the way `createTeam` / `forkTeam` would — every check they run, plus
   *  what would make it unrunnable — and returns all the problems at once,
   *  each with the JSON path at fault. Nothing is written. */
  async verifyTeam(
    name: string,
    contentSha256: string,
    opts?: { signal?: AbortSignal },
  ): Promise<TeamVerification>;
  async verifyTeam(
    name: string,
    draft: TeamDraft,
    opts?: { signal?: AbortSignal },
  ): Promise<TeamDraftVerification>;
  async verifyTeam(
    name: string,
    target: string | TeamDraft,
    opts?: { signal?: AbortSignal },
  ): Promise<TeamVerification | TeamDraftVerification> {
    const body: Record<string, unknown> = { op: "verify", name };
    if (typeof target === "string") {
      body.content_sha256 = target;
    } else {
      body.overlay = target.overlay;
      if (target.as) body.as = target.as;
      if (target.parentDefId) body.parent_def_id = target.parentDefId;
      if (target.description) body.description = target.description;
    }
    return postJSON<TeamVerification | TeamDraftVerification>(this.ctx, "/v1/_teamdef", body, opts);
  }

  /** Hard-remove a whole team by name — all versions + the active pointer
   *  (op=delete), scoped to the caller's tenant. Teams are runtime-only, so
   *  this is how an operator clears an obsolete/test team. */
  async deleteTeam(
    name: string,
    opts?: { signal?: AbortSignal },
  ): Promise<{ name: string; deleted: boolean }> {
    return postJSON<{ name: string; deleted: boolean }>(
      this.ctx,
      "/v1/_teamdef",
      { op: "delete", name },
      opts,
    );
  }

  /** Execute a team (op=run) — walk its state graph, spawning each state's agent
   *  until a terminal state, returning the per-state trace. Target the active
   *  version by `name` OR a specific version by `defId`. `input` is the initial
   *  task handed to the entry state's agent. The walk runs under the same
   *  admission a normal run gets (token budget / operator-key / depth). */
  async runTeam(
    target: TeamRunTarget & { mode: "detach" },
    opts?: { signal?: AbortSignal },
  ): Promise<TeamRunDetached>;
  async runTeam(
    target: TeamRunTarget & { mode?: undefined },
    opts?: { signal?: AbortSignal },
  ): Promise<TeamRunResult>;
  // A target whose `mode` is not known statically — one built from a form, a
  // config or a node parameter, typed plain TeamRunTarget — matches neither
  // overload above, and an implementation signature is not callable. This one
  // accepts it and answers the union, which the caller narrows by the `mode`
  // it sent (a deduplicated synchronous answer has no `steps` either).
  async runTeam(
    target: TeamRunTarget,
    opts?: { signal?: AbortSignal },
  ): Promise<TeamRunResult | TeamRunDetached>;
  async runTeam(
    target: TeamRunTarget,
    opts?: { signal?: AbortSignal },
  ): Promise<TeamRunResult | TeamRunDetached> {
    const body: Record<string, unknown> = { op: "run" };
    if (target.name !== undefined) body.name = target.name;
    if (target.defId !== undefined) body.def_id = target.defId;
    if (target.input !== undefined) body.input = target.input;
    if (target.vars !== undefined) body.vars = target.vars;
    if (target.boardChunkId !== undefined) body.board_chunk_id = target.boardChunkId;
    if (target.boardScope !== undefined) body.board_scope = target.boardScope;
    if (target.mode !== undefined) body.mode = target.mode;
    if (target.breakpoints !== undefined) body.breakpoints = target.breakpoints;
    if (target.review !== undefined) body.review = target.review;
    if (target.reviewTtlSeconds !== undefined) body.review_ttl_seconds = target.reviewTtlSeconds;
    if (target.idempotencyKey !== undefined) body.idempotency_key = target.idempotencyKey;
    if (target.interruptOnCap !== undefined) body.interrupt_on_cap = target.interruptOnCap;
    return postJSON<TeamRunResult | TeamRunDetached>(this.ctx, "/v1/_teamdef", body, opts);
  }

  /** Read the debug breakpoints armed on a live team walk
   *  (`GET /v1/runs/{run_id}/breakpoints`), and the review deadline its
   *  member holds get (`review_ttl_seconds`, 0 = none).
   *
   *  404 when no walk is in flight under that run on this replica — loudly,
   *  because an arming that appeared to succeed and then never paused anything
   *  is the worst possible outcome for a debugger. Surface it rather than
   *  treating it as "nothing armed". */
  async getRunBreakpoints(runId: string, opts?: { signal?: AbortSignal }): Promise<TeamBreakpoints> {
    return jsonFetch<TeamBreakpoints>(this.ctx, `/v1/runs/${encodeURIComponent(runId)}/breakpoints`, opts);
  }

  /** Replace the debug breakpoints armed on a live team walk
   *  (`PUT /v1/runs/{run_id}/breakpoints`).
   *
   *  The WHOLE desired set, not a delta: you hold the configuration and push
   *  it, so two operators cannot interleave a read-modify-write. `[]` is the
   *  off switch. A malformed entry is refused whole and leaves the previous
   *  arming exactly as it was.
   *
   *  `"wave"` (or `"wave:before_dispatch"`) pauses a starter state before it
   *  dispatches, and `"<state>:review"` holds the member runs of a starter,
   *  agent or parallel state for a verdict ({@link LoomcycleClient.reviewRun}).
   *  An arm takes effect at the next point the walk reaches; disarming
   *  RELEASES whatever is still pending rather than stranding it.
   *
   *  Each state is checked against the walk's own team, as it is when the run
   *  starts: an entry naming a state the team does not have, a pause on a
   *  state that is not a starter, or review on a consolidator or a state that
   *  runs no member, is refused with 400 `invalid_breakpoint` and the previous
   *  arming is kept — rather than armed and silently never hit.
   *
   *  A pause is read and answered through the run's interrupts.
   *
   *  `opts.reviewTtlSeconds` also replaces the walk's review deadline — a
   *  member hold nobody rules on within it ends rejected; `0` is no deadline.
   *  Omit it to leave the deadline as it is. It applies from each member's
   *  NEXT hold: a hold already in progress keeps the `expires_at` it
   *  announced. A negative or out-of-range value is refused with 400
   *  `invalid_review_ttl`, and so is the set sent with it. Like the
   *  breakpoints, it lives only as long as the walk; a member restored after
   *  a restart keeps the deadline it started with. */
  async setRunBreakpoints(
    runId: string,
    breakpoints: string[],
    opts?: { signal?: AbortSignal; reviewTtlSeconds?: number },
  ): Promise<TeamBreakpoints> {
    const body: Record<string, unknown> = { breakpoints };
    if (opts?.reviewTtlSeconds !== undefined) body.review_ttl_seconds = opts.reviewTtlSeconds;
    return putJSON<TeamBreakpoints>(
      this.ctx,
      `/v1/runs/${encodeURIComponent(runId)}/breakpoints`,
      body,
      { signal: opts?.signal },
    );
  }

  /** Invoke the RFC AL Path VFS tool over HTTP (`POST /v1/_path`). A
   *  Unix-like filesystem over your Memory entries, Volume mounts, and
   *  Documents — address them by human-readable paths (e.g. /docs/launch).
   *  Op-discriminated (`resolve`/`ls`/`stat`/`mkdir`/`mv`/`rm`). Scope
   *  (agent/user/tenant) + tenant are resolved server-side from the
   *  authenticated principal, never the wire.
   *
   *  Response varies per op — an `ls` returns `{path, entries}`, a `resolve`
   *  returns the dirent + resource_ref. Narrow as needed.
   *
   *  Raises {@link SubstrateToolRefusedError} on tool-level refusals (bad
   *  path, rm of a non-empty path without recursive, etc.);
   *  {@link InvalidArgumentError} on 400; {@link AuthError} on 401.
   *
   *  `opts.scopeId` / `opts.tenant` are the RFC AS browse-by-subject overrides
   *  (sent as `?scope_id=` / `?tenant=` query params — the server reads them
   *  from the URL, not the body, and re-checks authorization: a tenant
   *  principal's `tenant` is ignored, `scopeId` picks any subject it may see).
   *  Omit both to browse your own subject (byte-identical to the pre-RFC-AS
   *  request). */
  async path(
    input: PathToolInput,
    opts?: { signal?: AbortSignal; scopeId?: string; tenant?: string },
  ): Promise<PathToolResponse> {
    return postJSON<PathToolResponse>(this.ctx, "/v1/_path", input, {
      signal: opts?.signal,
      query: browseQuery(opts),
    });
  }

  /** Invoke the RFC AK Document tool over HTTP (`POST /v1/_document`). A
   *  chunked-graph document where each chunk is a first-class unit (UUID,
   *  hierarchy, type, fields, graph edges, Markdown body) that agents and
   *  humans co-author. Op-discriminated (16 ops: document/chunk lifecycle,
   *  set_path, edges, query_chunks, type defs, export_md/import_md). Scope
   *  agent/user (tenant deferred); resolved server-side from the principal.
   *
   *  Requires SQL Memory enabled on the sidecar (`LOOMCYCLE_SQLMEM_ENABLED=1`)
   *  — the chunk-structure tables live there. Without it the call is refused
   *  ({@link SubstrateToolRefusedError}).
   *
   *  Response varies per op — `create_document` returns
   *  `{document_id, root_chunk_id, ...}`, `query_chunks` returns rows.
   *  {@link InvalidArgumentError} on 400; {@link AuthError} on 401.
   *
   *  `opts.scopeId` / `opts.tenant` are the RFC AS browse-by-subject overrides —
   *  see {@link LoomcycleClient.path}. Omit both to browse your own subject. */
  async document(
    input: DocumentToolInput,
    opts?: { signal?: AbortSignal; scopeId?: string; tenant?: string },
  ): Promise<DocumentToolResponse> {
    return postJSON<DocumentToolResponse>(this.ctx, "/v1/_document", input, {
      signal: opts?.signal,
      query: browseQuery(opts),
    });
  }

  /** Build the URL of an image chunk's asset (RFC BO) — `GET
   *  /v1/_document/asset/{chunkId}`. Attach the bytes to a chunk with the
   *  `set_asset` op via {@link LoomcycleClient.document}, then read them here.
   *  `scope` is "agent" | "user" | "tenant" (default "user"); `scopeId`/`tenant` are the
   *  RFC AS browse overrides. NOTE: the URL needs the bearer to fetch — use
   *  {@link LoomcycleClient.fetchDocumentAsset} for an authenticated GET (a bare
   *  <img src> can't carry the Authorization header). */
  documentAssetUrl(
    chunkId: string,
    opts?: { scope?: string; scopeId?: string; tenant?: string },
  ): string {
    const q = new URLSearchParams({ scope: opts?.scope ?? "user" });
    if (opts?.scopeId) q.set("scope_id", opts.scopeId);
    if (opts?.tenant) q.set("tenant", opts.tenant);
    return `${this.ctx.baseUrl}/v1/_document/asset/${encodeURIComponent(chunkId)}?${q.toString()}`;
  }

  /** Fetch an image chunk's asset bytes (RFC BO) with authentication — returns
   *  the raw {@link Response} so a caller can `.blob()` / `.arrayBuffer()` it.
   *  Adds the bearer header (a bare <img src> can't). Throws on a non-2xx
   *  (a cross-scope / missing asset is an opaque 404). */
  async fetchDocumentAsset(
    chunkId: string,
    opts?: { scope?: string; scopeId?: string; tenant?: string; signal?: AbortSignal },
  ): Promise<Response> {
    const headers: Record<string, string> = {};
    if (this.ctx.authToken) headers.Authorization = `Bearer ${this.ctx.authToken}`;
    const resp = await this.ctx.fetchImpl(this.documentAssetUrl(chunkId, opts), {
      method: "GET",
      headers,
      signal: opts?.signal,
    });
    if (!resp.ok) {
      throw new Error(`fetchDocumentAsset: ${resp.status} ${resp.statusText}`);
    }
    return resp;
  }

  /** Invoke the RFC BE History tool over HTTP (`POST /v1/_history`). Browse,
   *  search, and annotate PAST CHATS — a chat is a session (it may span
   *  several runs). Op-discriminated (`list`/`get`/`search`/`rename`/
   *  `annotate`/`pin`/`archive`/`recap`/`resume`).
   *
   *  The owner is resolved server-side from the authenticated principal, NEVER
   *  the wire: `scope:"user"` keys on your own subject, `scope:"tenant"` on your
   *  tenant. The cross-tenant `scope:"global"` is admin-only — a tenant operator
   *  requesting it is refused ({@link SubstrateToolRefusedError}).
   *
   *  Response varies per op — `list`/`search` return `{scope, chats, total}`,
   *  `get` returns the chat metadata + transcript (or Markdown when
   *  `format:"markdown"`), `recap` returns a summary. Narrow as needed.
   *
   *  Raises {@link SubstrateToolRefusedError} on tool-level refusals (unknown
   *  session, disallowed scope); {@link InvalidArgumentError} on 400;
   *  {@link AuthError} on 401. */
  async history(
    input: HistoryToolInput,
    opts?: { signal?: AbortSignal },
  ): Promise<HistoryToolResponse> {
    return postJSON<HistoryToolResponse>(this.ctx, "/v1/_history", input, opts);
  }

  /** Invoke the Evaluation tool outside a run (`POST /v1/_evaluation`):
   *  record scores against runs and read them back. Op-discriminated
   *  (`submit` / `get` / `list_for_run` / `list_for_def` / `aggregate`); the
   *  return type follows the op. The `*Evaluation*` methods below are the same
   *  call with positional arguments.
   *
   *  What a token may do: `substrate:tenant` every op, inside its own tenant;
   *  admin every op, in every tenant; any other token (an isolated
   *  `substrate:user`, a tenant member) `submit_self` only, which a call from
   *  outside a run cannot use — it is not the run's agent — so each op is
   *  refused. A token without `runs:read` gets a 403.
   *
   *  Another tenant's evaluation or run reads exactly like one that does not
   *  exist. Raises {@link SubstrateToolRefusedError} on a tool refusal (an
   *  unknown id, an op the token may not use); {@link InvalidArgumentError}
   *  on 400; {@link AuthError} on 401. */
  async evaluation(
    input: EvaluationSubmitInput,
    opts?: { signal?: AbortSignal },
  ): Promise<EvaluationSubmitResult>;
  async evaluation(
    input: EvaluationGetInput,
    opts?: { signal?: AbortSignal },
  ): Promise<EvaluationRecord>;
  async evaluation(
    input: EvaluationListForRunInput,
    opts?: { signal?: AbortSignal },
  ): Promise<EvaluationListForRunResult>;
  async evaluation(
    input: EvaluationListForDefInput,
    opts?: { signal?: AbortSignal },
  ): Promise<EvaluationListForDefResult>;
  async evaluation(
    input: EvaluationAggregateInput,
    opts?: { signal?: AbortSignal },
  ): Promise<EvaluationAggregate>;
  async evaluation(
    input: EvaluationInput,
    opts?: { signal?: AbortSignal },
  ): Promise<
    | EvaluationSubmitResult
    | EvaluationRecord
    | EvaluationListForRunResult
    | EvaluationListForDefResult
    | EvaluationAggregate
  >;
  async evaluation(
    input: EvaluationInput,
    opts?: { signal?: AbortSignal },
  ): Promise<
    | EvaluationSubmitResult
    | EvaluationRecord
    | EvaluationListForRunResult
    | EvaluationListForDefResult
    | EvaluationAggregate
  > {
    return postJSON(this.ctx, "/v1/_evaluation", input, opts);
  }

  /** Record a score against a run — `evaluation({op:"submit"})`. Scores are
   *  additive: a second submit records another evaluation. */
  async submitEvaluation(
    runId: string,
    score: number,
    opts?: {
      dimensions?: Record<string, number>;
      judgement?: unknown;
      rationale?: string;
      signal?: AbortSignal;
    },
  ): Promise<EvaluationSubmitResult> {
    const input: EvaluationSubmitInput = { op: "submit", run_id: runId, score };
    if (opts?.dimensions !== undefined) input.dimensions = opts.dimensions;
    if (opts?.judgement !== undefined) input.judgement = opts.judgement;
    if (opts?.rationale !== undefined) input.rationale = opts.rationale;
    return this.evaluation(input, { signal: opts?.signal });
  }

  /** One evaluation by id — `evaluation({op:"get"})`. */
  async getEvaluation(
    evalId: string,
    opts?: { signal?: AbortSignal },
  ): Promise<EvaluationRecord> {
    return this.evaluation({ op: "get", eval_id: evalId }, opts);
  }

  /** Every evaluation of one run — `evaluation({op:"list_for_run"})`. */
  async listEvaluationsForRun(
    runId: string,
    opts?: { limit?: number; signal?: AbortSignal },
  ): Promise<EvaluationListForRunResult> {
    const input: EvaluationListForRunInput = { op: "list_for_run", run_id: runId };
    if (opts?.limit !== undefined) input.limit = opts.limit;
    return this.evaluation(input, { signal: opts?.signal });
  }

  /** Every evaluation of one definition version, in your tenant —
   *  `evaluation({op:"list_for_def"})`. */
  async listEvaluationsForDef(
    defId: string,
    opts?: { limit?: number; signal?: AbortSignal },
  ): Promise<EvaluationListForDefResult> {
    const input: EvaluationListForDefInput = { op: "list_for_def", def_id: defId };
    if (opts?.limit !== undefined) input.limit = opts.limit;
    return this.evaluation(input, { signal: opts?.signal });
  }

  /** A definition's scores rolled up — `evaluation({op:"aggregate"})`.
   *  `includeLineage` adds the ancestors' evaluations along parent_def_id. */
  async aggregateEvaluations(
    defId: string,
    opts?: { includeLineage?: boolean; signal?: AbortSignal },
  ): Promise<EvaluationAggregate> {
    const input: EvaluationAggregateInput = { op: "aggregate", def_id: defId };
    if (opts?.includeLineage !== undefined) input.include_lineage = opts.includeLineage;
    return this.evaluation(input, { signal: opts?.signal });
  }

  /** List the PERSISTENT volume universe for the caller's tenant — every
   *  static volume (the shared bind floor, `source: "static"`, read-only)
   *  plus the tenant's own dynamic VolumeDefs (`source: "dynamic"`). Host
   *  paths are redacted (`""`) for a non-operator (tenant) caller.
   *
   *  Wraps `GET /v1/_volumes` (RFC AH Phase 4). Bearer-authed; tenant-scoped
   *  from the authoritative principal. */
  async listVolumes(opts?: {
    signal?: AbortSignal;
  }): Promise<PersistentVolumesResponse> {
    return jsonFetch<PersistentVolumesResponse>(this.ctx, "/v1/_volumes", opts);
  }

  /** List the LIVE ephemeral (run-scoped) volumes for the caller's tenant —
   *  created mid-run, inherited by sub-agents, auto-purged when the run
   *  completes. Host paths are redacted (`""`) for a non-operator caller.
   *
   *  Wraps `GET /v1/_volumes/ephemeral` (RFC AH Phase 4). Bearer-authed;
   *  tenant-scoped from the authoritative principal. */
  async listEphemeralVolumes(opts?: {
    signal?: AbortSignal;
  }): Promise<EphemeralVolumesResponse> {
    return jsonFetch<EphemeralVolumesResponse>(this.ctx, "/v1/_volumes/ephemeral", opts);
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
  /** GET-based SSE stream (RFC AI re-attach). The interactive re-attach
   *  endpoint is a GET with a query string, not a POST body, so it can't reuse
   *  streamSSE; the parse + error handling are identical otherwise. No debug
   *  shape — re-attach is a plain tail. */
  private async *streamSSEGet(
    path: string,
    signal?: AbortSignal,
  ): AsyncIterable<AgentEvent> {
    const headers: Record<string, string> = {
      Accept: "text/event-stream, application/json",
    };
    if (this.ctx.authToken) headers.Authorization = `Bearer ${this.ctx.authToken}`;
    const resp = await this.ctx.fetchImpl(this.ctx.baseUrl + path, {
      method: "GET",
      headers,
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

  /** List the channels a team declares for itself, with each one's
   *  definition and traffic counts (GET /v1/_teamdef/{team}/channels).
   *
   *  A team's own channel is written and read only from inside the team, so
   *  {@link LoomcycleClient.listChannels} never shows it. This is how the
   *  person who runs the team sees it. The team is resolved in the caller's
   *  tenant: an unknown team, or one in another tenant, raises a
   *  {@link NotFoundError} (code `team_not_found`). Read-only. */
  async listTeamChannels(
    team: string,
    opts: ListTeamChannelsOptions = {},
  ): Promise<TeamChannelsResult> {
    let path = `/v1/_teamdef/${encodeURIComponent(team)}/channels`;
    if (opts.tenant) path += `?tenant=${encodeURIComponent(opts.tenant)}`;
    return jsonFetch<TeamChannelsResult>(this.ctx, path, { signal: opts.signal });
  }

  /** Read messages on one of a team's own channels without advancing any
   *  cursor (GET /v1/_teamdef/{team}/channels/{name}/peek). `name` is the
   *  channel's name inside the team (`journal` for `./journal`).
   *
   *  Nothing is consumed: the team still reads every message, and the same
   *  ones come back on the next call. Held messages are not listed;
   *  {@link LoomcycleClient.listTeamChannels} counts them. A name the team
   *  does not declare raises a {@link NotFoundError} (code
   *  `team_channel_not_declared`). {@link LoomcycleClient.peekChannel} keeps
   *  refusing both the stored name and `./name`. */
  async peekTeamChannel(
    team: string,
    name: string,
    opts: PeekTeamChannelOptions = {},
  ): Promise<TeamChannelPeekResult> {
    let path = `/v1/_teamdef/${encodeURIComponent(team)}/channels/${encodeURIComponent(name)}/peek`;
    const params: string[] = [];
    if (opts.userId) params.push(`user_id=${encodeURIComponent(opts.userId)}`);
    if (opts.fromCursor) params.push(`from_cursor=${encodeURIComponent(opts.fromCursor)}`);
    if (opts.maxMessages) params.push(`max_messages=${opts.maxMessages}`);
    if (opts.tenant) params.push(`tenant=${encodeURIComponent(opts.tenant)}`);
    if (params.length > 0) path += `?${params.join("&")}`;
    return jsonFetch<TeamChannelPeekResult>(this.ctx, path, { signal: opts.signal });
  }

  /** Deliver held messages on one of a team's own channels to the team's
   *  readers (POST /v1/_teamdef/{team}/channels/{name}/release).
   *
   *  A channel a team declares with `hold: true` stores every publish and
   *  delivers none until released. This is the operator's side of that
   *  breakpoint: it steps a team whose own states, schedule or webhook write
   *  to a held channel. It delivers only what the team itself wrote; there is
   *  no publish, ack or purge for a team's own channel from outside the team.
   *  Releasing when nothing is held resolves with `released_count: 0`. */
  async releaseTeamChannel(
    team: string,
    name: string,
    opts: ReleaseTeamChannelOptions = {},
  ): Promise<TeamChannelReleaseResult> {
    let path = `/v1/_teamdef/${encodeURIComponent(team)}/channels/${encodeURIComponent(name)}/release`;
    if (opts.tenant) path += `?tenant=${encodeURIComponent(opts.tenant)}`;
    const body: Record<string, unknown> = {};
    if (opts.count !== undefined) body.count = opts.count;
    if (opts.userId !== undefined) body.user_id = opts.userId;
    return postJSON<TeamChannelReleaseResult>(this.ctx, path, body, { signal: opts.signal });
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

  // ---- RFC S client twins: fan-in (await) / fan-out (broadcast) ----
  //
  // Multi-channel meta-ops (POST /v1/_channels/_await and /_broadcast).
  // Unlike the per-channel ops above they aren't addressed by name — the
  // whole request is the body — so they don't use channelOpPath. `scope`
  // + `userId` apply to every channel in the set. The complements to the
  // in-band Channel.await / Channel.broadcast tool ops, letting a wire
  // caller join independent producers / ping N workers over the same bus.

  /** Fan IN across `channels`: wait until the `mode` predicate is met
   *  (`any` / `all` / `at_least` N), or `waitMs` elapses. NON-committing
   *  (detection only) — `subscribe`/`ack` exactly what you process. A
   *  timeout is not an error: the result carries `timed_out: true`. */
  async awaitChannels(opts: AwaitChannelsOptions): Promise<ChannelAwaitResult> {
    const body: Record<string, unknown> = { channels: opts.channels };
    if (opts.scope) body.scope = opts.scope;
    if (opts.userId) body.scope_id = opts.userId;
    if (opts.mode) body.mode = opts.mode;
    if (opts.n !== undefined) body.n = opts.n;
    if (opts.fromCursor !== undefined) body.from_cursor = opts.fromCursor;
    if (opts.maxMessages !== undefined) body.max_messages = opts.maxMessages;
    if (opts.waitMs !== undefined) body.wait_ms = opts.waitMs;
    return postJSON<ChannelAwaitResult>(this.ctx, "/v1/_channels/_await", body, {
      signal: opts.signal,
    });
  }

  /** Fan OUT: publish the same `payload` to every channel in `channels`,
   *  in one call. Atomic at the declare pre-flight — one undeclared
   *  channel rejects the whole call ({@link NotFoundError}, code
   *  `channel_not_declared`) with nothing published. A per-channel write
   *  fault after that is reported in that channel's `results` entry while
   *  the successful publishes stand. */
  async broadcastChannels(
    opts: BroadcastChannelsOptions,
  ): Promise<ChannelBroadcastResult> {
    const body: Record<string, unknown> = {
      channels: opts.channels,
      payload: opts.payload,
    };
    if (opts.scope) body.scope = opts.scope;
    if (opts.userId) body.scope_id = opts.userId;
    if (opts.deliverAt) body.deliver_at = opts.deliverAt;
    return postJSON<ChannelBroadcastResult>(
      this.ctx,
      "/v1/_channels/_broadcast",
      body,
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
    if (opts.hold !== undefined) body.hold = opts.hold;
    if (opts.hooks !== undefined) body.hooks = opts.hooks;
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
    if (opts.hold !== undefined) body.hold = opts.hold;
    if (opts.hooks !== undefined) body.hooks = opts.hooks;
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

  /** Clear all buffered messages on a channel WITHOUT removing its
   *  definition or subscriber cursors. Unlike {@link deleteChannel} this
   *  is allowed on yaml-declared channels too — draining a yaml channel
   *  that filled with test traffic was the F20 pain it solves. Returns
   *  the channel name + the number of messages cleared. Unknown channels
   *  reject with a {@link NotFoundError}. */
  async purgeChannel(
    name: string,
    opts?: { signal?: AbortSignal },
  ): Promise<ChannelPurgeResult> {
    return postJSON<ChannelPurgeResult>(
      this.ctx,
      `/v1/_channels/${encodeURIComponent(name)}/purge`,
      {},
      { signal: opts?.signal },
    );
  }

  /** Hand the oldest `count` (default 1) messages held on a `hold:`
   *  channel to its subscribers. Allowed on yaml-declared channels —
   *  releasing moves messages, it does not mutate the definition.
   *  Releasing a channel with nothing held reports zero rather than
   *  failing. */
  async releaseChannel(
    name: string,
    opts?: ReleaseChannelOptions,
  ): Promise<ChannelReleaseResult> {
    const body: Record<string, unknown> = {};
    if (opts?.count !== undefined) body.count = opts.count;
    if (opts?.scope !== undefined) body.scope = opts.scope;
    if (opts?.scope_id !== undefined) body.scope_id = opts.scope_id;
    return postJSON<ChannelReleaseResult>(
      this.ctx,
      `/v1/_channels/${encodeURIComponent(name)}/release`,
      body,
      { signal: opts?.signal },
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
    const qs = LoomcycleClient.memoryFocusQuery(opts.tenant).toString();
    return putJSON<SetMemoryEntryResponse>(
      this.ctx,
      `/v1/_memory/scopes/${encodeURIComponent(scope)}/${encodeURIComponent(scopeID)}/keys/${encodeURIComponent(key)}${qs ? "?" + qs : ""}`,
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
    opts?: { tenant?: string; signal?: AbortSignal },
  ): Promise<void> {
    const qs = LoomcycleClient.memoryFocusQuery(opts?.tenant).toString();
    return deleteRequest(
      this.ctx,
      `/v1/_memory/scopes/${encodeURIComponent(scope)}/${encodeURIComponent(scopeID)}/keys/${encodeURIComponent(key)}${qs ? "?" + qs : ""}`,
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
   *  v1.78.0 — `walkId` is a SERVER-side filter: only the runs one team
   *  walk spawned. A walk's own run_id IS its walk id, so a caller that
   *  started a team with `mode: "detach"` passes back the handle it already
   *  holds and gets a live view of that workflow's agents:
   *
   *  ```ts
   *  const { run_id } = await client.runTeam({ name: "triage", mode: "detach" });
   *  for await (const item of client.streamUserRunStates(userId, { walkId: run_id })) {
   *    if (item.kind === "event") place(item.payload.parent_context?.wave_index);
   *  }
   *  ```
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
    if (opts?.walkId) {
      params.set("walk_id", opts.walkId);
    }
    if (opts?.parentRunId) {
      params.set("parent_run_id", opts.parentRunId);
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

  /** Tail the caller's tenant's MEMORY change feed (RFC CD Part C) — one
   *  value-free coordinate per `memory.set` / `memory.delete` /
   *  `memory.scope_deleted`. Mirrors `GET /v1/_memory/changes?since=<seq>`.
   *
   *  The first item is always `{ kind: "feed", payload: ChangeFeedStatus }`,
   *  then one `{ kind: "change", payload: MemoryChange }` per change, oldest
   *  first. Pull the current value through the data API; the feed never
   *  carries it.
   *
   *  - The feed is EMPTY unless the server runs with
   *    `LOOMCYCLE_MEMORY_CHANGES_ENABLED`. The stream still opens and idles,
   *    so read `payload.enabled` on the feed frame instead of waiting.
   *  - Scope is the caller's OWN tenant; there is no cross-tenant feed.
   *  - Needs `substrate:tenant` (or admin). A tenant MEMBER's token is refused
   *    — the feed exposes the tenant's write pattern, which is operator
   *    observability, not member data.
   *  - To resume after a disconnect, persist the last `payload.seq` you
   *    processed and pass it back as `since`. A cursor older than the
   *    server's change retention resumes past the pruned gap without saying
   *    so.
   *  - A `{ kind: "error", payload: ChangeFeedError }` item means the server
   *    could not read its change log. It is NOT fatal and iteration
   *    continues: the server retries with backoff without advancing its
   *    cursor, so changes resume after recovery with no gap. One item per
   *    failure streak.
   *  - The stream has no end of its own: iteration ends when the server
   *    closes it (shutdown — reconnect with `since`) or throws an
   *    `AbortError` when `signal` aborts. */
  async *streamMemoryChanges(
    opts?: StreamChangesOptions,
  ): AsyncIterable<ChangeFeedFrame> {
    yield* this.streamChangeFeed("/v1/_memory/changes", opts);
  }

  /** Tail the caller's tenant's DOCUMENT change feed (RFC CD Part C) — one
   *  coordinate per `document.chunk.updated` / `document.chunk.deleted`,
   *  naming the `chunk_id`. Mirrors `GET /v1/_document/changes?since=<seq>`.
   *  Same frames, scope, auth and resume contract as
   *  {@link LoomcycleClient.streamMemoryChanges}; a reader wanting both
   *  families opens both. */
  async *streamDocumentChanges(
    opts?: StreamChangesOptions,
  ): AsyncIterable<ChangeFeedFrame> {
    yield* this.streamChangeFeed("/v1/_document/changes", opts);
  }

  private async *streamChangeFeed(
    path: string,
    opts?: StreamChangesOptions,
  ): AsyncIterable<ChangeFeedFrame> {
    const since = opts?.since;
    // The server reads an unparseable or negative `since` as 0 rather than
    // refusing it, which replays every retained change — a resume bug that
    // would look like a burst of activity. Refuse it here instead.
    if (since !== undefined && !(Number.isSafeInteger(since) && since >= 0)) {
      throw new Error(
        `loomcycle: change feed "since" must be a non-negative integer, got ${String(since)}`,
      );
    }
    const q = since ? `?since=${since}` : "";
    for await (const ev of this.streamSSEGet(path + q, opts?.signal)) {
      const frame = toChangeFeedFrame(ev as unknown as Record<string, unknown>);
      if (frame) yield frame;
    }
  }

  /** List credential METADATA for a scope (RFC AR) — never a secret value.
   *  `user` = your own subject's tokens (per-user, e.g. a personal Telegram bot
   *  token); `tenant` = shared across the tenant (operator authority). RFC CN: a
   *  user token may only address `user`; `tenant` requires substrate:tenant.
   *  Mirrors `POST /v1/_credentialdef {op:list}`. */
  async listCredentials(
    scope: CredentialScope,
    opts?: { signal?: AbortSignal },
  ): Promise<CredentialListResponse> {
    return postJSON<CredentialListResponse>(
      this.ctx,
      "/v1/_credentialdef",
      { op: "list", scope },
      opts,
    );
  }

  /** Store or rotate a credential (RFC AR). The plaintext `value` is write-only:
   *  the server encrypts it, masks it from the transcript, and never returns it.
   *  Returns the stored row's metadata. Mirrors `POST /v1/_credentialdef {op:create}`.
   *
   *  `allowed_urls` restricts where the value may be sent: https URLs, each
   *  covering itself and everything under it. `[]` stores a value that is
   *  sent nowhere. Leave it out for no restriction, or, when the credential
   *  already exists, to keep the list it has. A restricted credential is used
   *  only where the destination is known and covered (an MCP server's or a
   *  hook's header, a remote peer's key, a git source), never as a provider
   *  key.
   *
   *  `username` is the user name sent with the value where a destination
   *  takes one (HTTP basic auth for a git source). Leave it out to keep the
   *  stored one; `""` removes it.
   *
   *  `expires_at` is an RFC3339 timestamp (`"2026-12-31T00:00:00Z"`) stored
   *  with the credential and reported back in its metadata; any other format
   *  is refused. `backend` is where the value is kept: `"inline"` (encrypted
   *  in loomcycle's own store, the default) is the only one accepted. */
  async createCredential(
    input: {
      scope: CredentialScope;
      name: string;
      value: string;
      allowed_urls?: string[];
      username?: string;
      expires_at?: string;
      backend?: "inline";
    },
    opts?: { signal?: AbortSignal },
  ): Promise<CredentialMeta> {
    return postJSON<CredentialMeta>(
      this.ctx,
      "/v1/_credentialdef",
      { op: "create", ...input },
      opts,
    );
  }

  /** Read ONE credential's metadata (RFC AR) — never its value. Answers
   *  whether a name is stored, and its `allowed_urls` / `username` /
   *  `expires_at`, without listing the whole scope. An absent name raises
   *  {@link SubstrateToolRefusedError}. Mirrors
   *  `POST /v1/_credentialdef {op:get}`. */
  async getCredential(
    input: { scope: CredentialScope; name: string },
    opts?: { signal?: AbortSignal },
  ): Promise<CredentialMeta> {
    return postJSON<CredentialMeta>(
      this.ctx,
      "/v1/_credentialdef",
      { op: "get", ...input },
      opts,
    );
  }

  /** Delete a credential (RFC AR). Anything referencing `$cred:<name>` at that
   *  scope stops resolving. Mirrors `POST /v1/_credentialdef {op:delete}`. */
  async deleteCredential(
    input: { scope: CredentialScope; name: string },
    opts?: { signal?: AbortSignal },
  ): Promise<{ name: string; scope: string; deleted: boolean }> {
    return postJSON(
      this.ctx,
      "/v1/_credentialdef",
      { op: "delete", ...input },
      opts,
    );
  }

  // ── Crypto keys (RFC EP) ─────────────────────────────────────────────────
  //
  // A crypto key is a credential with usage "crypto": a key type fixed at
  // creation, numbered versions, and a min_version below which versions are
  // retired. Its material is never returned by any of these calls; the Crypto
  // tool (`crypto()` here, or an agent's Crypto tool) uses it by name. Each
  // returns the key's metadata (`CredentialMeta` with `usage`, `key_type`,
  // `versions`, `current_version`, `min_version`). Delete a key with
  // `deleteCredential` once every version is retired (min_version raised to
  // current+1). All post to `POST /v1/_credentialdef`.

  /** Make a new crypto key nobody ever sees: the server generates the
   *  material. Version 1 is current. Mirrors `{op:generate}`. A name another
   *  reachable scope already uses for a crypto key is refused
   *  ({@link SubstrateToolRefusedError}), since a bare name would then be
   *  ambiguous. */
  async generateCryptoKey(
    input: { scope: CredentialScope; name: string; key_type: CryptoKeyType; expires_at?: string },
    opts?: { signal?: AbortSignal },
  ): Promise<CredentialMeta> {
    return postJSON<CredentialMeta>(
      this.ctx,
      "/v1/_credentialdef",
      { op: "generate", ...input },
      opts,
    );
  }

  /** Store key material you already have as a crypto key (version 1). `value`
   *  is write-only: masked from the transcript, never returned. Use this to
   *  bring in an existing key; to move a key from another loomcycle
   *  deployment use {@link importCryptoKey}. Mirrors
   *  `{op:create, usage:"crypto"}`. */
  async createCryptoKey(
    input: {
      scope: CredentialScope;
      name: string;
      key_type: CryptoKeyType;
      value: string;
      key_encoding?: CryptoKeyEncoding;
      expires_at?: string;
    },
    opts?: { signal?: AbortSignal },
  ): Promise<CredentialMeta> {
    return postJSON<CredentialMeta>(
      this.ctx,
      "/v1/_credentialdef",
      { op: "create", usage: "crypto", ...input },
      opts,
    );
  }

  /** Reference a key held in an external KMS (Vault Transit, AWS KMS or
   *  Google Cloud KMS) as a crypto key: loomcycle stores only where the key
   *  is, and every Crypto op on it is a call to the KMS. The server asks the
   *  KMS that the key exists, is enabled and is of `key_type` before storing
   *  it. Rotate and retire such a key in the KMS; deleting it removes only
   *  the reference. Mirrors `{op:create, usage:"crypto", backend, external}`. */
  async createKmsCryptoKey(
    input: {
      scope: CredentialScope;
      name: string;
      key_type: CryptoKeyType;
      external: CryptoKeyExternal;
      expires_at?: string;
    },
    opts?: { signal?: AbortSignal },
  ): Promise<CredentialMeta> {
    const { backend, ...external } = input.external;
    return postJSON<CredentialMeta>(
      this.ctx,
      "/v1/_credentialdef",
      { op: "create", usage: "crypto", ...input, backend, external },
      opts,
    );
  }

  /** Add a new version and make it current. Without `value` the server
   *  generates it; with one, it is the new version's material (write-only).
   *  Older versions keep decrypting and verifying until `min_version` passes
   *  them; `rewrap` moves ciphertexts to the new version. Mirrors
   *  `{op:rotate}`. */
  async rotateCryptoKey(
    input: { scope: CredentialScope; name: string; value?: string; key_encoding?: CryptoKeyEncoding },
    opts?: { signal?: AbortSignal },
  ): Promise<CredentialMeta> {
    return postJSON<CredentialMeta>(
      this.ctx,
      "/v1/_credentialdef",
      { op: "rotate", ...input },
      opts,
    );
  }

  /** Retire every version below `min_version`: nothing decrypts or verifies
   *  with them any more, and only they can be deleted. It only goes up, and
   *  at most to current+1 (which retires every version — the step before
   *  deleting the key). A lower value, or one past current+1, is refused
   *  ({@link SubstrateToolRefusedError}). Mirrors `{op:set_min_version}`. */
  async setCryptoKeyMinVersion(
    input: { scope: CredentialScope; name: string; min_version: number },
    opts?: { signal?: AbortSignal },
  ): Promise<CredentialMeta> {
    return postJSON<CredentialMeta>(
      this.ctx,
      "/v1/_credentialdef",
      { op: "set_min_version", ...input },
      opts,
    );
  }

  /** Delete one retired version (below `min_version`) and its material. A
   *  version still in use is refused ({@link SubstrateToolRefusedError}).
   *  Mirrors `{op:delete_version}`. */
  async deleteCryptoKeyVersion(
    input: { scope: CredentialScope; name: string; version: number },
    opts?: { signal?: AbortSignal },
  ): Promise<CredentialMeta> {
    return postJSON<CredentialMeta>(
      this.ctx,
      "/v1/_credentialdef",
      { op: "delete_version", ...input },
      opts,
    );
  }

  /** Export a crypto key, every version of it, encrypted under `passphrase`
   *  (at least 16 characters), to move it to another deployment with
   *  {@link importCryptoKey}. The blob plus the passphrase IS the key: keep
   *  them apart. Admin only (the legacy token, `substrate:admin`, or open
   *  mode) and only outside a run; anyone else is refused
   *  ({@link SubstrateToolRefusedError}). Mirrors `{op:export_key}`. */
  async exportCryptoKey(
    input: { scope: CredentialScope; name: string; passphrase: string },
    opts?: { signal?: AbortSignal },
  ): Promise<CryptoKeyExport> {
    return postJSON<CryptoKeyExport>(
      this.ctx,
      "/v1/_credentialdef",
      { op: "export_key", ...input },
      opts,
    );
  }

  /** Import what {@link exportCryptoKey} returned on another deployment, with
   *  the passphrase it was exported with, into `scope`. `name` must be the
   *  name it was exported under (the blob is bound to it); the server
   *  refuses another. Versions and `min_version` come with it. Admin only
   *  and outside a run, like export. Mirrors `{op:import_key}`. */
  async importCryptoKey(
    input: { scope: CredentialScope; name: string; blob: string; passphrase: string },
    opts?: { signal?: AbortSignal },
  ): Promise<CredentialMeta> {
    return postJSON<CredentialMeta>(
      this.ctx,
      "/v1/_credentialdef",
      { op: "import_key", ...input },
      opts,
    );
  }

  /** Run one Crypto-tool operation outside any run (`POST /v1/_crypto`, RFC
   *  EP): encrypt / decrypt / rewrap / mac / verify_mac / sign / verify /
   *  public_key / totp with a key the runtime holds, or hash / random with
   *  none. The return type follows `op`. Key material never leaves the
   *  server.
   *
   *  Keys reachable: the token's tenant's and its own subject's user keys
   *  (agent keys belong to their agent's runs, and there are no `run`
   *  credentials outside a run). An admin may pass `tenant` to use another
   *  tenant's keys (and then no user keys). A ciphertext sealed here
   *  decrypts in that tenant's runs, and the reverse.
   *
   *  Needs `substrate:tenant`; a tenant member's or run token gets a 403. A
   *  refusal (a wrong key name, a key type that does not do the op, a failed
   *  decrypt — which never says why) raises {@link SubstrateToolRefusedError};
   *  each call takes one from the token's run-start rate, and a spent rate
   *  raises {@link RateLimitedError} with the server's `retryAfterMs`. */
  async crypto<R extends CryptoRequest>(
    input: R,
    opts?: { signal?: AbortSignal },
  ): Promise<CryptoResultByOp[R["op"]]> {
    return postJSON<CryptoResultByOp[R["op"]]>(this.ctx, "/v1/_crypto", input, opts);
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

/** The wire input of a typed create / fork. `promote` goes out only when the
 *  caller set it — false included — so the server's per-def default applies
 *  otherwise. */
function defWriteInput(
  op: "create" | "fork",
  name: string,
  overlay: Record<string, unknown>,
  opts?: ForkDefOptions,
): SubstrateToolInput {
  const input: SubstrateToolInput = { op, name, overlay };
  if (opts?.parentDefId) input.parent_def_id = opts.parentDefId;
  if (opts?.description) input.description = opts.description;
  if (opts?.promote !== undefined) input.promote = opts.promote;
  return input;
}

function signalOpts(opts?: { signal?: AbortSignal }): { signal?: AbortSignal } | undefined {
  return opts?.signal ? { signal: opts.signal } : undefined;
}

/** The form a HookDef definition is stored in: hooks.Def.Normalize followed
 *  by Go's omitempty encoding. Applied to both sides of ensureHookDef's
 *  comparison; the stored side is already in this form, so what matters is
 *  that the overlay lands exactly where the server would put it. */
function normalizeHookDef(d: HookDefOverlay): Record<string, unknown> {
  const out: Record<string, unknown> = { event: d.event };
  const description = goTrimSpace(d.description ?? "");
  if (description) out.description = description;
  if (d.match?.tools && d.match.tools.length > 0) out.match = { tools: d.match.tools };
  const body: Record<string, unknown> = { kind: d.body.kind };
  if (d.body.kind === "code-js") {
    if (d.body.code) body.code = d.body.code;
  } else {
    if (d.body.url) body.url = d.body.url;
    if (d.body.headers && Object.keys(d.body.headers).length > 0) body.headers = d.body.headers;
    if (d.body.signing_secret) body.signing_secret = d.body.signing_secret;
  }
  out.body = body;
  out.fail_mode = d.fail_mode || "open";
  if (d.timeout_ms) out.timeout_ms = d.timeout_ms;
  return out;
}

/** strings.TrimSpace: Go's Unicode White_Space set, which is not
 *  String.prototype.trim's (Go trims U+0085 and keeps U+FEFF; JavaScript does
 *  the reverse). Trimming differently could make two descriptions the server
 *  stores differently compare equal, and skip a real change. */
const goTrimRe =
  /^[\t\n\v\f\r \u0085\u00A0\u1680\u2000-\u200A\u2028\u2029\u202F\u205F\u3000]+|[\t\n\v\f\r \u0085\u00A0\u1680\u2000-\u200A\u2028\u2029\u202F\u205F\u3000]+$/g;
function goTrimSpace(s: string): string {
  return s.replace(goTrimRe, "");
}

/** Structural JSON equality, ignoring object key order (the server's
 *  encoding orders keys by struct field, a caller's literal by whatever it
 *  wrote). Arrays compare in order. */
function sameJSON(a: unknown, b: unknown): boolean {
  if (a === b) return true;
  if (typeof a !== "object" || typeof b !== "object" || a === null || b === null) return false;
  if (Array.isArray(a) !== Array.isArray(b)) return false;
  if (Array.isArray(a) && Array.isArray(b)) {
    return a.length === b.length && a.every((v, i) => sameJSON(v, b[i]));
  }
  const ao = a as Record<string, unknown>;
  const bo = b as Record<string, unknown>;
  const keys = Object.keys(ao);
  if (keys.length !== Object.keys(bo).length) return false;
  return keys.every((k) => Object.prototype.hasOwnProperty.call(bo, k) && sameJSON(ao[k], bo[k]));
}

/** toChangeFeedFrame types one frame of a change feed, or returns null for a
 *  frame it does not recognise.
 *
 *  The frames arrive through parseSSE, which backfills `type` from the SSE
 *  event name ONLY when the payload has none. The `feed` and `error` frames
 *  deliberately carry no `type`, so they surface as `type: "feed"` /
 *  `type: "error"`; a `change` frame
 *  carries its own change type (`memory.set`, …), so it is recognised by its
 *  numeric `seq` instead. Anything else — an event name a newer server adds —
 *  is skipped, the adapter's usual ignore-what-you-don't-know posture. */
function toChangeFeedFrame(
  raw: Record<string, unknown>,
): ChangeFeedFrame | null {
  if (raw.type === "feed") {
    // An unreadable status must not default to enabled: the frame exists so a
    // disabled feed is not mistaken for a quiet one.
    if (typeof raw.enabled !== "boolean") return null;
    const { type: _type, ...status } = raw;
    return { kind: "feed", payload: status as unknown as ChangeFeedStatus };
  }
  if (raw.type === "error") {
    // Like `feed`, the error frame carries no `type` of its own, so this one
    // was backfilled from the event name. Both fields are required: a
    // half-read error is skipped rather than surfaced as a blank one.
    if (typeof raw.code !== "string" || typeof raw.error !== "string") return null;
    const { type: _type, ...failure } = raw;
    return { kind: "error", payload: failure as unknown as ChangeFeedError };
  }
  if (typeof raw.seq === "number" && typeof raw.type === "string") {
    return { kind: "change", payload: raw as unknown as MemoryChange };
  }
  return null;
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

// browseQuery maps the RFC AS browse-by-subject opts to the wire query-param
// names the off-run Path/Document endpoints read from the URL (scope_id /
// tenant — matching web/src/api.ts:substratePost). Only set values are
// included; postJSON drops the query entirely when the map is empty, so a
// caller passing neither builds the same URL as before RFC AS.
function browseQuery(opts?: {
  scopeId?: string;
  tenant?: string;
}): Record<string, string> {
  const q: Record<string, string> = {};
  if (opts?.scopeId) q.scope_id = opts.scopeId;
  if (opts?.tenant) q.tenant = opts.tenant;
  return q;
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
