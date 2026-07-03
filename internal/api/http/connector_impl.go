// Package http — Connector interface implementation.
//
// The HTTP Server is the canonical connector.Connector implementation.
// Other wire transports (MCP, gRPC, future CLI) CONSUME the Connector
// rather than re-implementing business logic.
//
// Methods in this file are organised by Connector method group:
//   1. Run lifecycle: SpawnRun, CancelRun, GetRun, ListRuns
//   2. Agent management: RegisterAgent, UnregisterAgent, ListAgents
//   3. Builtin tool wrappers: Memory, Channel, AgentDef, SkillDef, Evaluation, Context
//   4. Pause/Resume/Snapshot: real impls (v0.8.18; wire shapes locked in v0.8.15)
//   5. Interruption (v0.8.16)

package http

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/channels"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/pause"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/runner"
	"github.com/denn-gubsky/loomcycle/internal/snapshot"
	"github.com/denn-gubsky/loomcycle/internal/snapshot/migrations"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// Compile-time assertion that *Server satisfies connector.Connector.
// Adding new methods to the Connector interface forces a compile
// failure here until they're implemented — exactly the signal we want.
var _ connector.Connector = (*Server)(nil)

// --- 1. Run lifecycle ---

// SpawnRun translates a connector.SpawnRunRequest into a runner.RunInput
// and drives RunOnce. The blocking shape matches the Connector contract;
// transports that want streaming (MCP notifications, gRPC stream) hold a
// runner.Runner field separately and use it directly for that path.
func (s *Server) SpawnRun(ctx context.Context, req connector.SpawnRunRequest) (connector.SpawnRunResult, error) {
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
		UserCredentials: req.UserCredentials, // v1.x RFC F: per-tool named credentials
		ParentContext:   req.ParentContext,   // v0.12.x: opaque tracking lineage
		Metadata:        req.Metadata,        // non-secret trusted agent metadata
		Sampling:        req.Sampling,        // per-run LLM sampling override
		Compaction:      req.Compaction,      // per-run context-compaction override
	}

	// Capture: the OnRegistered callback gives us the resolved IDs;
	// OnEvent accumulates text deltas + the final usage/stop_reason.
	var (
		regAgentID, regRunID, regSessionID string
		finalText                          string
		finalUsage                         *providers.Usage
		finalStopReason                    string
		lastErrorMsg                       string
	)
	cb := runner.RunCallbacks{
		OnRegistered: func(agentID, runID, sessionID, _ string) {
			regAgentID, regRunID, regSessionID = agentID, runID, sessionID
		},
		OnEvent: func(ev providers.Event) {
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
			case providers.EventError:
				if ev.Error != "" {
					lastErrorMsg = ev.Error
				}
			}
		},
	}

	runErr := s.RunOnce(ctx, in, cb)

	result := connector.SpawnRunResult{
		AgentID:       regAgentID,
		RunID:         regRunID,
		SessionID:     regSessionID,
		Status:        string(store.RunCompleted),
		StopReason:    finalStopReason,
		FinalText:     finalText,
		Usage:         finalUsage,
		ParentContext: req.ParentContext, // v0.12.x: echo the lineage back to the caller
	}
	switch {
	case runErr != nil && errors.Is(runErr, context.Canceled):
		result.Status = string(store.RunCancelled)
		result.Error = runErr.Error()
	case runErr != nil:
		result.Status = string(store.RunFailed)
		result.Error = runErr.Error()
	case lastErrorMsg != "":
		// Loop emitted an error event but didn't return a Go error
		// (rare — current loop returns errors via the err path; this
		// is belt-and-suspenders for any future event-only error path).
		result.Status = string(store.RunFailed)
		result.Error = lastErrorMsg
	}
	return result, nil
}

// SpawnRunBatch implements the RFC Y external fan-out (mode "join"): spawn every
// child concurrently via SpawnRun and join them. Each child flows through the
// SAME per-user admission gate as a lone SpawnRun (RunOnce → sem.AcquireForUser),
// so there is deliberately NO wrapper semaphore here — admission back-pressure
// and per-user fairness are already enforced one level down, and adding a second
// gate would only double-count. The batch caller's ctx (carrying the
// authoritative principal/tenant from the auth middleware) is passed to every
// child, so children inherit the caller's tenant via the normal WithRunIdentity
// path — no manual tenant threading. Per-child failures are captured in that
// child's SpawnRunResult; the batch itself errors only on a malformed request
// (over-cap / unsupported mode).
func (s *Server) SpawnRunBatch(ctx context.Context, req connector.BatchSpawnRequest) (connector.BatchSpawnResult, error) {
	n := len(req.Spawns)
	if n == 0 {
		return connector.BatchSpawnResult{}, fmt.Errorf("spawn_runs: no spawns supplied")
	}
	if n > connector.MaxBatchSpawns {
		return connector.BatchSpawnResult{}, fmt.Errorf("spawn_runs: %d spawns exceeds the per-batch cap of %d", n, connector.MaxBatchSpawns)
	}
	mode := req.Mode
	if mode == "" {
		mode = "join"
	}
	if mode != "join" {
		// "detach" (return async run handles to poll/stream) requires RFC P's
		// bounded/async spawn; reject explicitly until it ships rather than
		// silently degrading to a blocking join.
		return connector.BatchSpawnResult{}, fmt.Errorf("spawn_runs: mode %q not supported (only %q; %q awaits RFC P async run handles)", mode, "join", "detach")
	}

	// Optional batch-level join deadline. A child still running when it fires
	// has its RunOnce ctx cancelled (derived from this ctx) and is reported
	// with a cancelled status in-envelope — the batch still returns the
	// finished children's results.
	if req.TimeoutMS > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(req.TimeoutMS)*time.Millisecond)
		defer cancel()
	}

	results := make([]connector.SpawnRunResult, n)
	var wg sync.WaitGroup
	wg.Add(n)
	for i := range req.Spawns {
		go func(i int) {
			defer wg.Done()
			spawn := req.Spawns[i]
			// Batch children are always fresh runs — a per-spawn session_id
			// would continue an existing session, which the fan-out shape never
			// intends. ParentContext is the caller's own opaque lineage (cost
			// attribution); preserve it verbatim — the caller groups a batch by
			// setting a shared root_agent_run_id across its spawns.
			spawn.SessionID = ""
			res, err := s.SpawnRun(ctx, spawn)
			if err != nil {
				// SpawnRun captures run failures in-result and returns a nil
				// error; a non-nil error here is an unexpected dispatch failure.
				// Surface it in THIS child's envelope, never fail the batch.
				res.Status = string(store.RunFailed)
				if res.Error == "" {
					res.Error = err.Error()
				}
			}
			results[i] = res
		}(i)
	}
	wg.Wait()

	return connector.BatchSpawnResult{Results: results, Spawned: n}, nil
}

