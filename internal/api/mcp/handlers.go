package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/runner"
	loommcp "github.com/denn-gubsky/loomcycle/internal/tools/mcp"
)

// toolHandler is the uniform signature every dispatch entry follows.
// Returns the MCP CallToolResult directly (not raw JSON) so the server
// can marshal it once.
type toolHandler func(ctx context.Context, env *handlerEnv, args json.RawMessage) (*loommcp.CallToolResult, error)

var handlersByName = map[string]toolHandler{
	// Run lifecycle
	"spawn_run":  handleSpawnRun,
	"cancel_run": handleCancelRun,
	"get_run":    handleGetRun,
	"list_runs":  handleListRuns,

	// Agent management
	"register_agent":   handleRegisterAgent,
	"unregister_agent": handleUnregisterAgent,
	"list_agents":      handleListAgents,

	// Builtin wrappers
	"memory": wrapBuiltin("memory", func(c connector.Connector, ctx context.Context, in json.RawMessage) (connector.ToolResult, error) {
		return c.Memory(ctx, in)
	}),
	"channel": wrapBuiltin("channel", func(c connector.Connector, ctx context.Context, in json.RawMessage) (connector.ToolResult, error) {
		return c.Channel(ctx, in)
	}),
	"agentdef": wrapBuiltin("agentdef", func(c connector.Connector, ctx context.Context, in json.RawMessage) (connector.ToolResult, error) {
		return c.AgentDef(ctx, in)
	}),
	"skilldef": wrapBuiltin("skilldef", func(c connector.Connector, ctx context.Context, in json.RawMessage) (connector.ToolResult, error) {
		return c.SkillDef(ctx, in)
	}),
	// v0.9.x dynamic MCP server registration. Operator-admin-only;
	// the LoomCycle MCP server is bearer-authed so external
	// orchestrators (Claude Code, n8n via the MCP Client Tool) can
	// register MCP servers at runtime through this meta-tool.
	"mcpserverdef": wrapBuiltin("mcpserverdef", func(c connector.Connector, ctx context.Context, in json.RawMessage) (connector.ToolResult, error) {
		return c.MCPServerDef(ctx, in)
	}),
	// v1.x RFC E scheduled-runs substrate. Same operator-admin-only
	// posture; lets external orchestrators (e.g. JobEmber's user-
	// signup pipeline) author per-user forks of yaml templates over
	// MCP without going through the HTTP admin endpoint.
	"scheduledef": wrapBuiltin("scheduledef", func(c connector.Connector, ctx context.Context, in json.RawMessage) (connector.ToolResult, error) {
		return c.ScheduleDef(ctx, in)
	}),
	"evaluation": wrapBuiltin("evaluation", func(c connector.Connector, ctx context.Context, in json.RawMessage) (connector.ToolResult, error) {
		return c.Evaluation(ctx, in)
	}),
	"context": wrapBuiltin("context", func(c connector.Connector, ctx context.Context, in json.RawMessage) (connector.ToolResult, error) {
		return c.Context(ctx, in)
	}),

	// Pause/Resume (v0.8.17 primitives; exposed via Connector in v0.8.18)
	"pause_runtime":     handlePauseRuntime,
	"resume_runtime":    handleResumeRuntime,
	"get_runtime_state": handleGetRuntimeState,

	// Snapshot (v0.8.17 primitives; exposed via Connector in v0.8.18)
	"create_snapshot":  handleCreateSnapshot,
	"list_snapshots":   handleListSnapshots,
	"get_snapshot":     handleGetSnapshot,
	"export_snapshot":  handleExportSnapshot,
	"restore_snapshot": handleRestoreSnapshot,
	"delete_snapshot":  handleDeleteSnapshot,

	// Interruption (v0.8.16)
	"interruption_resolve": handleInterruptionResolve,

	// Hook management (PR B of the hooks-connector series)
	"register_hook": handleRegisterHook,
	"list_hooks":    handleListHooks,
	"delete_hook":   handleDeleteHook,

	// v0.9.x n8n RFC Phase 0: channel listing + run-state streaming.
	"list_channels":          handleListChannels,
	"stream_user_run_states": handleStreamUserRunStates,

	// v0.9.x Channel CRUD — admin + per-user publish / subscribe /
	// peek / ack. Bearer-authed at the MCP server boundary; scope +
	// scope_id in the args select the cursor namespace. Same wire
	// semantics as the HTTP routes (single-round-trip long-poll for
	// subscribe; non-destructive peek; monotonic-cursor ack).
	"publish_channel":   handlePublishChannel,
	"subscribe_channel": handleSubscribeChannel,
	"peek_channel":      handlePeekChannel,
	"ack_channel":       handleAckChannel,
}

