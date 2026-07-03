// Package connector defines the abstract operation surface that every
// wire transport (HTTP, gRPC, MCP, future CLI) translates into. The
// Connector interface is the single canonical contract; transport
// adapters become thin wire-translation layers over it.
//
// Implementations and consumers:
//
//   - internal/api/http.Server     IMPLEMENTS  Connector (canonical;
//     existing handlers compose into the interface)
//   - internal/api/mcp.Server      CONSUMES    Connector (each
//     tools/call handler dispatches to a Connector method)
//   - internal/api/grpc.Server     CONSUMES    Connector (each proto
//     handler dispatches to a Connector method)
//   - adapters/ts (TypeScript)     MIRRORS     the operation surface
//     in TypeScript over HTTP wire
//   - future adapters/python       MIRRORS     same in Python
//   - future internal/api/cli      CONSUMES    Connector (`loomcycle run`)
//
// The MCP / gRPC / CLI servers hold a connector.Connector field and
// never make HTTP round-trips; they call methods directly on the
// underlying HTTP server (which holds the business logic). Adding a
// new wire surface is mechanical: implement input/output translation
// for each Connector method, no business logic duplication.
//
// Evolution policy: the interface is ADDITIVE going forward through
// v0.8.x. Adding methods is safe; changing signatures is a semver
// break and requires a v0.9.x bump.
package connector

