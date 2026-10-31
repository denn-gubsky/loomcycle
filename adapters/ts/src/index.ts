/**
 * @loomcycle/client — TypeScript client for the loomcycle sidecar.
 *
 * Public surface (v0.8.18 — Python-adapter parity):
 *
 *   class LoomcycleClient
 *     constructor(opts: ClientOptions)
 *
 *     // Run lifecycle (SSE streams)
 *     runStreaming(opts: RunOptions): AsyncIterable<AgentEvent>
 *     continueSession(opts: ContinueOptions): AsyncIterable<AgentEvent>
 *
 *     // Agent metadata
 *     getAgent(agentId): Promise<Agent>
 *     getRun(runId): Promise<Agent>
 *     cancelAgent(agentId, opts?): Promise<CancelAgentResult>
 *     listUserAgents(userId, opts?): Promise<AgentList>
 *     listWalkRuns(walkId, opts?): Promise<WalkRunsPage>
 *     deleteRunTree(runId, opts?): Promise<RunTreeDeletion>     // needs runs:delete
 *     deleteSession(sessionId, {cascade: "children"}): Promise<RunTreeDeletion>
 *     getTranscript(sessionId): Promise<TranscriptResponse>
 *     health(): Promise<HealthResponse>
 *     listUsers(opts?): Promise<ListUsersResponse>   // tenant-scoped (RFC L)
 *     createUser(body, opts?): Promise<UserRecord>    // RFC BX P2 (v1.50.0)
 *     updateUser(subject, body, opts?): Promise<UserRecord>
 *     deleteUser(subject, opts?): Promise<void>
 *     mintUserToken(subject, opts?): Promise<MintedUserToken>   // delegated token
 *     listUserTokens(subject, opts?): Promise<ListUserTokensResponse>
 *     revokeUserToken(subject, defId, opts?): Promise<{def_id, retired_at}>
 *     approveUser(subject, opts?): Promise<UserRecord>           // pending → active (RFC FA)
 *     listUserIdentities(subject, opts?): Promise<UserIdentitiesResponse>
 *     unlinkUserIdentity(subject, gate, opts?): Promise<void>
 *
 *     // Sign-in gates + the password gate's management API (RFC FA)
 *     listAuthGates(opts?): Promise<AuthGateListResponse>        // admin token only
 *     createInvite(gate, body, opts?): Promise<CreatedInvite>    // one-time link, shown once
 *     listInvites(gate, opts?): Promise<AuthInviteListResponse>
 *     revokeInvite(gate, inviteId, opts?): Promise<void>
 *     createPasswordReset(gate, subject, opts?): Promise<CreatedPasswordReset>
 *     removeGateCredentials(gate, subject, opts?): Promise<void>
 *     renameGateLogin(gate, subject, login, opts?): Promise<{subject, login}>
 *     listGateIdentities(gate, subject, opts?): Promise<GateIdentitiesResponse>
 *     getGateTenantStatus(gate, opts?): Promise<GateTenantStatus>
 *     runnableAgents(opts?): Promise<RunnableAgentsResponse>    // RFC BY (v1.51.0) — agents I can run
 *     whoami(): Promise<WhoamiResponse>               // RFC L principal (v0.17.0)
 *
 *     // Pause / Resume / State (v0.8.17/8.18)
 *     pauseRuntime(opts?): Promise<PauseResult>
 *     resumeRuntime(): Promise<ResumeResult>
 *     getRuntimeState(): Promise<RuntimeStateResponse>
 *     resolveProbe(opts?): Promise<ResolverMatrix>
 *     listModels(): Promise<ModelAliasesResponse>                 // configured model aliases
 *     getRouting(): Promise<RoutingResponse>                      // per user_tier × tier cascade
 *     listProviderModels(providerId): Promise<ProviderModelsResponse>   // admin token only
 *
 *     // Snapshot lifecycle (v0.8.17/8.18)
 *     createSnapshot(opts?): Promise<SnapshotCreateResponse>
 *     listSnapshots(opts?): Promise<SnapshotDescriptor[]>
 *     getSnapshot(id): Promise<SnapshotEnvelope>
 *     exportSnapshotURL(id): string  (synchronous; returns a URL)
 *     restoreSnapshot(opts): Promise<SnapshotRestoreResponse>
 *     deleteSnapshot(id): Promise<void>
 *
 *     // Memory admin
 *     listMemoryScopes(): Promise<MemoryScopesResponse>
 *     listMemoryScopeIDs(scope): Promise<MemoryScopeIDsResponse>
 *     listMemoryEntries(scope, scopeID, opts?): Promise<MemoryEntriesResponse>
 *     getMemoryEntry(scope, scopeID, key): Promise<MemoryEntryResponse>
 *
 *     // Memory view — off-run search + embed admin (v1.47.0 — RFC BV; HTTP-only)
 *     memorySearch(input): Promise<MemorySearchResponse>          // unified k/v + document-chunk search
 *     memoryEmbedStats(scope): Promise<MemoryEmbedStatsResponse>
 *     reembedMemory(scope, scopeId, opts?): Promise<MemoryReembedResponse>  // dry_run defaults true
 *     backfillEmbeddings(scope, scopeId, opts?): Promise<MemoryBackfillResponse>   // embeds rows that have none
 *     purgeStaleEmbeddings(scope, scopeId, opts?): Promise<MemoryPurgeResponse>    // DELETES; dry_run defaults true
 *
 *     // Memory / document change feeds (RFC CD Part C; SSE; substrate:tenant)
 *     streamMemoryChanges(opts?): AsyncIterable<ChangeFeedFrame>     // feed | change | error (non-fatal); resume with opts.since = last seq
 *     streamDocumentChanges(opts?): AsyncIterable<ChangeFeedFrame>
 *
 *     // Interruption (v0.8.16; decline RFC BH)
 *     listUserInterrupts(userId, opts?): Promise<InterruptListResponse>
 *     listRunInterrupts(runId, opts?): Promise<InterruptListResponse>
 *     resolveInterrupt(runId, interruptId, opts): Promise<unknown>
 *     cancelInterrupt(runId, interruptId, opts?): Promise<unknown>
 *
 *     // Turn-scoped run control (RFC BH)
 *     cancelTurn(runId, opts?): Promise<CancelTurnResult>
 *
 *     // Substrate admin (v0.8.22; mcpServerDef v0.9.x; scheduleDef v1.x)
 *     agentDef(input): Promise<SubstrateToolResponse>
 *     skillDef(input): Promise<SubstrateToolResponse>
 *     mcpServerDef(input): Promise<SubstrateToolResponse>
 *     mcpServerDefVerify(name, sha): Promise<MCPServerDefVerifyResult>   // v0.18.0
 *     ensureMcpServer(opts): Promise<EnsureMcpServerResult>              // v0.18.0 — idempotent register-if-changed
 *     scheduleDef(input): Promise<SubstrateToolResponse>                // input: ScheduleDefInput | SubstrateToolInput
 *     webhookDef(input): Promise<SubstrateToolResponse>                 // input: WebhookDefInput | SubstrateToolInput
 *     hookDef(input): Promise<SubstrateToolResponse>                    // input: HookDefInput | SubstrateToolInput
 *     ensureHookDef(name, overlay, opts?): Promise<EnsureHookDefResult>  // idempotent register-if-changed
 *     createHookDef / forkHookDef(name, overlay, opts?)                  // overlay strictly typed, no loose fallback
 *     createScheduleDef / forkScheduleDef(name, overlay, opts?)          // ditto (overlay open: types of known keys checked)
 *     createWebhookDef / forkWebhookDef(name, overlay, opts?)            // ditto
 *
 *     // Def name enumeration (tenant-scoped; opts.tenant = admin focus)
 *     listAgentDefNames / listSkillDefNames / listHookDefNames / listMcpServerDefNames /
 *     listScheduleDefNames / listA2AServerCardDefNames / listA2AAgentDefNames /
 *     listWebhookDefNames / listMemoryBackendDefNames / listDocumentSourceDefNames
 *       (opts?): Promise<DefNamesResponse>
 *     listOperatorTokenDefNames(opts?): Promise<OperatorTokenDefNamesResponse>   // admin token only
 *
 *     // Access control (RFC EY / EZ; tenant operator; opts.tenant = admin focus)
 *     listGroups / getGroup(name) / getGroupEffective(name) / createGroup(body) /
 *     deleteGroup(name) / addGroupMember(name, {kind, id}) / removeGroupMember(name, kind, id)
 *     aclDef(input): Promise<unknown>                             // op-dispatched; names → GET /v1/_acldef/names
 *     listAclDefs / getAclDef(name) / putAclDef(body) / retireAclDef(name) /
 *     validateAclRules(rules) / getAclCatalog() / listAclDefNames()
 *     listAclBindings / createAclBinding(body) / deleteAclBinding(bindingId)
 *     authzEvaluation(req) / authzEvaluations(req) / authzExplain(req) / authzSimulate(req)   // any principal, about itself
 *
 *     // Schedules admin
 *     listAllSchedules(opts?): Promise<ScheduleListResponse>      // static yaml + ScheduleDefs, merged
 *     getScheduleState(defId): Promise<ScheduleStateView>
 *     runScheduleNow(defId): Promise<ScheduleRunNowResult>        // fires on the next tick
 *     pauseSchedule(defId): Promise<SchedulePauseResult>          // indefinite
 *     resumeSchedule(defId): Promise<SchedulePauseResult>
 *
 *     // Dynamic filesystem volumes (v0.35.0 — RFC AH; tenant-confined)
 *     volumeDef(input): Promise<SubstrateToolResponse>            // create/get/list/delete/purge
 *     listVolumes(): Promise<PersistentVolumesResponse>
 *     listEphemeralVolumes(): Promise<EphemeralVolumesResponse>
 *
 *     // App clients (RFC FL ClientDef; tenant operator or admin)
 *     clientDef(input): Promise<unknown>                          // POST /v1/_clientdef
 *     createClientDef / updateClientDef(name, client, opts?): Promise<ClientDefRecord>
 *     getClientDef({name | def_id}): Promise<AppClientEntry>
 *     listClientDefs(opts?): Promise<ClientDefListResponse>
 *     retireClientDef(name): Promise<ClientDefRetireResult>
 *     verifyClientDef(name, {contentSha256}?): Promise<ClientDefVerifyResult>
 *     listClientDefNames(opts?): Promise<ClientDefNamesResponse> // GET /v1/_clientdef/names
 *
 *     // App sign-in (RFC FL) — also standalone as `new LoomcycleOAuth(...)`
 *     oauth: LoomcycleOAuth   // beginAuthorize / completeAuthorize / refresh / revoke / discover
 *     new LoomcycleClient({ tokenProvider: new OAuthTokenProvider(...) })  // refreshes on its own
 *
 *     // Agent Teams (RFC AP — TeamDef substrate; tenant-confined)
 *     listTeams(): Promise<ListTeamsResponse>
 *     renderTeamDiagram(name, opts?): Promise<TeamDiagram>
 *     getTeamDef(defId): Promise<TeamDefDetail>
 *     createTeam(name, overlay, opts?): Promise<CreatedTeam>       // opts: promote (default true) / description
 *     forkTeam(name, overlay, opts?): Promise<CreatedTeam>         // opts: promote / parentDefId / description
 *     deleteTeam(name): Promise<{name, deleted}>
 *     runTeam({name|defId, input}): Promise<TeamRunResult>
 *     runTeam({..., mode:"detach"}): Promise<TeamRunDetached>     // the handle, now — the walk runs on
 *     getRunBreakpoints(runId): Promise<TeamBreakpoints>          // debug a walk that is already running
 *     setRunBreakpoints(runId, breakpoints): Promise<TeamBreakpoints>
 *
 *     // Path VFS + chunked-graph Documents on the wire (v1.4.0 — RFC AL / RFC AK)
 *     path(input): Promise<PathToolResponse>                      // resolve/ls/stat/mkdir/mv/rm
 *     document(input): Promise<DocumentToolResponse>              // 13 ops; needs SQL Memory
 *
 *     // History — browse/search/annotate past chats (RFC BE; tenant-confined)
 *     history(input): Promise<HistoryToolResponse>                // list/get/search/rename/annotate/pin/archive/recap/resume
 *
 *     // Evaluation — score runs and read the scores back, outside a run
 *     evaluation(input): Promise<…>                               // submit/get/list_for_run/list_for_def/aggregate
 *     submitEvaluation(runId, score, opts?): Promise<EvaluationSubmitResult>
 *     getEvaluation(evalId): Promise<EvaluationRecord>
 *     listEvaluationsForRun(runId, opts?): Promise<EvaluationListForRunResult>
 *     listEvaluationsForDef(defId, opts?): Promise<EvaluationListForDefResult>
 *     aggregateEvaluations(defId, opts?): Promise<EvaluationAggregate>
 *
 *     // Library v2 enumeration (v0.10.3 — yaml+substrate merged)
 *     listLibraryAgents(): Promise<LibraryListResponse<LibraryAgentDefinition>>
 *     listLibrarySkills(): Promise<LibraryListResponse<LibrarySkillDefinition>>
 *     listLibraryMcpServers(): Promise<LibraryListResponse<LibraryMcpServerDefinition>>
 *
 *     // LLM Gateway (v0.11.0 — direct provider routing, no agent loop)
 *     llmChat(opts: LLMChatOptions): Promise<LLMChatResponse>
 *     llmStream(opts: LLMChatOptions): AsyncIterable<LLMChatStreamItem>
 *
 *     // OpenAI Embeddings compatibility shim (v0.11.4)
 *     embeddings(opts: LLMEmbeddingsOptions): Promise<LLMEmbeddingsResponse>
 *
 *   Errors (typed subclasses of LoomcycleError; see README for the
 *   full HTTP-status → typed-error mapping table):
 *     LoomcycleError, AgentNotFoundError, SessionNotFoundError,
 *     SessionBusyError, AgentIDInUseError, BackpressureError,
 *     RateLimitedError (subclass of BackpressureError),
 *     PerUserQuotaExhaustedError, AuthError, InsufficientScopeError (a 403
 *     naming the scope the token lacks), UnavailableError,
 *     InvalidArgumentError,
 *     PauseNotConfiguredError (subclass of UnavailableError),
 *     AlreadyPausingError, NotPausedError, SnapshotNotFoundError,
 *     SnapshotTooLargeError, RequestTooLargeError + PromptTooLargeError
 *     (both subclasses of SnapshotTooLargeError), TokenLimitExceededError
 *     (NOT a BackpressureError: a hard budget, not retryable),
 *     SnapshotVersionError, SubstrateToolRefusedError (v0.8.22),
 *     AclDeniedError (a 403 an access-control rule decided; action,
 *     resource, reasons).
 *     Every one carries `retryable` (true only for a refusal that clears
 *     on its own).
 *
 * Transport: HTTP+SSE. Auth: Bearer token via the Authorization
 * header. Designed for Node ≥18 (engines pinned); Bun/Deno likely
 * work but untested. Browser support is not a target (use the
 * Web UI for browser-side operator control).
 *
 * See `adapters/ts/README.md` for usage examples.
 */