func toolHandlerByName(name string) (toolHandler, bool) {
	h, ok := handlersByName[name]
	return h, ok
}

// --- Run lifecycle handlers ---

// handleSpawnRun has two code paths:
//
//  1. Streaming (session opted into runEvents AND a Runner is wired):
//     drive runner.RunOnce directly with an OnEvent that emits
//     notifications/loomcycle/run_event for each provider event.
//     Capture the final text + usage + IDs in a closure for the
//     tool/call result.
//
//  2. Blocking (no opt-in, or no Runner): call Connector.SpawnRun
//     and return its result. No streaming.
//
// Both paths produce the same final tool/call response shape — the
// adapter on the orchestrator side can branch on the response if it
// cares about which path was taken.
func handleSpawnRun(ctx context.Context, env *handlerEnv, args json.RawMessage) (*loommcp.CallToolResult, error) {
	if env.connector == nil {
		return nil, fmt.Errorf("spawn_run: no connector wired")
	}
	var req connector.SpawnRunRequest
	if err := json.Unmarshal(args, &req); err != nil {
		return toolErr("invalid spawn_run arguments: " + err.Error()), nil
	}
	if req.Agent == "" && req.SessionID == "" {
		return toolErr("spawn_run: either agent or session_id must be supplied"), nil
	}
	if errMsg, ok := connector.ValidateUserCredentialsMap(req.UserCredentials); !ok {
		return toolErr("spawn_run: " + errMsg), nil
	}
	if errMsg, ok := connector.ValidateParentContext(req.ParentContext); !ok {
		return toolErr("spawn_run: " + errMsg), nil
	}

	useStreaming := env.session != nil && env.session.RunEventsEnabled() && env.runner != nil
	var result connector.SpawnRunResult
	var err error
	if useStreaming {
		result, err = spawnRunStreaming(ctx, env, req)
	} else {
		result, err = env.connector.SpawnRun(ctx, req)
	}
	if err != nil {
		return toolErr("spawn_run: " + err.Error()), nil
	}
	return toolResultJSON(result), nil
}

// spawnRunStreaming drives the runner directly and emits per-event
// notifications. The final connector.SpawnRunResult is assembled
// from the captured callback state — identical shape to
// Connector.SpawnRun, so the tool/call response matches across both
// code paths.
func spawnRunStreaming(ctx context.Context, env *handlerEnv, req connector.SpawnRunRequest) (connector.SpawnRunResult, error) {
	in := runner.RunInput{
		Agent:           req.Agent,
		SessionID:       req.SessionID,
		TenantID:        req.TenantID,
		Segments:        req.Segments,
		AllowedTools:    req.AllowedTools,
		AllowedHosts:    req.AllowedHosts,
		WebSearchFilter: req.WebSearchFilter,
		UserID:          req.UserID,
		AgentID:         req.AgentID,
		UserTier:        req.UserTier,
		UserBearer:      req.UserBearer,
		UserCredentials: req.UserCredentials, // v1.x RFC F per-tool named credentials
		ParentContext:   req.ParentContext,   // v0.12.x opaque tracking lineage
	}

	var (
		regAgentID, regRunID, regSessionID string
		finalText                          string
		finalUsage                         *providers.Usage
		finalStopReason                    string
	)
	cb := runner.RunCallbacks{
		OnRegistered: func(agentID, runID, sessionID, _ string) {
			regAgentID, regRunID, regSessionID = agentID, runID, sessionID
		},
		OnEvent: func(ev providers.Event) {
			// Emit the notification BEFORE accumulating state — keeps
			// the wire order matching event timestamps.
			env.notify("notifications/loomcycle/run_event", runEventPayload{
				RunID:   regRunID,
				AgentID: regAgentID,
				Event:   ev,
			})
			switch ev.Type {
			case providers.EventText:
				finalText += ev.Text
			case providers.EventUsage:
				if ev.Usage != nil {
					u := *ev.Usage
					finalUsage = &u
				}
			case providers.EventDone:
				finalStopReason = ev.StopReason
				if ev.Usage != nil && finalUsage == nil {
					u := *ev.Usage
					finalUsage = &u
				}
			}
		},
	}

	runErr := env.runner.RunOnce(ctx, in, cb)

	result := connector.SpawnRunResult{
		AgentID:    regAgentID,
		RunID:      regRunID,
		SessionID:  regSessionID,
		Status:     "completed",
		StopReason: finalStopReason,
		FinalText:  finalText,
		Usage:      finalUsage,
	}
	switch {
	case runErr != nil && errors.Is(runErr, context.Canceled):
		result.Status = "cancelled"
		result.Error = runErr.Error()
	case runErr != nil:
		result.Status = "failed"
		result.Error = runErr.Error()
	}
	return result, nil
}

