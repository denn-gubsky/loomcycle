// Package http — v0.8.15 Connector interface implementation.
//
// The HTTP Server is the canonical connector.Connector implementation.
// Other wire transports (MCP, gRPC, future CLI) CONSUME the Connector
// rather than re-implementing business logic.
//
// Methods in this file are organised by Connector method group:
//   1. Run lifecycle: SpawnRun, CancelRun, GetRun, ListRuns
//   2. Agent management: RegisterAgent, UnregisterAgent, ListAgents
//   3. Builtin tool wrappers: Memory, Channel, AgentDef, Evaluation, Context
//   4. Pause/Resume/Snapshot: MOCKED in v0.8.15 (see RFC)

package http

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/channels"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/runner"
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
		AgentID:    regAgentID,
		RunID:      regRunID,
		SessionID:  regSessionID,
		Status:     string(store.RunCompleted),
		StopReason: finalStopReason,
		FinalText:  finalText,
		Usage:      finalUsage,
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

// CancelRun looks up the run by agent_id and triggers cancellation
// via the in-memory cancel registry. The registry's Cancel handles
// the BFS cascade to sub-agents internally.
func (s *Server) CancelRun(ctx context.Context, agentID, reason string) (connector.CancelRunResult, error) {
	if agentID == "" {
		return connector.CancelRunResult{}, fmt.Errorf("agent_id required")
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
		SystemPrompt: req.SystemPrompt,
		AllowedTools: allowedTools,
		Tier:         req.Tier,
		Provider:     req.Provider,
		Model:        req.Model,
		Effort:       req.Effort,
		MaxTokens:    req.MaxTokens,
		MemoryScopes: req.MemoryScopes,
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
	_, err := s.store.DynamicAgentDelete(ctx, name)
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

func (s *Server) Evaluation(ctx context.Context, input json.RawMessage) (connector.ToolResult, error) {
	return s.dispatchBuiltin(ctx, "Evaluation", input)
}

func (s *Server) Context(ctx context.Context, input json.RawMessage) (connector.ToolResult, error) {
	return s.dispatchBuiltin(ctx, "Context", input)
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

// --- 4. Pause/Resume/Snapshot (MOCKED in v0.8.15) ---
//
// Wire shapes stable; real implementations land in v0.8.16+ per
// doc-internal/rfcs/pause-resume-snapshot.md.

const previewNote = "Pause/Resume/Snapshot implementation pending in v0.8.16+; wire shape is stable. See doc-internal/rfcs/pause-resume-snapshot.md."

func (s *Server) PauseRuntime(_ context.Context, _ int) (connector.PauseResult, error) {
	return connector.PauseResult{
		Status:        "paused",
		FeatureStatus: "preview",
		Note:          previewNote,
	}, nil
}

func (s *Server) ResumeRuntime(_ context.Context) (connector.ResumeResult, error) {
	return connector.ResumeResult{
		Status:        "running",
		FeatureStatus: "preview",
		Note:          previewNote,
	}, nil
}

func (s *Server) GetRuntimeState(_ context.Context) (connector.RuntimeState, error) {
	return connector.RuntimeState{
		Status:        "running",
		FeatureStatus: "preview",
	}, nil
}

func (s *Server) CreateSnapshot(_ context.Context, _ connector.CreateSnapshotRequest) (connector.SnapshotDescriptor, error) {
	return connector.SnapshotDescriptor{
		SnapshotID:    mintPreviewSnapshotID(),
		CreatedAt:     time.Now().UTC(),
		FormatVersion: "preview",
		FeatureStatus: "preview",
	}, nil
}

func (s *Server) ListSnapshots(_ context.Context) ([]connector.SnapshotDescriptor, error) {
	// Mocks don't persist — list is always empty in v0.8.15.
	return []connector.SnapshotDescriptor{}, nil
}

func (s *Server) ExportSnapshot(_ context.Context, snapshotID string) (connector.ExportSnapshotResult, error) {
	return connector.ExportSnapshotResult{
		SnapshotID:    snapshotID,
		FeatureStatus: "preview",
	}, fmt.Errorf("export_snapshot: %s", previewNote)
}

func (s *Server) RestoreSnapshot(_ context.Context, _ connector.RestoreSnapshotRequest) (connector.RestoreSnapshotResult, error) {
	return connector.RestoreSnapshotResult{
		Restored:      map[string]int{},
		FeatureStatus: "preview",
	}, fmt.Errorf("restore_snapshot: %s", previewNote)
}

func (s *Server) DeleteSnapshot(_ context.Context, _ string) error {
	return fmt.Errorf("delete_snapshot: %s", previewNote)
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

// mintPreviewSnapshotID produces a snap_preview_<8hex> identifier.
// Real snapshot IDs in v0.8.16+ will use a different prefix
// (snap_<time><rand>) so callers can distinguish preview-only IDs.
func mintPreviewSnapshotID() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return "snap_preview_" + hex.EncodeToString(b[:])
}