export { LoomcycleClient } from "./client.js";
export { InteractiveSession } from "./interactive.js";
export type { InteractiveSessionOps } from "./interactive.js";
export { verifyLoomcycleSignature, verifyLoomcycleDelivery } from "./signature.js";
export type { VerifySignatureOptions, VerifiedDelivery } from "./signature.js";
export { ClientToolHost, clientToolsURL, CLIENT_TOOL_SUBPROTOCOL } from "./client-tools.js";
export type {
  ClientToolSchema,
  ClientToolInvocation,
  ConnectClientToolsOptions,
  WebSocketLike,
  WebSocketCtor,
} from "./client-tools.js";
export { LoomcycleOAuth, OAuthTokenProvider, parseAuthorizeResponse, pkceChallenge } from "./oauth.js";
export type { LoomcycleOAuthOptions, OAuthTokenProviderOptions } from "./oauth.js";

export type {
  // Run lifecycle
  AgentEvent,
  ClientOptions,
  ContinueOptions,
  EventType,
  HostWidening,
  ImageMediaType,
  ParentContext,
  PromptContent,
  PromptSegment,
  RetryInfo,
  RunOptions,
  RunOverrideOptions,
  SamplingOptions,
  CompactionOptions,
  ContextOptions,
  ToolUse,
  Usage,
  CallTiming,
  // Agent metadata
  Agent,
  AgentStatus,
  AwaitedState,
  AgentUsage,
  RunResult,
  RunSpec,
  RunSpecTeam,
  ConfiguredRun,
  ConfiguredRunPatch,
  ToolChoiceOptions,
  OutputFormatOptions,
  CancelAgentResult,
  ListAgentsResponse,
  ListUserAgentsOptions,
  AgentList,
  WalkRunsPage,
  // Fan-out (RFC Y) + compaction
  RunBatchOptions,
  RunBatchResult,
  SpawnRunResult,
  CompactRunResult,
  RunTreeDeletion,
  RunTreeNotReached,
  DirectoryBudget,
  DirectoryInspection,
  DirectoryTenant,
  DirectoryUser,
  ErasureAccess,
  ErasureReport,
  ErasureResidue,
  ErasureResult,
  ErasureTier,
  MergeSubjectsResult,
  SubjectRef,
  ErasureTokenRef,
  CancelTurnResult,
  ReviewRunResult,
  ReplaySessionResult,
  // Transcript
  TranscriptEvent,
  TranscriptResponse,
  // Health + Users
  HealthResponse,
  ListUsersResponse,
  UserSummary,
  // RFC BX Phase 2: tenant-owned users + delegated per-user tokens (v1.50.0+)
  UserStatus,
  UserRecord,
  CreateUserBody,
  UpdateUserBody,
  MintedUserToken,
  TokenRestrictions,
  UserTokenMeta,
  ListUserTokensResponse,
  UserIdentity,
  UserIdentitiesResponse,
  // RFC FA: sign-in gates + the password gate's management API
  AuthGateKind,
  AuthGate,
  AuthGateKey,
  AuthGateRemoval,
  AuthGateProbe,
  AuthGateListResponse,
  CreateInviteBody,
  CreatedInvite,
  AuthInviteState,
  AuthInvite,
  AuthInviteListResponse,
  CreatedPasswordReset,
  GateIdentity,
  GateIdentitiesResponse,
  GateTenantStatus,
  // RFC BY: runnable-agent discovery (v1.51.0+)
  RunnableAgent,
  RunnableAgentsResponse,
  // Whoami / principal (RFC L, v0.17.0)
  WhoamiResponse,
  WhoamiEffective,
  WhoamiAuth,
  // Pause / Resume / State
  PauseResult,
  RetentionOverride,
  ResumeResult,
  RuntimeStateResponse,
  RuntimeStateStatus,
  // Resolver re-probe (issue #88)
  ResolverMatrix,
  ResolverModelStatus,
  ResolverProviderAvailability,
  // Snapshot
  CreateSnapshotOptions,
  SnapshotCreateResponse,
  SnapshotDescriptor,
  SnapshotEnvelope,
  SnapshotListResponse,
  SnapshotRestoreResponse,
  // Memory
  MemoryEntriesResponse,
  MemoryEntry,
  MemoryEntryResponse,
  MemoryScopeIDsResponse,
  MemoryScopeIDSummary,
  MemoryScopeKind,
  MemoryScopesResponse,
  // Memory view (RFC BV): off-run search + embed admin
  MemorySearchDocumentFilter,
  MemorySearchInput,
  MemorySource,
  MemoryWhen,
  MemoryTimeFilter,
  MemorySearchEntry,
  MemorySearchResponse,
  MemoryEmbedModelStats,
  MemoryEmbedStatsResponse,
  MemoryReembedConfigured,
  MemoryReembedResponse,
  MemoryBackfillResponse,
  MemoryPurgeResponse,
  // Interruption
  InterruptListResponse,
  InterruptRow,
  InterruptStatus,
  ResolveInterruptOptions,
  // Hook payloads (what a webhook receives and returns)
  ChannelHookCall,
  ChannelHookResult,
  HookFailMode,
  HookPhase,
  HookToolCall,
  HookToolResult,
  PostHookCall,
  PostHookResult,
  PreHookCall,
  PreHookResult,
  // Substrate admin (v0.8.22)
  SubstrateToolInput,
  SubstrateToolResponse,
  // RFC AP Agent Teams (TeamDef substrate)
  TeamNameSummary,
  ListTeamsResponse,
  TeamDiagram,
  CreatedTeam,
  PromotedTeam,
  RetiredTeam,
  TeamDefDetail,
  TeamVerification,
  TeamDraft,
  TeamDraftVerification,
  TeamIssue,
  TeamIssueKind,
  TeamVersion,
  TeamVersionList,
  TeamBreakpoints,
  TeamRunDetached,
  TeamRunResult,
  TeamRunTarget,
  CreateTeamOptions,
  ForkTeamOptions,
  // Def name enumeration (GET /v1/_<family>def/names)
  DefNameSummary,
  DefNamesResponse,
  OperatorTokenDefNameSummary,
  OperatorTokenDefNamesResponse,
  // Schedules admin (/v1/_schedules/...)
  ScheduleListEntry,
  ScheduleListResponse,
  ScheduleActiveRun,
  ScheduleStateView,
  ScheduleRunNowResult,
  SchedulePauseResult,
  // Model aliases, routing view, live provider models
  ModelAlias,
  ModelAliasesResponse,
  RoutingResponse,
  RoutingUserTier,
  RoutingTier,
  RoutingCandidate,
  RoutingThroughput,
  RoutingProvider,
  SearchRoutingProvider,
  RoutingEmbedder,
  RoutingDecision,
  RoutingDecisionModel,
  ProviderModelsResponse,
  TeamRunStep,
  TeamDecisionHandler,
  TeamDecisionQuestion,
  TeamDecisionAnswer,
  // RFC AL Path VFS + RFC AK Document on the wire (v1.4.0)
  PathToolInput,
  PathToolResponse,
  DocumentToolInput,
  DocumentEdgeEvidence,
  DocumentToolResponse,
  DocumentSearchFilter,
  DocumentSearchHit,
  DocumentSearchResult,
  DocumentRelatedResult,
  // RFC BE History (browse/search/annotate past chats)
  HistoryToolInput,
  HistoryToolResponse,
  // Evaluation outside a run (POST /v1/_evaluation)
  EvaluationEmitterRole,
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
  EvaluationScoreStats,
  EvaluationAggregate,
  // RFC AH dynamic volumes (v0.35.0)
  VolumeMode,
  PersistentVolumeEntry,
  PersistentVolumesResponse,
  EphemeralVolumeEntry,
  EphemeralVolumesResponse,
  // Transcript first-cycle types (v0.9.1)
  SystemPromptPayload,
  UserInputPayload,
  // n8n RFC Phase 0 (v0.9.x)
  ChannelDescriptor,
  ListChannelsResponse,
  RunStateEvent,
  RunStateStreamClose,
  RunStateStreamItem,
  RunStateStreamOpen,
  StreamUserRunStatesOptions,
  // Memory / document change feeds (RFC CD Part C)
  ChangeFeedError,
  ChangeFeedFrame,
  ChangeFeedStatus,
  EmbedderHealth,
  MemoryChange,
  MemoryChangeScope,
  MemoryChangeType,
  StreamChangesOptions,
  // Channel CRUD (v0.9.x)
  AckChannelOptions,
  ChannelAckResult,
  ChannelMessageItem,
  ChannelPeekResult,
  ListTeamChannelsOptions,
  PeekTeamChannelOptions,
  ReleaseTeamChannelOptions,
  TeamChannelReleaseResult,
  TeamChannelDeclaredIn,
  TeamChannelDescriptor,
  TeamChannelPeekResult,
  TeamChannelsResult,
  ChannelPublishResult,
  ChannelPurgeResult,
  ChannelReleaseResult,
  ChannelScope,
  ChannelSubscribeResult,
  PeekChannelOptions,
  PublishChannelOptions,
  SubscribeChannelOptions,
  // Channel fan-in / fan-out (RFC S client twins)
  AwaitChannelsOptions,
  BroadcastChannelsOptions,
  ChannelAwaitEntry,
  ChannelAwaitMode,
  ChannelAwaitResult,
  ChannelBroadcastEntry,
  ChannelBroadcastResult,
  // Channel admin CRUD (v0.11.5)
  CreateChannelOptions,
  ReleaseChannelOptions,
  UpdateChannelOptions,
  // Memory entry admin CRUD (v0.11.5)
  SetMemoryEntryOptions,
  SetMemoryEntryResponse,
  // Content signatures (v0.9.x)
  AgentDefRowResponse,
  AgentDefVerifyResult,
  SkillDefVerifyResult,
  // Dynamic MCP server registration (v0.9.x)
  EnsureMcpServerOptions,
  EnsureMcpServerResult,
  MCPServerDefRowResponse,
  MCPServerDefVerifyResult,
  // Inline code-js agent ingestion (v0.19.0, RFC J)
  AgentDefOverlay,
  AgentRouting,
  AgentChannelACL,
  AgentInterruptionACL,
  AgentDecision,
  MemoryRerank,
  AgentSampling,
  AgentCompaction,
  AgentTranscription,
  AgentSpeech,
  AgentContext,
  TierCandidate,
  CoreBlock,
  HookEvent,
  InlineWebhook,
  HookEntry,
  EventHooks,
  ToolEntry,
  ToolHooksByTool,
  EnsureCodeAgentOptions,
  EnsureCodeAgentResult,
  // Typed HookDef / ScheduleDef / WebhookDef inputs (RFC EO)
  CreateDefOptions,
  ForkDefOptions,
  EnsureHookDefOptions,
  EnsureHookDefResult,
  HookDefBody,
  HookDefForkOverlay,
  HookDefInput,
  HookDefMatch,
  HookDefOverlay,
  HookDefRowResponse,
  HookDefVerifyResult,
  ScheduleDefInput,
  ScheduleDefOverlay,
  ScheduleDefRowResponse,
  ScheduledRunHook,
  ScheduledRunPromptContent,
  ScheduledRunPromptSegment,
  WebhookDefAuth,
  WebhookDefFilter,
  WebhookDefInput,
  WebhookDefOverlay,
  WebhookDefRateLimit,
  WebhookDefRowResponse,
  WebhookDefDeliveries,
  WebhookDefSyncResponse,
  // Library v2 enumeration (v0.10.3)
  LibraryAgentDefinition,
  LibraryEntry,
  LibraryListResponse,
  LibraryMcpServerDefinition,
  LibrarySkillDefinition,
  // LLM Gateway (v0.11.0)
  LLMChatContent,
  LLMChatMessage,
  LLMChatOptions,
  LLMChatResponse,
  LLMChatStreamDelta,
  LLMChatStreamItem,
  LLMChatToolCall,
  LLMChatUsage,
  LLMTool,
  // OpenAI Embeddings compatibility shim (v0.11.4)
  LLMEmbeddingItem,
  LLMEmbeddingsOptions,
  LLMEmbeddingsResponse,
  LLMEmbeddingsUsage,
  // RFC AV usage/cost report
  UsageDimension,
  UsageBucket,
  UsageAggregate,
  UsageReportResponse,
  // RFC AW per-scope token budgets
  CapabilityInertInfo,
  LimitInfo,
  FileCapturedInfo,
  TranscriptionInfo,
  // Run admission: the payload on event: queued / event: admitted
  AdmissionInfo,
  // A failure's structure (event: error, a failed event: tool_result) and the
  // hook decision payload — both carried on AgentEvent, now importable.
  ErrorInfo,
  CallFormat,
  CallRef,
  HookDecisionInfo,
  TeamDecisionInfo,
  // The remaining AgentEvent payloads — each types the field one event kind
  // carries (`ev.fallback`, `ev.context_state`, …), so a consumer that
  // narrows on `type` can name what it holds.
  OverrideInfo,
  FallbackInfo,
  InterruptionEventInfo,
  SpawnChildInfo,
  TurnCancelledInfo,
  ChannelEventInfo,
  ContextCompactionInfo,
  ContextRecapInfo,
  ContextStateInfo,
  ContextExhaustedInfo,
  ContextTierVerdict,
  ContextDistillDeclinedInfo,
  EffectiveConfigResponse,
  RunFileInput,
  RunFileImportSource,
  RunFileImportHeading,
  RunInboxDownload,
  EffectiveConfigSource,
  EffectiveValue,
  InertContextSetting,
  AgentOverridableResponse,
  OverridableField,
  OverrideLockReason,
  RetuneRunResponse,
  RunConfigRecord,
  SubagentHoldInfo,
  RunConfigResponse,
  RunPromptResponse,
  RunPromptSnapshot,
  PromptBlock,
  TokenLimit,
  TokenLimitsResponse,
  ConfigResponse,
  ConfigProvider,
  ConfigModel,
  ConfigSearch,
  ConfigInstance,
  SetTokenLimitRequest,
  // RFC AR credential store (RFC CN adapter parity)
  CredentialScope,
  CredentialMeta,
  CredentialListResponse,
  // RFC EP crypto keys + the Crypto tool (POST /v1/_crypto)
  CryptoKeyVersion,
  CryptoKeyExport,
  CryptoKeyType,
  CryptoKeyEncoding,
  CryptoKeyBackend,
  CryptoKeyExternal,
  CryptoOp,
  CryptoRequestBase,
  CryptoKeyRef,
  CryptoEncryptRequest,
  CryptoDecryptRequest,
  CryptoRewrapRequest,
  CryptoMacRequest,
  CryptoVerifyMacRequest,
  CryptoSignRequest,
  CryptoVerifyRequest,
  CryptoPublicKeyRequest,
  CryptoTotpRequest,
  CryptoHashRequest,
  CryptoRandomRequest,
  CryptoHashPasswordRequest,
  CryptoVerifyPasswordRequest,
  CryptoRequest,
  CryptoEncryptResult,
  CryptoDecryptResult,
  CryptoRewrapResult,
  CryptoMacResult,
  CryptoVerifyResult,
  CryptoSignResult,
  CryptoPublicJWK,
  CryptoPublicKeyResult,
  CryptoTotpResult,
  CryptoHashResult,
  CryptoRandomResult,
  CryptoHashPasswordResult,
  CryptoVerifyPasswordResult,
  CryptoResultByOp,
  CryptoResult,
  // Decision models
  DecideRequest,
  DecideResponse,
  DecisionQuestion,
  DecisionChoiceQuestion,
  DecisionNoulQuestion,
  DecisionScoreQuestion,
  DecisionAnswer,
  DecisionChoiceAnswer,
  DecisionNoulAnswer,
  DecisionScoreAnswer,
  DecisionModel,
  DecisionModelsResponse,
  // Access control: groups, rule lists, bindings, decisions (RFC EY / EZ)
  AclMode,
  AclSource,
  AclEffect,
  AclSubjectKind,
  AclCondition,
  AclRule,
  AclList,
  AclDefRow,
  AclDefListResponse,
  AclDefNamesResponse,
  AclValidateResult,
  AclCatalogType,
  AclCatalog,
  AclDefOp,
  AclDefInput,
  GroupRow,
  GroupMemberKind,
  GroupMember,
  GroupListResponse,
  GroupDetail,
  GroupEffective,
  CreateGroupBody,
  GroupMemberChange,
  AclBinding,
  AclBindingListResponse,
  CreateAclBindingBody,
  AuthzReason,
  AuthzDecision,
  AuthzRequestContext,
  AuthzEvaluationRequest,
  AuthzEvaluationsSemantic,
  AuthzEvaluationsRequest,
  AuthzEvaluationsResponse,
  AuthzTrace,
  AuthzAppliedBinding,
  AuthzExplainResponse,
  AuthzDraftBinding,
  AuthzSimulateRequest,
  AuthzSimulateResult,
  AuthzSimulateResponse,
  // App sign-in + app clients (RFC FL)
  OAuthServerMetadata,
  OAuthTokenResponse,
  OAuthTokenSet,
  BeginAuthorizeOptions,
  AuthorizeRequest,
  AuthorizeResponseParams,
  CompleteAuthorizeOptions,
  RefreshTokenOptions,
  RevokeTokenOptions,
  TokenProvider,
  AppClientRefresh,
  AppClientConfig,
  ClientDefInput,
  ClientDefRecord,
  ConfigAppClient,
  AppClientEntry,
  ClientDefListResponse,
  ClientDefVerifyResult,
  ClientDefRetireResult,
  ClientDefNameSummary,
  ClientDefNamesResponse,
  DeviceLoginPrompt,
  DeviceLoginOptions,
  DeviceLoginResult,
} from "./types.js";

export {
  AclDeniedError,
  AgentIDInUseError,
  AgentNotFoundError,
  AlreadyPausingError,
  AuthError,
  BackpressureError,
  DeviceLoginError,
  NotFoundError,
  InsufficientScopeError,
  InvalidArgumentError,
  ChannelCursorRegressionError,
  LoomcycleError,
  NotPausedError,
  OAuthError,
  PauseNotConfiguredError,
  PerUserQuotaExhaustedError,
  PromptTooLargeError,
  RateLimitedError,
  RequestTooLargeError,
  SessionBusyError,
  SessionNotFoundError,
  SnapshotNotFoundError,
  SnapshotTooLargeError,
  SnapshotVersionError,
  SubstrateToolRefusedError,
  TokenLimitExceededError,
  UnavailableError,
} from "./errors.js";