func handleCancelRun(ctx context.Context, env *handlerEnv, args json.RawMessage) (*loommcp.CallToolResult, error) {
	var p struct {
		AgentID string `json:"agent_id"`
		Reason  string `json:"reason"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return toolErr("invalid cancel_run arguments: " + err.Error()), nil
	}
	res, err := env.connector.CancelRun(ctx, p.AgentID, p.Reason)
	if err != nil {
		return toolErr("cancel_run: " + err.Error()), nil
	}
	return toolResultJSON(res), nil
}

func handleGetRun(ctx context.Context, env *handlerEnv, args json.RawMessage) (*loommcp.CallToolResult, error) {
	var p struct {
		AgentID string `json:"agent_id"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return toolErr("invalid get_run arguments: " + err.Error()), nil
	}
	res, err := env.connector.GetRun(ctx, p.AgentID)
	if err != nil {
		return toolErr("get_run: " + err.Error()), nil
	}
	return toolResultJSON(res), nil
}

func handleListRuns(ctx context.Context, env *handlerEnv, args json.RawMessage) (*loommcp.CallToolResult, error) {
	var f connector.ListRunsFilter
	if err := json.Unmarshal(args, &f); err != nil {
		return toolErr("invalid list_runs arguments: " + err.Error()), nil
	}
	res, err := env.connector.ListRuns(ctx, f)
	if err != nil {
		return toolErr("list_runs: " + err.Error()), nil
	}
	return toolResultJSON(struct {
		Runs []connector.Run `json:"runs"`
	}{Runs: res}), nil
}

// --- Agent management handlers ---

func handleRegisterAgent(ctx context.Context, env *handlerEnv, args json.RawMessage) (*loommcp.CallToolResult, error) {
	var req connector.RegisterAgentRequest
	if err := json.Unmarshal(args, &req); err != nil {
		return toolErr("invalid register_agent arguments: " + err.Error()), nil
	}
	res, err := env.connector.RegisterAgent(ctx, req)
	if err != nil {
		return toolErr("register_agent: " + err.Error()), nil
	}
	return toolResultJSON(res), nil
}

func handleUnregisterAgent(ctx context.Context, env *handlerEnv, args json.RawMessage) (*loommcp.CallToolResult, error) {
	var p struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return toolErr("invalid unregister_agent arguments: " + err.Error()), nil
	}
	if err := env.connector.UnregisterAgent(ctx, p.Name); err != nil {
		return toolErr("unregister_agent: " + err.Error()), nil
	}
	return toolResultJSON(map[string]any{"unregistered": true, "name": p.Name}), nil
}

func handleListAgents(ctx context.Context, env *handlerEnv, args json.RawMessage) (*loommcp.CallToolResult, error) {
	var p struct {
		IncludeDynamic *bool `json:"include_dynamic"`
	}
	_ = json.Unmarshal(args, &p) // best-effort; defaults to true below
	includeDyn := true
	if p.IncludeDynamic != nil {
		includeDyn = *p.IncludeDynamic
	}
	agents, err := env.connector.ListAgents(ctx, includeDyn)
	if err != nil {
		return toolErr("list_agents: " + err.Error()), nil
	}
	return toolResultJSON(struct {
		Agents []connector.AgentDescriptor `json:"agents"`
	}{Agents: agents}), nil
}