// CancelRun looks up the run by agent_id and triggers cancellation
// via the in-memory cancel registry. The registry's Cancel handles
// the BFS cascade to sub-agents internally.
func (s *Server) CancelRun(ctx context.Context, agentID, reason string) (connector.CancelRunResult, error) {
	if agentID == "" {
		return connector.CancelRunResult{}, fmt.Errorf("agent_id required")
	}
	// Tenant ownership gate (RFC L/N): this method backs gRPC CancelAgent + the
	// MCP cancel_run tool, both of which dispatch with a principal-bearing ctx.
	// A cancel keyed only by agent_id must not reach another tenant's run (ids
	// are not secret; cluster cancel broadcasts). tenantStore folds a
	// cross-tenant/missing run into an opaque ErrNotFound; open/legacy/admin see
	// all tenants so behaviour is unchanged for them.
	if s.store != nil {
		if _, err := s.tenantStore(ctx).GetRunByAgentID(ctx, agentID); err != nil {
			return connector.CancelRunResult{}, err
		}
	}
	res, ok := s.cancelReg.Cancel(agentID, reason)
	if !ok {
		// Run may already have completed. Check the store to
		// distinguish "never existed" from "already ended".
		if s.store != nil {
			if _, err := s.store.GetRunByAgentID(ctx, agentID); err == nil {
				return connector.CancelRunResult{AlreadyEnded: true}, nil
			}
		}
		return connector.CancelRunResult{}, &store.ErrNotFound{Kind: "run", ID: agentID}
	}
	return connector.CancelRunResult{
		Cancelled:    res.Cancelled,
		CascadeCount: len(res.Cascaded),
	}, nil
}

// GetRun returns a status snapshot for one tracked agent_id. Maps the
// store's persistence-layer Run shape onto the connector's wire shape.
func (s *Server) GetRun(ctx context.Context, agentID string) (connector.Run, error) {
	if s.store == nil {
		return connector.Run{}, fmt.Errorf("get_run requires persistence (no Store configured)")
	}
	r, err := s.store.GetRunByAgentID(ctx, agentID)
	if err != nil {
		return connector.Run{}, err
	}
	return storeRunToConnector(r), nil
}

// ListRuns enumerates runs. Today only the UserID filter has an
// optimised store path (ListActiveRunsByUser); other combinations
// fall through to a broader query that the storetest contract suite
// already exercises.
func (s *Server) ListRuns(ctx context.Context, filter connector.ListRunsFilter) ([]connector.Run, error) {
	if s.store == nil {
		return nil, fmt.Errorf("list_runs requires persistence (no Store configured)")
	}
	limit := filter.Limit
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	var rows []store.Run
	if filter.UserID != "" {
		rs, err := s.store.ListActiveRunsByUser(ctx, filter.UserID, store.RunStatus(filter.Status))
		if err != nil {
			return nil, err
		}
		rows = rs
	} else {
		// No optimised non-user query exists today; fall through to a
		// scan via storetest semantics. For v0.8.15 we require a UserID
		// filter; callers omitting it get an explicit error so they
		// don't accidentally walk the entire runs table.
		return nil, fmt.Errorf("list_runs without user_id is not supported in v0.8.15; supply user_id in filter")
	}
	out := make([]connector.Run, 0, len(rows))
	for i, r := range rows {
		if i >= limit {
			break
		}
		out = append(out, storeRunToConnector(r))
	}
	return out, nil
}

func storeRunToConnector(r store.Run) connector.Run {
	usage := &providers.Usage{
		InputTokens:         r.InputTokens,
		OutputTokens:        r.OutputTokens,
		CacheCreationTokens: r.CacheCreationTokens,
		CacheReadTokens:     r.CacheReadTokens,
		Model:               r.Model,
	}
	if usage.InputTokens == 0 && usage.OutputTokens == 0 && usage.Model == "" {
		usage = nil
	}
	out := connector.Run{
		AgentID:       r.AgentID,
		RunID:         r.ID,
		SessionID:     r.SessionID,
		UserID:        r.UserID,
		Agent:         r.Agent,
		ParentAgentID: r.ParentAgentID,
		Status:        string(r.Status),
		StartedAt:     r.StartedAt,
		StopReason:    r.StopReason,
		Usage:         usage,
		Error:         r.ErrorMsg,
	}
	if !r.CompletedAt.IsZero() {
		ct := r.CompletedAt
		out.CompletedAt = &ct
	}
	return out
}

// --- 2. Agent management ---