import (
	"context"
	"encoding/json"

	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// Connector is the operation surface every wire transport exposes.
// See package doc for the architectural rationale.
//
// v0.8.18 status: Pause/Resume/Snapshot impls are real and delegate
// to the pause Manager + snapshot package. Wire shapes are identical
// to v0.8.15 (where the same methods returned PREVIEW placeholders)
// — orchestrators built against the v0.8.15 contracts continue to
// work; only the response semantics flip from placeholder to
// authoritative data. See errors.go for the typed errors transports
// translate into protocol-specific status codes.
type Connector interface {
	// --- Run lifecycle ---

	// SpawnRun is the alternate front-end to POST /v1/runs. Blocks
	// until the run completes (or fails); use Notify-style streaming
	// at the transport layer for live progress (e.g., MCP
	// notifications/loomcycle/run_event). Returns the final result
	// (text, stop_reason, usage) along with the assigned IDs.
	SpawnRun(ctx context.Context, req SpawnRunRequest) (SpawnRunResult, error)

	// SpawnRunBatch is the RFC Y external fan-out: one call spawns N fresh
	// child runs server-side concurrent (bounded by the same per-user
	// admission gate as SpawnRun), joins them, and returns the combined
	// index-aligned envelope. A per-child failure is captured in that child's
	// result and never fails the batch. Mirrors POST /v1/runs:batch and the
	// in-loop Agent op=parallel_spawn.
	SpawnRunBatch(ctx context.Context, req BatchSpawnRequest) (BatchSpawnResult, error)

	// CancelRun mirrors POST /v1/agents/{agent_id}/cancel. Cascades
	// to sub-agent runs spawned by this run. Idempotent.
	CancelRun(ctx context.Context, agentID, reason string) (CancelRunResult, error)

	// GetRun returns the latest status snapshot for a tracked
	// agent_id. Mirrors GET /v1/agents/{agent_id}.
	GetRun(ctx context.Context, agentID string) (Run, error)

	// CompactRun summarizes a run's conversation to free context and continue
	// from the summary — mirrors POST /v1/runs/{run_id}/compact. Keyed by
	// run_id (transports holding an agent_id resolve it via GetRun first). A
	// live run must be parked; a mid-turn run is refused. Cross-tenant is an
	// opaque not-found.
	CompactRun(ctx context.Context, runID string) (CompactResult, error)

	// ListRuns enumerates runs matching the filter. Mirrors
	// GET /v1/runs (with optional user_id / status filters).
	ListRuns(ctx context.Context, filter ListRunsFilter) ([]Run, error)

	// --- Agent management ---

	// RegisterAgent adds a dynamic agent that survives until its TTL
	// expires (or until UnregisterAgent is called). Returns the
	// effective AgentDescriptor — note allowed_tools may have been
	// stripped if Bash/Write/Edit were requested without the
	// LOOMCYCLE_MCP_ALLOW_PRIVILEGED_TOOLS opt-in.
	RegisterAgent(ctx context.Context, req RegisterAgentRequest) (AgentDescriptor, error)

	// UnregisterAgent removes a dynamic agent immediately. Returns
	// nil if the agent didn't exist (idempotent). Cannot unregister
	// static agents declared in YAML — that returns an error.
	UnregisterAgent(ctx context.Context, name string) error

	// ListAgents returns all known agents — both static (from
	// cfg.Agents / discovery) and dynamic (TTL-active rows from
	// dynamic_agents). includeDynamic=false returns only static.
	ListAgents(ctx context.Context, includeDynamic bool) ([]AgentDescriptor, error)

	// --- Builtin tool invocations ---
	//
	// Each builtin's discriminated-op input shape stays the authority
	// for inner-op validation. The connector passes raw JSON through
	// to tool.Execute; the result Text is the model-facing payload
	// (typically JSON for builtin tools). Transport adapters wrap
	// (text, is_error) in their wire-shape (e.g. MCP's content+isError).
	//
	// Operator-level ctx is the responsibility of the TRANSPORT adapter
	// (MCP / gRPC / future CLI), NOT the connector. The connector is
	// intentionally policy-agnostic — the caller attaches the right
	// memory_scopes / channel ACL / evaluation / agent_def / history
	// policy on ctx BEFORE calling, otherwise the underlying tools
	// return default-deny refusals (every op fails).
	//
	// See internal/api/mcp/context.go (operatorCtx) for the MCP
	// transport's policy-enrichment helper. Future gRPC/CLI transports
	// that surface builtin tools directly will need their own
	// equivalent — the gRPC server today only exposes run-lifecycle
	// RPCs through Connector, so the issue doesn't arise there.

	Memory(ctx context.Context, input json.RawMessage) (ToolResult, error)
	Channel(ctx context.Context, input json.RawMessage) (ToolResult, error)
	AgentDef(ctx context.Context, input json.RawMessage) (ToolResult, error)
	SkillDef(ctx context.Context, input json.RawMessage) (ToolResult, error)
	Evaluation(ctx context.Context, input json.RawMessage) (ToolResult, error)
	Context(ctx context.Context, input json.RawMessage) (ToolResult, error)

	// Path — RFC AL Unix-like VFS over the dirents runtime-store table.
	// Op-discriminated (resolve / ls / stat / mkdir / mv / rm). Scope-aware
	// (agent / user / tenant) and tenant-isolated; gated purely by the
	// agent's allowed_tools (a dirent is a name, not an authority grant — no
	// separate scope policy). Reachable via POST /v1/_path, the gRPC Path RPC,
	// the LoomCycle MCP meta-tool `path`, and the TS/Python adapters'
	// client.path() — all dispatch through this single Connector method.
	Path(ctx context.Context, input json.RawMessage) (ToolResult, error)

	// Document — RFC AK chunked-graph documents. Op-discriminated (13 ops:
	// document/chunk lifecycle, edges, query_chunks, type defs). Scope-aware
	// (agent / user; tenant deferred) and tenant-isolated via the SQL Memory
	// scope key; gated by the agent's allowed_tools. Requires SQL Memory.
	// Reachable via POST /v1/_document, the gRPC Document RPC, the LoomCycle
	// MCP meta-tool `document`, and the TS/Python adapters' client.document()
	// — all dispatch through this single Connector method.
	Document(ctx context.Context, input json.RawMessage) (ToolResult, error)

	// MCPServerDef — v0.9.x dynamic MCP server registration substrate.
	// Op-discriminated (create / fork / get / list / promote / retire
	// / rediscover / verify). Operator-admin-only: NOT auto-attached
	// to any agent's per-run dispatcher. Reachable via the
	// POST /v1/_mcpserverdef admin endpoint, the LoomCycle MCP
	// meta-tool `mcpserverdef`, the gRPC MCPServerDef RPC, and the
	// TS adapter's client.mcpServerDef() method — all four dispatch
	// through this single Connector method.
	MCPServerDef(ctx context.Context, input json.RawMessage) (ToolResult, error)

	// ScheduleDef — v1.x dynamic scheduled-runs registration substrate.
	// Op-discriminated (create / fork / get / list / retire). Operator-
	// admin-only: NOT auto-attached to every agent's per-run dispatcher.
	// Reachable via POST /v1/_scheduledef admin endpoint, the LoomCycle
	// MCP meta-tool `scheduledef`, the gRPC ScheduleDef RPC, and the TS
	// adapter's client.scheduleDef() method — all four dispatch through
	// this single Connector method.
	ScheduleDef(ctx context.Context, input json.RawMessage) (ToolResult, error)

	// A2AServerCardDef — v1.x RFC G dynamic A2A-server-card registration
	// substrate. Op-discriminated (create / fork / get / list / retire).
	// Operator-admin-only: NOT auto-attached to every agent's per-run
	// dispatcher. Reachable via POST /v1/_a2aservercarddef admin endpoint,
	// the LoomCycle MCP meta-tool `a2aservercarddef`, the gRPC
	// A2AServerCardDef RPC, and the TS adapter's client.a2aServerCardDef()
	// method — all four dispatch through this single Connector method.
	A2AServerCardDef(ctx context.Context, input json.RawMessage) (ToolResult, error)

	// A2AAgentDef — v1.x RFC G dynamic A2A-agent registration substrate.
	// Op-discriminated (create / fork / get / list / retire). Same
	// operator-admin-only posture as A2AServerCardDef. Reachable via
	// POST /v1/_a2aagentdef admin endpoint, the LoomCycle MCP meta-tool
	// `a2aagentdef`, the gRPC A2AAgentDef RPC, and the TS adapter's
	// client.a2aAgentDef() method — all four dispatch through this single
	// Connector method.
	A2AAgentDef(ctx context.Context, input json.RawMessage) (ToolResult, error)

	// WebhookDef — v1.x RFC H inbound-webhook registration substrate.
	// Op-discriminated (create / fork / get / list / retire). Same
	// operator-admin-only posture as A2AAgentDef. Reachable via
	// POST /v1/_webhookdef admin endpoint, the LoomCycle MCP meta-tool
	// `webhookdef`, the gRPC WebhookDef RPC, and the TS adapter's
	// client.webhookDef() method — all four dispatch through this single
	// Connector method.
	WebhookDef(ctx context.Context, input json.RawMessage) (ToolResult, error)

	// MemoryBackendDef — RFC I MR-3a memory-backend registration
	// substrate. Op-discriminated (create / fork / get / list / retire).
	// Same operator-admin-only posture as WebhookDef. Reachable via
	// POST /v1/_memorybackenddef admin endpoint, the LoomCycle MCP
	// meta-tool `memorybackenddef`, the gRPC MemoryBackendDef RPC, and
	// the TS adapter's client.memoryBackendDef() method — all four
	// dispatch through this single Connector method.
	MemoryBackendDef(ctx context.Context, input json.RawMessage) (ToolResult, error)

	// OperatorTokenDef — RFC L OSS multi-tenant authorization. Mints,
	// rotates, retires, and inspects the bearer tokens that replace the
	// single LOOMCYCLE_AUTH_TOKEN shared secret, each bound to an
	// authoritative principal (tenant + subject + scopes). Op-discriminated
	// (create / rotate / retire / get / list). OPERATOR-ADMIN only.
	// Reachable via POST /v1/_operatortokendef, the gRPC OperatorTokenDef
	// RPC, the LoomCycle MCP meta-tool `operatortokendef`, and the TS
	// adapter's client.operatorTokenDef() — all dispatch through here.
	OperatorTokenDef(ctx context.Context, input json.RawMessage) (ToolResult, error)

	// VolumeDef — RFC AH dynamic filesystem-volume substrate.
	// Op-discriminated (create / get / list / delete / purge — flat, NOT the
	// content-addressed retire/promote/fork of the families above, because a
	// volume points at mutable on-disk state). TENANT-CONFINED (ScopeTenant,
	// like AgentDef/SkillDef — not operator-admin-only): the tool stamps the
	// caller's authoritative tenant + opaque-404s cross-tenant. Reachable via
	// POST /v1/_volumedef admin endpoint, the gRPC VolumeDef RPC, the LoomCycle
	// MCP meta-tool `volumedef`, and the TS adapter's client.volumeDef() — all
	// dispatch through this single Connector method.
	VolumeDef(ctx context.Context, input json.RawMessage) (ToolResult, error)

	// CredentialDef — RFC AR secure per-tenant credential store. Op-discriminated
	// (create / get / list / delete). TENANT-CONFINED (ScopeTenant): the tool
	// stamps the caller's authoritative tenant, and for scope=user the caller's
	// OWN subject (per-user tokens). get/list return metadata only — never a
	// secret. Reachable via the MCP meta-tool `credentialdef` and in-band
	// (allowed_tools:[CredentialDef]); dispatches through this single method.
	CredentialDef(ctx context.Context, input json.RawMessage) (ToolResult, error)

	// --- Pause/Resume/Snapshot (real in v0.8.18) ---
	//
	// Wire shapes finalised v0.8.15. Real implementations landed in
	// v0.8.18 behind the same signatures. Per the locked RFC at
	// doc-internal/rfcs/pause-resume-snapshot.md.
	//
	// Error semantics: typed errors from errors.go let transports
	// map to protocol-specific status codes:
	//   ErrPauseNotConfigured     — 503 / Unavailable
	//   ErrAlreadyPausing         — 409 / FailedPrecondition
	//   ErrNotPaused              — 409 / FailedPrecondition
	//   ErrSnapshotNotFound       — 404 / NotFound
	//   ErrSnapshotTooLarge       — 413 / ResourceExhausted
	//   ErrSnapshotVersionTooNew  — 422 / FailedPrecondition
	//   ErrSnapshotVersionUnknown — 422 / FailedPrecondition

	PauseRuntime(ctx context.Context, timeoutMS int) (PauseResult, error)
	ResumeRuntime(ctx context.Context) (ResumeResult, error)
	GetRuntimeState(ctx context.Context) (RuntimeState, error)

	CreateSnapshot(ctx context.Context, req CreateSnapshotRequest) (SnapshotDescriptor, error)
	ListSnapshots(ctx context.Context) ([]SnapshotDescriptor, error)

	// GetSnapshot returns the full JSON envelope for a snapshot id —
	// distinct from ExportSnapshot, which is operator-facing "where
	// did this land on the host" semantics with a FilePath/Checksum.
	// Added in v0.8.18 (additive); transports map to GET /v1/_snapshots/{id}.
	GetSnapshot(ctx context.Context, snapshotID string) (SnapshotEnvelope, error)

	ExportSnapshot(ctx context.Context, snapshotID string) (ExportSnapshotResult, error)
	RestoreSnapshot(ctx context.Context, req RestoreSnapshotRequest) (RestoreSnapshotResult, error)
	DeleteSnapshot(ctx context.Context, snapshotID string) error

	// --- Resolver (operator re-probe; issue #88) ---
	//
	// ResolveProbe triggers an immediate, synchronous re-probe of every
	// configured provider and returns the refreshed availability matrix
	// — the operator escape hatch when a transient outage stalls every
	// provider and the runtime would otherwise 503 until the next
	// periodic probe. Transports map to POST /v1/_resolve/probe.
	//
	// Error semantics:
	//   ErrResolverUnavailable     — 503 / Unavailable (no resolver wired)
	//   ErrResolveProbeUnavailable — 503 / Unavailable (no probe loop wired)
	ResolveProbe(ctx context.Context) (ResolverMatrix, error)

	// --- Interruption (v0.8.16) ---

	// InterruptionResolve writes a resolution to a pending interrupt
	// row + wakes any blocked Interruption.ask waiter via the bus.
	// The LoomCycle MCP server's `interruption_resolve` meta-tool
	// surfaces this on its 21st tool slot so external orchestrators
	// (Claude Code, custom dashboards) can act as the answerer.
	//
	// The req payload is `kind`-discriminated; v0.8.16 supports only
	// kind=question with {answer, resolved_by?}. Future kinds slot
	// in as additional discriminator branches in the server-side
	// validator (closed enum: see doc-internal/rfcs/interruption-tool.md
	// §8).
	//
	// Returns ErrInterruptAlreadyTerminal-equivalent on conflict
	// (409 from the HTTP layer maps to this); ErrNotFound on a
	// missing interrupt_id; nil on success.
	InterruptionResolve(ctx context.Context, req InterruptionResolveRequest) (InterruptionResolveResult, error)

	// --- Hook management (PR A of the hooks-connector series) ---
	//
	// Hooks are in-memory only; registrations do not survive restarts.
	// The callback half is HTTP-only (consumer's own web server receives
	// the Pre/PostHookCall payloads); these Connector methods cover the
	// registration management surface only.
	//
	// Typed errors:
	//   ErrHookInvalidRegistration — bad owner/name/callback_url, etc.
	//                                Transports: HTTP 400 / InvalidArgument.
	//   ErrHookNotFound            — DeleteHook on unknown id.
	//                                Transports: HTTP 404 / NotFound.
	//   ErrHookNotConfigured       — Server has no hookRegistry wired
	//                                (test-harness guard; not seen in
	//                                production). Transports: Unavailable.

	RegisterHook(ctx context.Context, req RegisterHookRequest) (RegisterHookResponse, error)
	ListHooks(ctx context.Context) (ListHooksResponse, error)
	DeleteHook(ctx context.Context, id string) error

	// --- v0.9.x n8n RFC Phase 0 ---
	//
	// Two methods that surface the new HTTP endpoints (GET /v1/_channels
	// and GET /v1/users/{user_id}/agents/stream) to MCP + gRPC. Both
	// pure read paths; no business-logic divergence between transports.
	//
	// ListChannels is synchronous request/response — the operator
	// listing of declared channels with aggregate stats.
	//
	// StreamUserRunStates is the streaming counterpart to the SSE
	// endpoint. Visitor-pattern shape (callback per event) rather than
	// channel-return so the interface stays sync-style for transports
	// that don't natively stream (CLI's tabular output, future REST
	// long-poll). Visitors that return ErrStopStreaming exit cleanly;
	// other errors propagate. ctx cancel ends the stream with nil.
	//
	// Typed errors:
	//   ErrRunStateStreamUnavailable — runStateBus not wired on the
	//                                  underlying server (operator
	//                                  embedding skipped SetRunStateBus).
	//                                  Transports: Unavailable.

	ListChannels(ctx context.Context) (ListChannelsResponse, error)
	StreamUserRunStates(ctx context.Context, req StreamUserRunStatesRequest, visit RunStateVisitor) error

	// --- RFC AI interactive sessions ---
	//
	// SteerRun pushes an operator steering message into a LIVE interactive run
	// (one parked at end_turn, or mid-turn — drained at the next iteration
	// boundary). `source` is the auth-boundary-resolved origin ("api"|"webui"),
	// stamped by the caller, NOT wire-trusted. Cross-replica routing is
	// inherited from the underlying steer registry (a local miss delegates to
	// the cluster coordinator) — no extra work here. Tenant-ownership is gated:
	// a cross-tenant run is folded into ErrRunNotInFlight (opaque).
	// Reachable via POST /v1/runs/{run_id}/input + the gRPC RunInput RPC.
	//
	// Typed errors:
	//   ErrSteeringUnavailable — no steer registry wired. Transports: Unavailable.
	//   ErrRunNotInFlight      — no live run for run_id (or cross-tenant). NotFound.
	//   ErrSteerQueueFull      — the run's buffer is full. ResourceExhausted.
	SteerRun(ctx context.Context, runID, text, source string) (delivered bool, err error)

	// StreamRunEvents tails a single run's persisted events to a visitor,
	// replaying from fromSeq and live-tailing — the streaming counterpart to
	// GET /v1/runs/{run_id}/stream. The operator's own turns are replayed too
	// (RFC AI self-sufficient re-attach), so a cold client reconstructs the
	// whole conversation. Exits nil when the run is terminal and drained, or
	// when ctx fires (a client disconnect does NOT stop an interactive run; a
	// PARKED run is non-terminal so the tail stays open). Visitors that return
	// ErrStopStreaming exit cleanly; other visitor errors propagate.
	// Tenant-ownership gated (cross-tenant → ErrRunNotInFlight, opaque).
	// Reachable via GET /v1/runs/{run_id}/stream + the gRPC StreamRun RPC.
	//
	// Typed errors:
	//   ErrSteeringUnavailable — no persistence backend wired. Transports: Unavailable.
	//   ErrRunNotInFlight      — unknown or cross-tenant run_id. NotFound.
	StreamRunEvents(ctx context.Context, runID string, fromSeq int64, visit RunEventVisitor) error

	// Channel CRUD (v0.9.x): admin + per-user publish / subscribe /
	// peek / ack on operator-declared channels. Bearer-authed at the
	// HTTP transport boundary; scope + scope_id select the cursor
	// namespace (global = admin surface; user = per-end-user surface
	// via the /v1/users/{user_id}/channels/* route family). Mirrors
	// the in-band Channel tool's four ops by calling the SAME store
	// helpers — wire-surface parity for the n8n integration's
	// drop-in agentic-OS use case.
	//
	// SubscribeChannel returns a synchronous batch (long-poll up to
	// WaitMS, single round-trip). For a server-pushed stream use
	// StreamUserRunStates or rebuild via repeated SubscribeChannel
	// calls.
	//
	// Typed errors:
	//   ErrChannelNotDeclared  — channel name not in operator yaml.
	//                            Transports: NotFound.
	//   ErrChannelScopeInvalid — scope is not one of global/user.
	//                            Transports: InvalidArgument.
	//   ErrChannelCursorRegression — ack cursor is older than committed.
	//                            Transports: FailedPrecondition.
	PublishChannel(ctx context.Context, req ChannelPublishRequest) (ChannelPublishResult, error)
	SubscribeChannel(ctx context.Context, req ChannelSubscribeRequest) (ChannelSubscribeResult, error)
	PeekChannel(ctx context.Context, req ChannelPeekRequest) (ChannelPeekResult, error)
	AckChannel(ctx context.Context, req ChannelAckRequest) (ChannelAckResult, error)

	// AwaitChannels / BroadcastChannels (RFC S client twins) are the
	// fan-in / fan-out counterparts of the in-band Channel.await /
	// Channel.broadcast ops, exposed to wire callers so an external
	// orchestrator can join independent producers / ping N workers over
	// the SAME bus + store agents use. Operator-authed; Scope + ScopeID
	// apply to every channel in the set. AwaitChannels long-polls up to
	// WaitMS and is non-committing; BroadcastChannels is atomic at the
	// ACL pre-flight (one undeclared channel refuses the whole op).
	//
	// Typed errors: ErrChannelNotDeclared (NotFound), ErrChannelScopeInvalid
	// (InvalidArgument). A timeout is NOT an error — AwaitChannels returns
	// TimedOut:true with partials.
	AwaitChannels(ctx context.Context, req ChannelAwaitRequest) (ChannelAwaitResult, error)
	BroadcastChannels(ctx context.Context, req ChannelBroadcastRequest) (ChannelBroadcastResult, error)

	// Channel admin CRUD (v0.11.5): the runtime-declared substrate.
	// yaml-declared channels are static (immutable from the API);
	// runtime channels persist in the substrate `channels` table and
	// support Create / Update / Delete via the same Connector surface.
	//
	// Typed errors:
	//   ErrChannelYamlImmutable — name matches a yaml-declared channel;
	//                             the runtime CRUD layer refuses the
	//                             mutation. Transports: Conflict (409).
	//   ErrChannelAlreadyExists — Create called with a name that already
	//                             exists in the runtime table.
	//                             Transports: Conflict (409).
	//   ErrChannelNotFound       — Update/Delete called on a name that is
	//                             neither yaml-declared nor in the
	//                             runtime table. Transports: NotFound.
	CreateChannel(ctx context.Context, req ChannelCreateRequest) (ChannelDescriptor, error)
	UpdateChannel(ctx context.Context, name string, req ChannelUpdateRequest) (ChannelDescriptor, error)
	DeleteChannel(ctx context.Context, name string) error

	// PurgeChannel clears all buffered messages on a channel without
	// removing its definition or subscriber cursors. UNLIKE Create/
	// Update/Delete it is allowed on yaml-declared channels too —
	// draining the queue is not a definition mutation, and "clear a
	// yaml channel that filled with test traffic" was the F20 pain that
	// otherwise needed a raw DB delete. Returns ErrChannelNotFound when
	// the name is neither yaml-declared nor in the runtime table.
	PurgeChannel(ctx context.Context, name string) (ChannelPurgeResult, error)
}

// RunStateVisitor is the visitor callback for StreamUserRunStates.
// Return ErrStopStreaming to end the stream cleanly; any other
// non-nil error aborts the stream and propagates out of
// StreamUserRunStates.
type RunStateVisitor func(evt RunStateEvent) error

// RunEventVisitor is the visitor callback for StreamRunEvents (RFC AI) — one
// providers.Event per persisted run frame. Return ErrStopStreaming to end the
// tail cleanly; any other non-nil error aborts it and propagates out of
// StreamRunEvents.
type RunEventVisitor func(ev providers.Event) error

// InterruptionResolveRequest is the input to Connector.InterruptionResolve.
type InterruptionResolveRequest struct {
	RunID       string `json:"run_id"`
	InterruptID string `json:"interrupt_id"`
	Kind        string `json:"kind"`        // "question" in v0.8.16; future: "pause" / "wait_until" / "approval"
	Answer      string `json:"answer"`      // for kind=question
	ResolvedBy  string `json:"resolved_by"` // operator attribution; default "mcp" when surfaced via LoomCycle MCP
}

// InterruptionResolveResult is what the resolve op returns. The
// terminal status is always "resolved" on success; failure modes
// surface as Go errors (typed where useful).
type InterruptionResolveResult struct {
	InterruptID string `json:"interrupt_id"`
	Status      string `json:"status"`
	ResolvedAt  string `json:"resolved_at"` // RFC3339Nano
}
