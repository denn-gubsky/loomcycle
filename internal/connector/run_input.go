package connector

import "github.com/denn-gubsky/loomcycle/internal/runner"

// SpawnRequestToRunInput maps a spawn request onto a RunInput. It is the one
// mapping every spawn path uses: the blocking connector spawn, the batch, the
// start of a configured or queued run, and MCP spawn_run's streaming branch.
// That last branch once kept its own field-by-field copy, which dropped
// admission and interruption for every streaming caller.
func SpawnRequestToRunInput(req SpawnRunRequest) runner.RunInput {
	return runner.RunInput{
		Agent:           req.Agent,
		SessionID:       req.SessionID,
		TenantID:        req.TenantID,
		Segments:        req.Segments,
		Tools:           req.Tools,
		AllowedHosts:    req.AllowedHosts,
		WebSearchFilter: req.WebSearchFilter,
		UserID:          req.UserID,
		AgentID:         req.AgentID,
		UserTier:        req.UserTier,
		UserBearer:      req.UserBearer,
		UserCredentials: req.UserCredentials, // v1.x RFC F: per-tool named credentials
		ParentContext:   req.ParentContext,   // v0.12.x: opaque tracking lineage
		// The caller's key, as sent; RunOnce scopes and stores it.
		ClientIdempotencyKey: req.IdempotencyKey,
		MaxWallSeconds:       req.MaxWallSeconds,
		Admission:            req.Admission, // RFC EI P3: wait (queue) or reject
		Retention:            req.Retention,
		Metadata:             req.Metadata,         // non-secret trusted agent metadata
		Sampling:             req.Sampling,         // per-run LLM sampling override
		ToolChoice:           req.ToolChoice,       // per-run tool_choice (RFC DI)
		OutputFormat:         req.OutputFormat,     // per-run answer schema (RFC DI)
		Routing:              req.Routing,          // per-run routing: block (queue-aware start)
		Compaction:           req.Compaction,       // per-run context-compaction override
		Context:              req.Context,          // per-run layered-context override (RFC CR)
		MaxContextTokens:     req.MaxContextTokens, // RFC CJ per-run context-window override
		// RFC DC P6: the spawn surfaces reach the SAME validation the HTTP
		// endpoint uses, because they hand RunOnce the same RunInput.
		Model:                 req.Model,
		Provider:              req.Provider,
		Tier:                  req.Tier,
		Effort:                req.Effort,
		MaxTokens:             req.MaxTokens,
		MaxIterations:         req.MaxIterations,
		UnboundedIterations:   req.UnboundedIterations,
		MaxConcurrentChildren: req.MaxConcurrentChildren,
		RetryAttempts:         req.RetryAttempts,
		MemoryInjectMaxTokens: req.MemoryInjectMaxTokens,
		MemoryIndexMaxBytes:   req.MemoryIndexMaxBytes,
		InjectToolGuide:       req.InjectToolGuide,
		// A blocking spawn honours review: the call returns once an operator
		// approves the held answer or rejects it.
		Review:           req.Review != nil && *req.Review,
		ReviewTTLSeconds: req.ReviewTTLSeconds,
		Interruption:     req.Interruption, // per-run "may this agent ask a human"
		Hooks:            req.Hooks,
		ToolHooks:        req.ToolHooks,
	}
}
