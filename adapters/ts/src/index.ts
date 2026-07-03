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
 *     cancelAgent(agentId, opts?): Promise<CancelAgentResult>
 *     listUserAgents(userId, opts?): Promise<Agent[]>
 *     getTranscript(sessionId): Promise<TranscriptResponse>
 *     health(): Promise<HealthResponse>
 *     listUsers(opts?): Promise<ListUsersResponse>   // tenant-scoped (RFC L)
 *     whoami(): Promise<WhoamiResponse>               // RFC L principal (v0.17.0)
 *
 *     // Pause / Resume / State (v0.8.17/8.18)
 *     pauseRuntime(opts?): Promise<PauseResult>
 *     resumeRuntime(): Promise<ResumeResult>
 *     getRuntimeState(): Promise<RuntimeStateResponse>
 *     resolveProbe(opts?): Promise<ResolverMatrix>
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
 *     // Interruption (v0.8.16)
 *     listUserInterrupts(userId, opts?): Promise<InterruptListResponse>
 *     listRunInterrupts(runId, opts?): Promise<InterruptListResponse>
 *     resolveInterrupt(runId, interruptId, opts): Promise<unknown>
 *
 *     // Substrate admin (v0.8.22; mcpServerDef v0.9.x; scheduleDef v1.x)
 *     agentDef(input): Promise<SubstrateToolResponse>
 *     skillDef(input): Promise<SubstrateToolResponse>
 *     mcpServerDef(input): Promise<SubstrateToolResponse>
 *     mcpServerDefVerify(name, sha): Promise<MCPServerDefVerifyResult>   // v0.18.0
 *     ensureMcpServer(opts): Promise<EnsureMcpServerResult>              // v0.18.0 — idempotent register-if-changed
 *     scheduleDef(input): Promise<SubstrateToolResponse>
 *
 *     // Dynamic filesystem volumes (v0.35.0 — RFC AH; tenant-confined)
 *     volumeDef(input): Promise<SubstrateToolResponse>            // create/get/list/delete/purge
 *     listVolumes(): Promise<PersistentVolumesResponse>
 *     listEphemeralVolumes(): Promise<EphemeralVolumesResponse>
 *
 *     // Path VFS + chunked-graph Documents on the wire (v1.4.0 — RFC AL / RFC AK)
 *     path(input): Promise<PathToolResponse>                      // resolve/ls/stat/mkdir/mv/rm
 *     document(input): Promise<DocumentToolResponse>              // 13 ops; needs SQL Memory
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
 *     AuthError, UnavailableError, InvalidArgumentError,
 *     PauseNotConfiguredError (subclass of UnavailableError),
 *     AlreadyPausingError, NotPausedError, SnapshotNotFoundError,
 *     SnapshotTooLargeError, SnapshotVersionError,
 *     SubstrateToolRefusedError (v0.8.22)
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
  SamplingOptions,
  CompactionOptions,
  ToolUse,
  Usage,
  // Agent metadata
  Agent,
  AgentStatus,
  AgentUsage,
  CancelAgentResult,
  ListAgentsResponse,
  // Fan-out (RFC Y) + compaction
  RunBatchOptions,
  RunBatchResult,
  SpawnRunResult,
  CompactRunResult,
  // Transcript
  TranscriptEvent,
  TranscriptResponse,
  // Health + Users
  HealthResponse,
  ListUsersResponse,
  UserSummary,
  // Whoami / principal (RFC L, v0.17.0)
  WhoamiResponse,
  // Pause / Resume / State
  PauseResult,
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
  // Interruption
  InterruptListResponse,
  InterruptRow,
  InterruptStatus,
  ResolveInterruptOptions,
  // Hook management (PR C)
  Hook,
  HookFailMode,
  HookPhase,
  HookToolCall,
  HookToolResult,
  ListHooksResponse,
  PostHookCall,
  PostHookResult,
  PreHookCall,
  PreHookResult,
  RegisterHookOptions,
  RegisterHookResponse,
  // Substrate admin (v0.8.22)
  SubstrateToolInput,
  SubstrateToolResponse,
  // RFC AL Path VFS + RFC AK Document on the wire (v1.4.0)
  PathToolInput,
  PathToolResponse,
  DocumentToolInput,
  DocumentToolResponse,
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
  // Channel CRUD (v0.9.x)
  AckChannelOptions,
  ChannelAckResult,
  ChannelMessageItem,
  ChannelPeekResult,
  ChannelPublishResult,
  ChannelPurgeResult,
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
  EnsureCodeAgentOptions,
  EnsureCodeAgentResult,
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
  UsageAggregate,
  UsageReportResponse,
  // RFC AW per-scope token budgets
  LimitInfo,
  TokenLimit,
  TokenLimitsResponse,
  SetTokenLimitRequest,
} from "./types.js";

export {
  AgentIDInUseError,
  AgentNotFoundError,
  AlreadyPausingError,
  AuthError,
  BackpressureError,
  HookNotFoundError,
  NotFoundError,
  InvalidArgumentError,
  ChannelCursorRegressionError,
  LoomcycleError,
  NotPausedError,
  PauseNotConfiguredError,
  PerUserQuotaExhaustedError,
  SessionBusyError,
  SessionNotFoundError,
  SnapshotNotFoundError,
  SnapshotTooLargeError,
  SnapshotVersionError,
  SubstrateToolRefusedError,
  UnavailableError,
} from "./errors.js";