// RegisterAgent persists a dynamic agent in store.dynamic_agents.
// Privileged tools (Bash/Write/Edit) are stripped unless the operator
// has set LOOMCYCLE_MCP_ALLOW_PRIVILEGED_TOOLS=1. Collisions with
// static agents (from yaml / discovery) are rejected.
func (s *Server) RegisterAgent(ctx context.Context, req connector.RegisterAgentRequest) (connector.AgentDescriptor, error) {
	if !validDynamicAgentName(req.Name) {
		return connector.AgentDescriptor{}, fmt.Errorf("invalid agent name %q: must match [A-Za-z0-9_-]{1,64}", req.Name)
	}
	if req.SystemPrompt == "" {
		return connector.AgentDescriptor{}, fmt.Errorf("system_prompt required")
	}
	if len(req.SystemPrompt) > 64*1024 {
		return connector.AgentDescriptor{}, fmt.Errorf("system_prompt exceeds 64KB cap")
	}
	if len(req.AllowedTools) == 0 {
		return connector.AgentDescriptor{}, fmt.Errorf("allowed_tools required (default-deny model)")
	}
	if _, collides := s.cfg.Agents[req.Name]; collides {
		return connector.AgentDescriptor{}, fmt.Errorf("agent %q is statically defined in yaml; cannot register over it", req.Name)
	}
	if s.store == nil {
		return connector.AgentDescriptor{}, fmt.Errorf("register_agent requires persistence (no Store configured)")
	}

	allowedTools := stripPrivilegedTools(req.AllowedTools, s.cfg.Env.MCPAllowPrivilegedTools)

	def := config.AgentDef{
		SystemPrompt:     req.SystemPrompt,
		AllowedTools:     allowedTools,
		Tier:             req.Tier,
		Provider:         req.Provider,
		Model:            req.Model,
		Effort:           req.Effort,
		MaxTokens:        req.MaxTokens,
		MemoryScopes:     req.MemoryScopes,
		MaxIterations:    req.MaxIterations,
		Channels:         req.Channels,
		EvaluationScopes: req.EvaluationScopes,
		Interruption:     req.Interruption,
	}
	defJSON, err := json.Marshal(def)
	if err != nil {
		return connector.AgentDescriptor{}, fmt.Errorf("marshal agent def: %w", err)
	}

	createdAt := time.Now().UTC()
	expiresAt := computeExpiresAt(createdAt, req.TTLSeconds, s.cfg.Env.DynamicAgentDefaultTTLSeconds)

	row := store.DynamicAgent{
		Name:        req.Name,
		Definition:  defJSON,
		CreatedAt:   createdAt,
		ExpiresAt:   expiresAt,
		Description: req.Description,
		// RFC N: stamp the tenant from the authoritative principal in ctx
		// (RegisterAgentRequest carries no tenant — never trust the wire).
		// "" = shared/legacy tenant. Scopes the dynamic registration to
		// the registering principal's tenant so it can't clobber another
		// tenant's same-named agent.
		TenantID: tenantFromCtx(ctx),
	}
	if err := s.store.DynamicAgentUpsert(ctx, row); err != nil {
		return connector.AgentDescriptor{}, err
	}

	desc := connector.AgentDescriptor{
		Name:         req.Name,
		Source:       "dynamic",
		AllowedTools: allowedTools,
		Tier:         req.Tier,
		Provider:     req.Provider,
		Model:        req.Model,
		Description:  req.Description,
		CreatedAt:    &createdAt,
	}
	if !expiresAt.IsZero() {
		desc.ExpiresAt = &expiresAt
	}
	return desc, nil
}

// UnregisterAgent removes a dynamic agent. Refuses to operate on
// static (yaml-defined) agents — those need a yaml edit and restart.
func (s *Server) UnregisterAgent(ctx context.Context, name string) error {
	if _, isStatic := s.cfg.Agents[name]; isStatic {
		return fmt.Errorf("agent %q is statically defined in yaml; cannot unregister", name)
	}
	if s.store == nil {
		return fmt.Errorf("unregister_agent requires persistence (no Store configured)")
	}
	// RFC N: delete scoped to the registering principal's tenant — mirrors
	// RegisterAgent's tenantFromCtx stamping so one tenant can't unregister
	// another tenant's same-named agent (exp7 C1).
	_, err := s.store.DynamicAgentDelete(ctx, tenantFromCtx(ctx), name)
	return err
}

// ListAgents merges static (cfg.Agents) and dynamic (store) agent
// descriptors. includeDynamic=false omits the dynamic set entirely.
func (s *Server) ListAgents(ctx context.Context, includeDynamic bool) ([]connector.AgentDescriptor, error) {
	out := make([]connector.AgentDescriptor, 0, len(s.cfg.Agents)+8)
	for name, def := range s.cfg.Agents {
		out = append(out, connector.AgentDescriptor{
			Name:         name,
			Source:       "static",
			AllowedTools: append([]string(nil), def.AllowedTools...),
			Tier:         def.Tier,
			Provider:     def.Provider,
			Model:        def.Model,
		})
	}
	if includeDynamic && s.store != nil {
		dyn, err := s.store.DynamicAgentList(ctx)
		if err != nil {
			return nil, err
		}
		for _, row := range dyn {
			var def config.AgentDef
			_ = json.Unmarshal(row.Definition, &def) // best-effort; we still surface name/description on parse failure
			desc := connector.AgentDescriptor{
				Name:         row.Name,
				Source:       "dynamic",
				AllowedTools: def.AllowedTools,
				Tier:         def.Tier,
				Provider:     def.Provider,
				Model:        def.Model,
				Description:  row.Description,
			}
			c := row.CreatedAt
			desc.CreatedAt = &c
			if !row.ExpiresAt.IsZero() {
				e := row.ExpiresAt
				desc.ExpiresAt = &e
			}
			out = append(out, desc)
		}
	}
	return out, nil
}

// --- 3. Builtin tool wrappers ---

func (s *Server) Memory(ctx context.Context, input json.RawMessage) (connector.ToolResult, error) {
	return s.dispatchBuiltin(ctx, "Memory", input)
}

func (s *Server) Channel(ctx context.Context, input json.RawMessage) (connector.ToolResult, error) {
	return s.dispatchBuiltin(ctx, "Channel", input)
}

func (s *Server) AgentDef(ctx context.Context, input json.RawMessage) (connector.ToolResult, error) {
	return s.dispatchBuiltin(ctx, "AgentDef", input)
}

func (s *Server) SkillDef(ctx context.Context, input json.RawMessage) (connector.ToolResult, error) {
	return s.dispatchBuiltin(ctx, "SkillDef", input)
}