// --- Builtin wrappers (memory/channel/agentdef/evaluation/context) ---

// wrapBuiltin returns a handler that dispatches to one Connector
// builtin method. The connector returns ToolResult{Text, IsError};
// we map that 1:1 to the MCP tool/call response (content[].text +
// isError).
//
// CRITICAL: enrich ctx with operatorCtx() BEFORE calling the Connector.
// The underlying tools (Memory/Channel/AgentDef/Evaluation/Context)
// gate every op on per-agent policy values from ctx. Without enrichment,
// MCP-direct callers see "no scope configured" errors on every call —
// the policies are missing from a bare bgCtx. See internal/api/mcp/
// context.go for the policy synthesis rationale.
func wrapBuiltin(toolName string, call func(connector.Connector, context.Context, json.RawMessage) (connector.ToolResult, error)) toolHandler {
	return func(ctx context.Context, env *handlerEnv, args json.RawMessage) (*loommcp.CallToolResult, error) {
		if env.connector == nil {
			return nil, fmt.Errorf("%s: no connector wired", toolName)
		}
		res, err := call(env.connector, operatorCtx(ctx), args)
		if err != nil {
			return toolErr(toolName + ": " + err.Error()), nil
		}
		return &loommcp.CallToolResult{
			Content: []loommcp.ContentBlock{{Type: "text", Text: res.Text}},
			IsError: res.IsError,
		}, nil
	}
}

// --- Pause/Resume/Snapshot handlers (PREVIEW mocks in v0.8.15) ---

func handlePauseRuntime(ctx context.Context, env *handlerEnv, args json.RawMessage) (*loommcp.CallToolResult, error) {
	var p struct {
		TimeoutMS int `json:"timeout_ms"`
	}
	_ = json.Unmarshal(args, &p)
	res, err := env.connector.PauseRuntime(ctx, p.TimeoutMS)
	if err != nil {
		return toolErr("pause_runtime: " + err.Error()), nil
	}
	return toolResultJSON(res), nil
}

func handleResumeRuntime(ctx context.Context, env *handlerEnv, _ json.RawMessage) (*loommcp.CallToolResult, error) {
	res, err := env.connector.ResumeRuntime(ctx)
	if err != nil {
		return toolErr("resume_runtime: " + err.Error()), nil
	}
	return toolResultJSON(res), nil
}

func handleGetRuntimeState(ctx context.Context, env *handlerEnv, _ json.RawMessage) (*loommcp.CallToolResult, error) {
	res, err := env.connector.GetRuntimeState(ctx)
	if err != nil {
		return toolErr("get_runtime_state: " + err.Error()), nil
	}
	return toolResultJSON(res), nil
}

func handleCreateSnapshot(ctx context.Context, env *handlerEnv, args json.RawMessage) (*loommcp.CallToolResult, error) {
	var req connector.CreateSnapshotRequest
	_ = json.Unmarshal(args, &req)
	res, err := env.connector.CreateSnapshot(ctx, req)
	if err != nil {
		return toolErr("create_snapshot: " + err.Error()), nil
	}
	return toolResultJSON(res), nil
}

func handleListSnapshots(ctx context.Context, env *handlerEnv, _ json.RawMessage) (*loommcp.CallToolResult, error) {
	res, err := env.connector.ListSnapshots(ctx)
	if err != nil {
		return toolErr("list_snapshots: " + err.Error()), nil
	}
	return toolResultJSON(struct {
		Snapshots []connector.SnapshotDescriptor `json:"snapshots"`
	}{Snapshots: res}), nil
}

