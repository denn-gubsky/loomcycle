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
// v0.8.15 mock policy: PauseRuntime / ResumeRuntime / GetRuntimeState /
// CreateSnapshot / ListSnapshots / ExportSnapshot / RestoreSnapshot /
// DeleteSnapshot return placeholder responses with FeatureStatus="preview".
// Wire shapes are stable; real implementations land in v0.8.16+.
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
	Evaluation(ctx context.Context, input json.RawMessage) (ToolResult, error)
	Context(ctx context.Context, input json.RawMessage) (ToolResult, error)

	// --- Pause/Resume/Snapshot (MOCKED in v0.8.15) ---
	//
	// Wire shapes are stable; real implementations land in v0.8.16+
	// per doc-internal/rfcs/pause-resume-snapshot.md (RFC locked
	// 2026-05-12). Mock responses include FeatureStatus="preview" so
	// adapters can detect the stub state without surprise.

	PauseRuntime(ctx context.Context, timeoutMS int) (PauseResult, error)
	ResumeRuntime(ctx context.Context) (ResumeResult, error)
	GetRuntimeState(ctx context.Context) (RuntimeState, error)

	CreateSnapshot(ctx context.Context, req CreateSnapshotRequest) (SnapshotDescriptor, error)
	ListSnapshots(ctx context.Context) ([]SnapshotDescriptor, error)
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
}

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