// VolumeDef dispatches to the RFC AH Phase 2a dynamic-volume substrate
// tool. Unlike MCPServerDef (operator-admin-only) the VolumeDef tool IS in
// the per-agent dispatcher (s.tools) with a default-deny volume_def_scopes
// gate, so it routes through dispatchBuiltin like AgentDef/SkillDef — this
// connector method is what the POST /v1/_volumedef admin endpoint calls.
// (No gRPC/MCP-meta parity in Phase 2a — the tool is already reachable
// in-loop + over the MCP server; a gRPC RPC is a follow-up.)
func (s *Server) VolumeDef(ctx context.Context, input json.RawMessage) (connector.ToolResult, error) {
	return s.dispatchBuiltin(ctx, "VolumeDef", input)
}

// CredentialDef dispatches to the RFC AR secure credential store tool. In the
// per-agent dispatcher (s.tools), so it routes through dispatchBuiltin; what the
// MCP `credentialdef` meta-tool + in-band calls reach. Tenant/user identity comes
// from the operator-trust ctx the transport stamped, never the wire.
func (s *Server) CredentialDef(ctx context.Context, input json.RawMessage) (connector.ToolResult, error) {
	return s.dispatchBuiltin(ctx, "CredentialDef", input)
}

func (s *Server) Evaluation(ctx context.Context, input json.RawMessage) (connector.ToolResult, error) {
	return s.dispatchBuiltin(ctx, "Evaluation", input)
}

func (s *Server) Context(ctx context.Context, input json.RawMessage) (connector.ToolResult, error) {
	return s.dispatchBuiltin(ctx, "Context", input)
}

// Path dispatches to the RFC AL Path VFS tool. Like VolumeDef it IS in the
// per-agent dispatcher (s.tools), so it routes through dispatchBuiltin; this
// connector method is what POST /v1/_path, the gRPC Path RPC, and the MCP
// `path` meta-tool call. Scope/tenant come from the operator-trust ctx the
// caller's transport stamped (substrateAdminCtx / substrateGRPCCtx /
// operatorCtx), never from the wire.
func (s *Server) Path(ctx context.Context, input json.RawMessage) (connector.ToolResult, error) {
	return s.dispatchBuiltin(ctx, "Path", input)
}

// Document dispatches to the RFC AK Document tool. Same posture as Path —
// in-dispatcher tool reached via dispatchBuiltin; what POST /v1/_document,
// the gRPC Document RPC, and the MCP `document` meta-tool call.
func (s *Server) Document(ctx context.Context, input json.RawMessage) (connector.ToolResult, error) {
	return s.dispatchBuiltin(ctx, "Document", input)
}

// MCPServerDef dispatches to the v0.9.x dynamic MCP-server-registration
// substrate tool. The tool is NOT in the per-agent dispatcher (operator-
// admin-only) — dispatchBuiltinDirect looks it up via the dedicated
// registered-but-not-attached slot. See SetMCPServerDefTool for wiring.
func (s *Server) MCPServerDef(ctx context.Context, input json.RawMessage) (connector.ToolResult, error) {
	if s.mcpServerDefTool == nil {
		return connector.ToolResult{}, fmt.Errorf("MCPServerDef: not configured (no tool wired via SetMCPServerDefTool)")
	}
	res, err := s.mcpServerDefTool.Execute(ctx, input)
	if err != nil {
		return connector.ToolResult{}, err
	}
	return connector.ToolResult{Text: res.Text, IsError: res.IsError}, nil
}

// ScheduleDef dispatches to the v1.x dynamic scheduled-runs substrate
// tool. Same operator-admin-only posture as MCPServerDef. See
// SetScheduleDefTool for wiring.
func (s *Server) ScheduleDef(ctx context.Context, input json.RawMessage) (connector.ToolResult, error) {
	if s.scheduleDefTool == nil {
		return connector.ToolResult{}, fmt.Errorf("ScheduleDef: not configured (no tool wired via SetScheduleDefTool)")
	}
	res, err := s.scheduleDefTool.Execute(ctx, input)
	if err != nil {
		return connector.ToolResult{}, err
	}
	return connector.ToolResult{Text: res.Text, IsError: res.IsError}, nil
}

// A2AServerCardDef dispatches to the v1.x RFC G A2A-server-card substrate
// tool. Same operator-admin-only posture as ScheduleDef. See
// SetA2AServerCardDefTool for wiring.
func (s *Server) A2AServerCardDef(ctx context.Context, input json.RawMessage) (connector.ToolResult, error) {
	if s.a2aServerCardDefTool == nil {
		return connector.ToolResult{}, fmt.Errorf("A2AServerCardDef: not configured (no tool wired via SetA2AServerCardDefTool)")
	}
	res, err := s.a2aServerCardDefTool.Execute(ctx, input)
	if err != nil {
		return connector.ToolResult{}, err
	}
	return connector.ToolResult{Text: res.Text, IsError: res.IsError}, nil
}

// A2AAgentDef dispatches to the v1.x RFC G A2A-agent substrate tool. Same
// operator-admin-only posture as ScheduleDef. See SetA2AAgentDefTool for
// wiring.
func (s *Server) A2AAgentDef(ctx context.Context, input json.RawMessage) (connector.ToolResult, error) {
	if s.a2aAgentDefTool == nil {
		return connector.ToolResult{}, fmt.Errorf("A2AAgentDef: not configured (no tool wired via SetA2AAgentDefTool)")
	}
	res, err := s.a2aAgentDefTool.Execute(ctx, input)
	if err != nil {
		return connector.ToolResult{}, err
	}
	return connector.ToolResult{Text: res.Text, IsError: res.IsError}, nil
}

// WebhookDef dispatches to the v1.x RFC H inbound-webhook substrate tool.
// Same operator-admin-only posture as A2AAgentDef. See SetWebhookDefTool
// for wiring.
func (s *Server) WebhookDef(ctx context.Context, input json.RawMessage) (connector.ToolResult, error) {
	if s.webhookDefTool == nil {
		return connector.ToolResult{}, fmt.Errorf("WebhookDef: not configured (no tool wired via SetWebhookDefTool)")
	}
	res, err := s.webhookDefTool.Execute(ctx, input)
	if err != nil {
		return connector.ToolResult{}, err
	}
	return connector.ToolResult{Text: res.Text, IsError: res.IsError}, nil
}