func handleGetSnapshot(ctx context.Context, env *handlerEnv, args json.RawMessage) (*loommcp.CallToolResult, error) {
	var p struct {
		SnapshotID string `json:"snapshot_id"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return toolErr("invalid get_snapshot arguments: " + err.Error()), nil
	}
	res, err := env.connector.GetSnapshot(ctx, p.SnapshotID)
	if err != nil {
		return toolErr("get_snapshot: " + err.Error()), nil
	}
	return toolResultJSON(res), nil
}

func handleExportSnapshot(ctx context.Context, env *handlerEnv, args json.RawMessage) (*loommcp.CallToolResult, error) {
	var p struct {
		SnapshotID string `json:"snapshot_id"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return toolErr("invalid export_snapshot arguments: " + err.Error()), nil
	}
	res, err := env.connector.ExportSnapshot(ctx, p.SnapshotID)
	if err != nil {
		return toolErr("export_snapshot: " + err.Error()), nil
	}
	return toolResultJSON(res), nil
}

func handleRestoreSnapshot(ctx context.Context, env *handlerEnv, args json.RawMessage) (*loommcp.CallToolResult, error) {
	var req connector.RestoreSnapshotRequest
	if err := json.Unmarshal(args, &req); err != nil {
		return toolErr("invalid restore_snapshot arguments: " + err.Error()), nil
	}
	res, err := env.connector.RestoreSnapshot(ctx, req)
	if err != nil {
		return toolErr("restore_snapshot: " + err.Error()), nil
	}
	return toolResultJSON(res), nil
}

