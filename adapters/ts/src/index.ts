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
 *     listUsers(): Promise<ListUsersResponse>
 *
 *     // Pause / Resume / State (v0.8.17/8.18)
 *     pauseRuntime(opts?): Promise<PauseResult>
 *     resumeRuntime(): Promise<ResumeResult>
 *     getRuntimeState(): Promise<RuntimeStateResponse>
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
 *     // Substrate admin (v0.8.22)
 *     agentDef(input): Promise<SubstrateToolResponse>
 *     skillDef(input): Promise<SubstrateToolResponse>
 *
 *     // Library v2 enumeration (v0.10.3 — yaml+substrate merged)
 *     listLibraryAgents(): Promise<LibraryListResponse<LibraryAgentDefinition>>
 *     listLibrarySkills(): Promise<LibraryListResponse<LibrarySkillDefinition>>
 *     listLibraryMcpServers(): Promise<LibraryListResponse<LibraryMcpServerDefinition>>
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

export type {
  // Run lifecycle
  AgentEvent,
  ClientOptions,
  ContinueOptions,
  EventType,
  HostWidening,
  PromptContent,
  PromptSegment,
  RetryInfo,
  RunOptions,
  ToolUse,
  Usage,
  // Agent metadata
  Agent,
  AgentStatus,
  AgentUsage,
  CancelAgentResult,
  ListAgentsResponse,
  // Transcript
  TranscriptEvent,
  TranscriptResponse,
  // Health + Users
  HealthResponse,
  ListUsersResponse,
  UserSummary,
  // Pause / Resume / State
  PauseResult,
  ResumeResult,
  RuntimeStateResponse,
  RuntimeStateStatus,
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
  ChannelScope,
  ChannelSubscribeResult,
  PeekChannelOptions,
  PublishChannelOptions,
  SubscribeChannelOptions,
  // Content signatures (v0.9.x)
  AgentDefRowResponse,
  AgentDefVerifyResult,
  SkillDefVerifyResult,
  // Dynamic MCP server registration (v0.9.x)
  MCPServerDefRowResponse,
  MCPServerDefVerifyResult,
  // Library v2 enumeration (v0.10.3)
  LibraryAgentDefinition,
  LibraryEntry,
  LibraryListResponse,
  LibraryMcpServerDefinition,
  LibrarySkillDefinition,
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