// MemoryBackendDef dispatches to the RFC I MR-3a memory-backend
// substrate tool. Same operator-admin-only posture as WebhookDef. See
// SetMemoryBackendDefTool for wiring.
func (s *Server) MemoryBackendDef(ctx context.Context, input json.RawMessage) (connector.ToolResult, error) {
	if s.memoryBackendDefTool == nil {
		return connector.ToolResult{}, fmt.Errorf("MemoryBackendDef: not configured (no tool wired via SetMemoryBackendDefTool)")
	}
	res, err := s.memoryBackendDefTool.Execute(ctx, input)
	if err != nil {
		return connector.ToolResult{}, err
	}
	return connector.ToolResult{Text: res.Text, IsError: res.IsError}, nil
}

// OperatorTokenDef dispatches to the RFC L OperatorTokenDef substrate
// tool. Operator-admin-only. See SetOperatorTokenDefTool for wiring.
func (s *Server) OperatorTokenDef(ctx context.Context, input json.RawMessage) (connector.ToolResult, error) {
	if s.operatorTokenDefTool == nil {
		return connector.ToolResult{}, fmt.Errorf("OperatorTokenDef: not configured (no tool wired via SetOperatorTokenDefTool)")
	}
	res, err := s.operatorTokenDefTool.Execute(ctx, input)
	if err != nil {
		return connector.ToolResult{}, err
	}
	// RFC L Decision 11: a successful mutation flushes the auth cache
	// (locally + cross-replica) so a created/rotated token authenticates
	// and a retired one stops within one backplane round-trip. Reads
	// (get/list) don't invalidate.
	if !res.IsError && isMutatingTokenOp(input) {
		s.invalidateTokenCache(ctx)
	}
	return connector.ToolResult{Text: res.Text, IsError: res.IsError}, nil
}

// isMutatingTokenOp reports whether an OperatorTokenDef input is a
// create/rotate/retire (vs a read get/list) — used to decide whether to
// flush the auth cache.
func isMutatingTokenOp(input json.RawMessage) bool {
	var p struct {
		Op string `json:"op"`
	}
	if err := json.Unmarshal(input, &p); err != nil {
		return true // unknown shape → invalidate defensively
	}
	switch p.Op {
	case "create", "rotate", "retire":
		return true
	default:
		return false
	}
}

// dispatchBuiltin is the shared lookup-and-execute path for the five
// builtin wrappers. tools.Result {Text, IsError} maps directly onto
// connector.ToolResult; the transport adapter (MCP) then wraps both
// fields into its wire shape (content[].text + isError).
//
// Go-error return path is reserved for "tool not registered" /
// "internal failure" — distinct from IsError which is a normal tool
// outcome the model is allowed to see and self-correct from.
func (s *Server) dispatchBuiltin(ctx context.Context, name string, input json.RawMessage) (connector.ToolResult, error) {
	var tool tools.Tool
	for _, t := range s.tools {
		if t.Name() == name {
			tool = t
			break
		}
	}
	if tool == nil {
		return connector.ToolResult{}, fmt.Errorf("builtin tool %q not registered (operator disabled it via allowed_tools or yaml)", name)
	}
	res, err := tool.Execute(ctx, input)
	if err != nil {
		return connector.ToolResult{}, err
	}
	return connector.ToolResult{Text: res.Text, IsError: res.IsError}, nil
}

// --- 4. Pause/Resume/Snapshot (real in v0.8.18; wire shapes locked v0.8.15) ---
//
// Each method delegates to the pause Manager + snapshot package + Store,
// translating typed errors from those packages into the Connector's
// typed errors (connector.Err*) so MCP / gRPC / Python transports can
// errors.Is() against a single canonical set.

// PauseRuntime delegates to pause.Manager.Pause. Returns
// connector.ErrPauseNotConfigured when the manager hasn't been wired
// (e.g. no Store backend), ErrAlreadyPausing when the runtime is
// already past Running.
func (s *Server) PauseRuntime(ctx context.Context, timeoutMS int) (connector.PauseResult, error) {
	if s.pauseMgr == nil {
		return connector.PauseResult{}, connector.ErrPauseNotConfigured
	}
	timeout := time.Duration(timeoutMS) * time.Millisecond
	res, err := s.pauseMgr.Pause(ctx, timeout)
	if err != nil {
		if errors.Is(err, pause.ErrAlreadyPausing) {
			return connector.PauseResult{Status: res.State}, connector.ErrAlreadyPausing
		}
		return connector.PauseResult{}, err
	}
	return connector.PauseResult{
		Status:              res.State,
		DurationMS:          res.DurationMs,
		ForceCancelledCount: res.ForceCancelledCount,
		PausedRunsCount:     res.PausedRunsCount,
		Warnings:            res.Warnings,
	}, nil
}

// ResumeRuntime delegates to pause.Manager.Resume.
func (s *Server) ResumeRuntime(ctx context.Context) (connector.ResumeResult, error) {
	if s.pauseMgr == nil {
		return connector.ResumeResult{}, connector.ErrPauseNotConfigured
	}
	res, err := s.pauseMgr.Resume(ctx)
	if err != nil {
		if errors.Is(err, pause.ErrNotPaused) {
			return connector.ResumeResult{Status: res.State}, connector.ErrNotPaused
		}
		return connector.ResumeResult{}, err
	}
	return connector.ResumeResult{
		Status:          res.State,
		ResumedRunCount: res.ResumedRunsCount,
		Warnings:        res.Warnings,
	}, nil
}