func handleDeleteSnapshot(ctx context.Context, env *handlerEnv, args json.RawMessage) (*loommcp.CallToolResult, error) {
	var p struct {
		SnapshotID string `json:"snapshot_id"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return toolErr("invalid delete_snapshot arguments: " + err.Error()), nil
	}
	if err := env.connector.DeleteSnapshot(ctx, p.SnapshotID); err != nil {
		return toolErr("delete_snapshot: " + err.Error()), nil
	}
	return toolResultJSON(map[string]any{"deleted": true, "snapshot_id": p.SnapshotID}), nil
}

// --- Helpers ---

// runEventPayload is the wire shape for notifications/loomcycle/run_event.
type runEventPayload struct {
	RunID   string          `json:"run_id"`
	AgentID string          `json:"agent_id"`
	Event   providers.Event `json:"event"`
}

// toolResultJSON marshals v as JSON and wraps it in a non-error MCP
// CallToolResult with a single text content block.
func toolResultJSON(v any) *loommcp.CallToolResult {
	raw, err := json.Marshal(v)
	if err != nil {
		// Internal failure — surface as tool error rather than panic.
		return toolErr("marshal result: " + err.Error())
	}
	return &loommcp.CallToolResult{
		Content: []loommcp.ContentBlock{{Type: "text", Text: string(raw)}},
	}
}

// toolErr returns an MCP tool/call response with isError=true and a
// single text content block carrying the error message. Distinct from
// the JSON-RPC -32603 error path: tool errors are a normal tool
// outcome the orchestrator surfaces to the user; -32603 is an
// internal-server-error path.
func toolErr(msg string) *loommcp.CallToolResult {
	return &loommcp.CallToolResult{
		Content: []loommcp.ContentBlock{{Type: "text", Text: msg}},
		IsError: true,
	}
}

// --- Interruption (v0.8.16) ---

// handleInterruptionResolve is the 21st LoomCycle MCP meta-tool. Lets
// an external orchestrator (Claude Code etc.) resolve a pending
// interrupt without speaking HTTP to loomcycle directly. Wraps
// connector.InterruptionResolve.
func handleInterruptionResolve(ctx context.Context, env *handlerEnv, args json.RawMessage) (*loommcp.CallToolResult, error) {
	if env.connector == nil {
		return nil, fmt.Errorf("interruption_resolve: no connector wired")
	}
	var req connector.InterruptionResolveRequest
	if err := json.Unmarshal(args, &req); err != nil {
		return toolErr("invalid interruption_resolve arguments: " + err.Error()), nil
	}
	if req.RunID == "" || req.InterruptID == "" {
		return toolErr("interruption_resolve: run_id and interrupt_id are required"), nil
	}
	res, err := env.connector.InterruptionResolve(ctx, req)
	if err != nil {
		return toolErr("interruption_resolve: " + err.Error()), nil
	}
	return toolResultJSON(res), nil
}

// --- Hook management handlers (PR B of the hooks-connector series) ---
//
// Three-line shape mirrors handleRegisterAgent: unmarshal arguments
// into the connector request type, dispatch through env.connector,
// surface success as toolResultJSON or any error via toolErr. MCP
// doesn't have typed-error subclasses — every failure is a tool_result
// with isError=true + a descriptive text message.

func handleRegisterHook(ctx context.Context, env *handlerEnv, args json.RawMessage) (*loommcp.CallToolResult, error) {
	if env.connector == nil {
		return nil, fmt.Errorf("register_hook: no connector wired")
	}
	var req connector.RegisterHookRequest
	if err := json.Unmarshal(args, &req); err != nil {
		return toolErr("invalid register_hook arguments: " + err.Error()), nil
	}
	res, err := env.connector.RegisterHook(ctx, req)
	if err != nil {
		return toolErr("register_hook: " + err.Error()), nil
	}
	return toolResultJSON(res), nil
}

func handleListHooks(ctx context.Context, env *handlerEnv, _ json.RawMessage) (*loommcp.CallToolResult, error) {
	if env.connector == nil {
		return nil, fmt.Errorf("list_hooks: no connector wired")
	}
	res, err := env.connector.ListHooks(ctx)
	if err != nil {
		return toolErr("list_hooks: " + err.Error()), nil
	}
	return toolResultJSON(res), nil
}

// handleListChannels — v0.9.x n8n RFC Phase 0. Dispatches through
// Connector.ListChannels and returns the result as a JSON tool result.
func handleListChannels(ctx context.Context, env *handlerEnv, _ json.RawMessage) (*loommcp.CallToolResult, error) {
	if env.connector == nil {
		return nil, fmt.Errorf("list_channels: no connector wired")
	}
	resp, err := env.connector.ListChannels(ctx)
	if err != nil {
		return toolErr("list_channels: " + err.Error()), nil
	}
	return toolResultJSON(resp), nil
}

// streamUserRunStatesArgs is the input to the stream_user_run_states
// meta-tool. Mirrors connector.StreamUserRunStatesRequest plus two
// extra fields that bound the blocking-aggregate code path (the
// streaming code path uses ctx done instead).
type streamUserRunStatesArgs struct {
	UserID    string   `json:"user_id"`
	Statuses  []string `json:"statuses,omitempty"`
	Agent     string   `json:"agent,omitempty"`
	MaxEvents int      `json:"max_events,omitempty"`
	TimeoutMS int      `json:"timeout_ms,omitempty"`
}

// handleStreamUserRunStates has two code paths analogous to spawn_run:
//
//  1. Streaming (session opted into runEvents): each matching event
//     gets emitted as notifications/loomcycle/run_state; the tool
//     call returns when ctx fires or MaxEvents hit, with the count
//     of forwarded events.
//
//  2. Blocking (no opt-in): collects matching events into a slice
//     until MaxEvents or TimeoutMS, then returns the slice as the
//     tool result.
//
// Either way the final response is { "events": [...], "count": N }
// so adapters can branch on whether they want streaming or polled
// behaviour by setting the capability flag.
func handleStreamUserRunStates(ctx context.Context, env *handlerEnv, args json.RawMessage) (*loommcp.CallToolResult, error) {
	if env.connector == nil {
		return nil, fmt.Errorf("stream_user_run_states: no connector wired")
	}
	var a streamUserRunStatesArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return toolErr("invalid stream_user_run_states arguments: " + err.Error()), nil
	}
	if a.UserID == "" {
		return toolErr("stream_user_run_states: user_id is required"), nil
	}
	if a.MaxEvents <= 0 {
		a.MaxEvents = 16
	}
	if a.TimeoutMS <= 0 {
		a.TimeoutMS = 30000
	}

	useStreaming := env.session != nil && env.session.RunEventsEnabled()
	collected := make([]connector.RunStateEvent, 0, a.MaxEvents)
	var count int

	streamCtx, cancel := context.WithTimeout(ctx, time.Duration(a.TimeoutMS)*time.Millisecond)
	defer cancel()

	visit := func(evt connector.RunStateEvent) error {
		count++
		if useStreaming {
			env.notify("notifications/loomcycle/run_state", evt)
		} else {
			collected = append(collected, evt)
		}
		if count >= a.MaxEvents {
			return connector.ErrStopStreaming
		}
		return nil
	}

	err := env.connector.StreamUserRunStates(streamCtx, connector.StreamUserRunStatesRequest{
		UserID:   a.UserID,
		Statuses: a.Statuses,
		Agent:    a.Agent,
	}, visit)
	if err != nil {
		return toolErr("stream_user_run_states: " + err.Error()), nil
	}
	return toolResultJSON(struct {
		Events []connector.RunStateEvent `json:"events"`
		Count  int                       `json:"count"`
	}{Events: collected, Count: count}), nil
}

func handleDeleteHook(ctx context.Context, env *handlerEnv, args json.RawMessage) (*loommcp.CallToolResult, error) {
	if env.connector == nil {
		return nil, fmt.Errorf("delete_hook: no connector wired")
	}
	var p struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return toolErr("invalid delete_hook arguments: " + err.Error()), nil
	}
	if p.ID == "" {
		return toolErr("delete_hook: id required"), nil
	}
	if err := env.connector.DeleteHook(ctx, p.ID); err != nil {
		return toolErr("delete_hook: " + err.Error()), nil
	}
	return toolResultJSON(map[string]any{"deleted": p.ID}), nil
}

// --- v0.9.x Channel CRUD handlers ---
//
// Each takes the Connector request shape verbatim — scope + scope_id
// + channel + op-specific fields. No URL-path-derived scope_id like
// the HTTP per-user route family; MCP callers supply both scope and
// scope_id directly. The MCP server is bearer-authed (LoomCycle MCP
// server's stdio transport runs inside the operator's trust boundary)
// so the cross-channel + cross-user reach is equivalent to the
// admin HTTP routes.

func handlePublishChannel(ctx context.Context, env *handlerEnv, args json.RawMessage) (*loommcp.CallToolResult, error) {
	if env.connector == nil {
		return nil, fmt.Errorf("publish_channel: no connector wired")
	}
	var req connector.ChannelPublishRequest
	if err := json.Unmarshal(args, &req); err != nil {
		return toolErr("invalid publish_channel arguments: " + err.Error()), nil
	}
	res, err := env.connector.PublishChannel(ctx, req)
	if err != nil {
		return toolErr("publish_channel: " + err.Error()), nil
	}
	return toolResultJSON(res), nil
}

func handleSubscribeChannel(ctx context.Context, env *handlerEnv, args json.RawMessage) (*loommcp.CallToolResult, error) {
	if env.connector == nil {
		return nil, fmt.Errorf("subscribe_channel: no connector wired")
	}
	var req connector.ChannelSubscribeRequest
	if err := json.Unmarshal(args, &req); err != nil {
		return toolErr("invalid subscribe_channel arguments: " + err.Error()), nil
	}
	res, err := env.connector.SubscribeChannel(ctx, req)
	if err != nil {
		return toolErr("subscribe_channel: " + err.Error()), nil
	}
	return toolResultJSON(res), nil
}

func handlePeekChannel(ctx context.Context, env *handlerEnv, args json.RawMessage) (*loommcp.CallToolResult, error) {
	if env.connector == nil {
		return nil, fmt.Errorf("peek_channel: no connector wired")
	}
	var req connector.ChannelPeekRequest
	if err := json.Unmarshal(args, &req); err != nil {
		return toolErr("invalid peek_channel arguments: " + err.Error()), nil
	}
	res, err := env.connector.PeekChannel(ctx, req)
	if err != nil {
		return toolErr("peek_channel: " + err.Error()), nil
	}
	return toolResultJSON(res), nil
}

func handleAckChannel(ctx context.Context, env *handlerEnv, args json.RawMessage) (*loommcp.CallToolResult, error) {
	if env.connector == nil {
		return nil, fmt.Errorf("ack_channel: no connector wired")
	}
	var req connector.ChannelAckRequest
	if err := json.Unmarshal(args, &req); err != nil {
		return toolErr("invalid ack_channel arguments: " + err.Error()), nil
	}
	res, err := env.connector.AckChannel(ctx, req)
	if err != nil {
		return toolErr("ack_channel: " + err.Error()), nil
	}
	return toolResultJSON(res), nil
}
