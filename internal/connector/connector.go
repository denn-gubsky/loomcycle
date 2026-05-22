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

	// CancelRun mirrors POST /v1/agents/{agent_id}/cancel. Cascades
	// to sub-agent runs spawned by this run. Idempotent.
	CancelRun(ctx context.Context, agentID, reason string) (CancelRunResult, error)

	// GetRun returns the latest status snapshot for a tracked
	// agent_id. Mirrors GET /v1/agents/{agent_id}.
	GetRun(ctx context.Context, agentID string) (Run, error)

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
}

// RunStateVisitor is the visitor callback for StreamUserRunStates.
// Return ErrStopStreaming to end the stream cleanly; any other
// non-nil error aborts the stream and propagates out of
// StreamUserRunStates.
type RunStateVisitor func(evt RunStateEvent) error

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