// GetRuntimeState delegates to pause.Manager.Snapshot. Augmented with
// SnapshotsCount via a cheap COUNT against the snapshots table.
func (s *Server) GetRuntimeState(ctx context.Context) (connector.RuntimeState, error) {
	if s.pauseMgr == nil {
		return connector.RuntimeState{}, connector.ErrPauseNotConfigured
	}
	snap, err := s.pauseMgr.Snapshot(ctx)
	if err != nil {
		return connector.RuntimeState{}, err
	}
	out := connector.RuntimeState{
		Status:         snap.State,
		PausedRunCount: snap.PausedRunsCount,
	}
	// Best-effort: include the snapshots count so dashboards can
	// render it without a second round-trip. Failure here is
	// non-fatal; the state itself is still authoritative. Cap the
	// query at the same default as ListSnapshots so we don't full-
	// table-scan a large snapshots table just to surface a count;
	// dashboards consuming this value treat the cap as a saturation
	// signal ("≥ 200 snapshots"). True precise counts defer to a
	// future SnapshotCount Store method.
	if s.store != nil {
		if rows, lerr := s.store.SnapshotList(ctx, "", 200); lerr == nil {
			out.SnapshotsCount = len(rows)
		}
	}
	return out, nil
}

// CreateSnapshot delegates to snapshot.Capture + Store.SnapshotCreate.
// MaxBytes / IncludeHistory / SinceTS / Description flow through.
func (s *Server) CreateSnapshot(ctx context.Context, req connector.CreateSnapshotRequest) (connector.SnapshotDescriptor, error) {
	if s.store == nil {
		return connector.SnapshotDescriptor{}, fmt.Errorf("create_snapshot: store not configured")
	}
	opts := snapshot.CaptureOptions{
		Label:          req.Description,
		MaxBytes:       req.MaxBytes,
		IncludeHistory: req.IncludeHistory,
		Channels:       channelConfigForSnapshot(s.cfg),
	}
	if req.SinceTS != nil {
		opts.IncludeHistorySince = *req.SinceTS
	}
	if s.sqlMem != nil {
		opts.SqlMem = s.sqlMem                                               // RFC AA Phase 3e
		opts.SqlMemMaxScopeBytes = s.cfg.Storage.SqlMemSnapshotMaxScopeBytes // 3f.2 per-scope cap
	}
	row, _, err := snapshot.Capture(ctx, s.store, opts)
	if err != nil {
		var tooLarge *snapshot.ErrSnapshotTooLarge
		if errors.As(err, &tooLarge) {
			return connector.SnapshotDescriptor{}, fmt.Errorf("%w: %s", connector.ErrSnapshotTooLarge, tooLarge.Error())
		}
		return connector.SnapshotDescriptor{}, fmt.Errorf("create_snapshot: %w", err)
	}
	if err := s.store.SnapshotCreate(ctx, *row); err != nil {
		return connector.SnapshotDescriptor{}, fmt.Errorf("create_snapshot persist: %w", err)
	}
	return snapshotRowToDescriptor(*row, req.IncludeHistory, req.SinceTS), nil
}

// ListSnapshots delegates to Store.SnapshotList. Returns up to 200
// most-recent rows; transports that need more issue follow-ups (a
// pagination cursor in a future revision).
func (s *Server) ListSnapshots(ctx context.Context) ([]connector.SnapshotDescriptor, error) {
	if s.store == nil {
		return nil, fmt.Errorf("list_snapshots: store not configured")
	}
	rows, err := s.store.SnapshotList(ctx, "", 200)
	if err != nil {
		return nil, fmt.Errorf("list_snapshots: %w", err)
	}
	out := make([]connector.SnapshotDescriptor, 0, len(rows))
	for _, r := range rows {
		out = append(out, connector.SnapshotDescriptor{
			SnapshotID:    r.ID,
			CreatedAt:     r.CreatedAt,
			SizeBytes:     r.ByteSize,
			Description:   r.Label,
			FormatVersion: fmt.Sprintf("%d", r.SchemaVersion),
		})
	}
	return out, nil
}

// GetSnapshot delegates to Store.SnapshotGet and returns the full
// envelope JSON. ErrSnapshotNotFound when no row matches.
func (s *Server) GetSnapshot(ctx context.Context, snapshotID string) (connector.SnapshotEnvelope, error) {
	if s.store == nil {
		return connector.SnapshotEnvelope{}, fmt.Errorf("get_snapshot: store not configured")
	}
	row, err := s.store.SnapshotGet(ctx, snapshotID)
	if err != nil {
		var nf *store.ErrNotFound
		if errors.As(err, &nf) {
			return connector.SnapshotEnvelope{}, fmt.Errorf("%w: %s", connector.ErrSnapshotNotFound, snapshotID)
		}
		return connector.SnapshotEnvelope{}, fmt.Errorf("get_snapshot: %w", err)
	}
	return connector.SnapshotEnvelope{
		SnapshotID:    row.ID,
		CreatedAt:     row.CreatedAt,
		Description:   row.Label,
		FormatVersion: fmt.Sprintf("%d", row.SchemaVersion),
		SizeBytes:     row.ByteSize,
		JSONContent:   row.JSONContent,
	}, nil
}

// ExportSnapshot returns the canonical envelope bytes for an id.
// In v0.8.18+, transports use RawJSON directly (HTTP writes it to
// the response body with a Content-Disposition; gRPC could stream
// future). FilePath / Checksum are left empty unless a transport
// chooses to materialise to disk first.
func (s *Server) ExportSnapshot(ctx context.Context, snapshotID string) (connector.ExportSnapshotResult, error) {
	if s.store == nil {
		return connector.ExportSnapshotResult{}, fmt.Errorf("export_snapshot: store not configured")
	}
	row, err := s.store.SnapshotGet(ctx, snapshotID)
	if err != nil {
		var nf *store.ErrNotFound
		if errors.As(err, &nf) {
			return connector.ExportSnapshotResult{}, fmt.Errorf("%w: %s", connector.ErrSnapshotNotFound, snapshotID)
		}
		return connector.ExportSnapshotResult{}, fmt.Errorf("export_snapshot: %w", err)
	}
	return connector.ExportSnapshotResult{
		SnapshotID: row.ID,
		SizeBytes:  row.ByteSize,
		RawJSON:    json.RawMessage(row.JSONContent),
	}, nil
}

// RestoreSnapshot delegates to snapshot.Restore. The envelope bytes
// can come from one of three sources (mutually exclusive per request):
// RawJSON (inline), SnapshotID (fetch from this store), or FilePath
// (read from disk — operator must have placed the file at a path
// readable by the loomcycle process).
//
// When the request hits SnapshotID lookup, the store error path
// returns ErrSnapshotNotFound. Restore wraps version-skew errors as
// ErrSnapshotVersionTooNew / ErrSnapshotVersionUnknown.
//
// The resolver matrix is refreshed via ForceProbe before this
// returns, so operators can call ResumeRuntime immediately after
// without waiting for the periodic probe.
func (s *Server) RestoreSnapshot(ctx context.Context, req connector.RestoreSnapshotRequest) (connector.RestoreSnapshotResult, error) {
	if s.store == nil {
		return connector.RestoreSnapshotResult{}, fmt.Errorf("restore_snapshot: store not configured")
	}
	var rawBytes []byte
	switch {
	case len(req.RawJSON) > 0:
		rawBytes = req.RawJSON
	case req.SnapshotID != "":
		row, err := s.store.SnapshotGet(ctx, req.SnapshotID)
		if err != nil {
			var nf *store.ErrNotFound
			if errors.As(err, &nf) {
				return connector.RestoreSnapshotResult{}, fmt.Errorf("%w: %s", connector.ErrSnapshotNotFound, req.SnapshotID)
			}
			return connector.RestoreSnapshotResult{}, fmt.Errorf("restore_snapshot lookup: %w", err)
		}
		rawBytes = row.JSONContent
	case req.FilePath != "":
		// File-on-disk restore is intentionally not supported via
		// the Connector — it would require the loomcycle process
		// to have filesystem access to the operator's chosen path
		// and a trust model around what's permitted there. Use
		// inline RawJSON instead (CLI reads the file client-side).
		return connector.RestoreSnapshotResult{}, fmt.Errorf("restore_snapshot: FilePath restore not supported via connector — supply RawJSON inline")
	default:
		return connector.RestoreSnapshotResult{}, fmt.Errorf("restore_snapshot: one of snapshot_id, raw_json, or file_path is required")
	}

	opts := snapshot.RestoreOptions{IncludeHistory: req.IncludeHistory}
	if s.resolver != nil {
		opts.ForceProbe = s.resolver.ForceProbe
	}
	if s.sqlMem != nil {
		opts.SqlMem = s.sqlMem // RFC AA Phase 3e
	}
	result, err := snapshot.Restore(ctx, s.store, rawBytes, opts)
	if err != nil {
		var tooNew *migrations.ErrSnapshotVersionTooNew
		var unknown *migrations.ErrUnknownSectionVersion
		switch {
		case errors.As(err, &tooNew):
			return connector.RestoreSnapshotResult{}, fmt.Errorf("%w: %s", connector.ErrSnapshotVersionTooNew, err.Error())
		case errors.As(err, &unknown):
			return connector.RestoreSnapshotResult{}, fmt.Errorf("%w: %s", connector.ErrSnapshotVersionUnknown, err.Error())
		}
		return connector.RestoreSnapshotResult{}, fmt.Errorf("restore_snapshot: %w", err)
	}

	return connector.RestoreSnapshotResult{
		Restored: map[string]int{
			"agent_defs":          result.AgentDefsRestored,
			"agent_def_active":    result.AgentDefActiveRestored,
			"memory":              result.MemoryRestored,
			"channel_messages":    result.ChannelMessagesRestored,
			"channel_cursors":     result.ChannelCursorsRestored,
			"evaluations":         result.EvaluationsRestored,
			"paused_runs":         result.PausedRunsRestored,
			"transcript_events":   result.TranscriptEventsRestored,
			"interaction_history": result.InteractionHistoryRestored,
		},
		AgentDefsRestored:          result.AgentDefsRestored,
		AgentDefActiveRestored:     result.AgentDefActiveRestored,
		MemoryRestored:             result.MemoryRestored,
		ChannelMessagesRestored:    result.ChannelMessagesRestored,
		ChannelCursorsRestored:     result.ChannelCursorsRestored,
		EvaluationsRestored:        result.EvaluationsRestored,
		PausedRunsRestored:         result.PausedRunsRestored,
		SynthesizedSessions:        result.SynthesizedSessions,
		TranscriptEventsRestored:   result.TranscriptEventsRestored,
		InteractionHistoryRestored: result.InteractionHistoryRestored,
		Warnings:                   result.Warnings,
	}, nil
}

// DeleteSnapshot delegates to Store.SnapshotDelete. Idempotent —
// returns nil whether or not a row was present (mirrors the HTTP
// DELETE /v1/_snapshots/{id} = 204 contract).
func (s *Server) DeleteSnapshot(ctx context.Context, snapshotID string) error {
	if s.store == nil {
		return fmt.Errorf("delete_snapshot: store not configured")
	}
	if _, err := s.store.SnapshotDelete(ctx, snapshotID); err != nil {
		return fmt.Errorf("delete_snapshot: %w", err)
	}
	return nil
}

// snapshotRowToDescriptor maps a stored row into the wire-facing
// SnapshotDescriptor. includesHistory + sinceTS are supplied by the
// caller (CreateSnapshot remembers them from the request; ListSnapshots
// can't reconstruct them from the row, so leaves them zero).
func snapshotRowToDescriptor(row store.SnapshotRow, includesHistory bool, sinceTS *time.Time) connector.SnapshotDescriptor {
	return connector.SnapshotDescriptor{
		SnapshotID:      row.ID,
		CreatedAt:       row.CreatedAt,
		SizeBytes:       row.ByteSize,
		IncludesHistory: includesHistory,
		SinceTS:         sinceTS,
		Description:     row.Label,
		FormatVersion:   fmt.Sprintf("%d", row.SchemaVersion),
	}
}

// --- Interruption (v0.8.16) ---

// InterruptionResolve implements connector.Connector. It mirrors the
// HTTP resolve endpoint's logic without round-tripping through HTTP:
// validate row + answer, transition status, fire bus.Notify,
// publish to _system/interrupts/resolved.
//
// Used by the LoomCycle MCP server's `interruption_resolve` meta-tool
// so external orchestrators (Claude Code, custom dashboards) can act
// as the answerer without speaking HTTP to loomcycle.
func (s *Server) InterruptionResolve(ctx context.Context, req connector.InterruptionResolveRequest) (connector.InterruptionResolveResult, error) {
	if s.store == nil {
		return connector.InterruptionResolveResult{}, fmt.Errorf("interruption_resolve: store not configured")
	}
	kind := req.Kind
	if kind == "" {
		kind = store.InterruptKindQuestion
	}
	if kind != store.InterruptKindQuestion {
		return connector.InterruptionResolveResult{}, fmt.Errorf("interruption_resolve: unsupported kind %q (v0.8.16 supports: question)", kind)
	}
	resolvedBy := req.ResolvedBy
	if resolvedBy == "" {
		resolvedBy = store.InterruptResolvedByMCP
	}

	row, err := s.store.InterruptGet(ctx, req.InterruptID)
	if err != nil {
		return connector.InterruptionResolveResult{}, err
	}
	if row.RunID != req.RunID {
		return connector.InterruptionResolveResult{}, fmt.Errorf("interruption_resolve: interrupt %q does not belong to run %q", req.InterruptID, req.RunID)
	}
	// Tenant ownership gate (RFC L/N): the run this interrupt belongs to must be
	// in the caller's tenant, else resolving it steers ANOTHER tenant's paused
	// run. Backs the MCP interruption_resolve tool (principal-bearing ctx). The
	// row.RunID==req.RunID check above only blocks retargeting within a tenant.
	// tenantStore folds a cross-tenant/missing run into an opaque ErrNotFound.
	if _, err := s.tenantStore(ctx).GetRun(ctx, row.RunID); err != nil {
		return connector.InterruptionResolveResult{}, err
	}
	if row.Status != store.InterruptStatusPending {
		return connector.InterruptionResolveResult{}, fmt.Errorf("interruption_resolve: already %s", row.Status)
	}
	if !row.ExpiresAt.IsZero() && row.ExpiresAt.Before(time.Now()) {
		return connector.InterruptionResolveResult{}, fmt.Errorf("interruption_resolve: expired")
	}

	// Option-list validation. Same shape as the HTTP handler.
	if len(row.Options) > 0 {
		var opts []string
		if err := json.Unmarshal(row.Options, &opts); err == nil && len(opts) > 0 {
			ok := false
			for _, o := range opts {
				if o == req.Answer {
					ok = true
					break
				}
			}
			if !ok {
				return connector.InterruptionResolveResult{}, fmt.Errorf("interruption_resolve: answer %q is not in declared options %v", req.Answer, opts)
			}
		}
	} else if req.Answer == "" {
		return connector.InterruptionResolveResult{}, fmt.Errorf("interruption_resolve: answer required for free-text interrupts")
	}

	if err := s.store.InterruptResolve(ctx, req.InterruptID, req.Answer, resolvedBy, nil); err != nil {
		return connector.InterruptionResolveResult{}, err
	}
	if s.interruptionBus != nil {
		s.interruptionBus.Notify("intr:" + req.InterruptID)
	}
	if s.systemPublisher != nil && row.UserID != "" {
		payload, _ := json.Marshal(map[string]any{
			"interrupt_id": req.InterruptID,
			"run_id":       req.RunID,
			"kind":         row.Kind,
			"status":       store.InterruptStatusResolved,
			"answer":       req.Answer,
			"resolved_by":  resolvedBy,
		})
		_, _ = s.systemPublisher.PublishNow(
			ctx,
			"_system/interrupts/resolved",
			store.MemoryScopeUser, row.UserID,
			payload, channels.SystemPublisherUserID, 0, 0,
		)
	}

	return connector.InterruptionResolveResult{
		InterruptID: req.InterruptID,
		Status:      store.InterruptStatusResolved,
		ResolvedAt:  time.Now().UTC().Format(time.RFC3339Nano),
	}, nil
}

// --- Helpers ---

// validDynamicAgentName matches [A-Za-z0-9_-]{1,64}. Stricter than
// general identifiers because agent names appear in log lines, yaml
// errors, and `mcp__loomcycle__spawn_run` parameters.
func validDynamicAgentName(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '_', r == '-':
			continue
		default:
			return false
		}
	}
	return true
}

// stripPrivilegedTools removes Bash/Write/Edit from the requested
// allowed_tools when the operator hasn't opted into privileged-tool
// dynamic registration. Default-deny matches v0.8.7 / v0.8.8 pattern.
func stripPrivilegedTools(requested []string, allowPrivileged bool) []string {
	if allowPrivileged {
		return append([]string(nil), requested...)
	}
	out := make([]string, 0, len(requested))
	for _, t := range requested {
		switch t {
		case "Bash", "Write", "Edit":
			continue
		default:
			out = append(out, t)
		}
	}
	return out
}

// computeExpiresAt resolves the effective TTL for a register_agent
// request. TTLSeconds < 0 means "no expiry" (return zero-value
// time.Time, which the store treats as 'epoch' = no expiry). 0 falls
// back to the env default. Positive values are used directly.
func computeExpiresAt(now time.Time, ttlSeconds, defaultTTLSeconds int) time.Time {
	if ttlSeconds < 0 {
		return time.Time{}
	}
	if ttlSeconds == 0 {
		ttlSeconds = defaultTTLSeconds
	}
	if ttlSeconds <= 0 {
		// defaultTTLSeconds also unset or non-positive → no expiry.
		return time.Time{}
	}
	return now.Add(time.Duration(ttlSeconds) * time.Second)
}
