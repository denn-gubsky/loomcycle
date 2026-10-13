package mcp

import (
	"context"
	"encoding/json"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/connector"
	loommcp "github.com/denn-gubsky/loomcycle/internal/tools/mcp"
)

// handleRetentionOverride serves the `retention_override` tool: op=list |
// set | clear, delegating to the shared connector so this transport cannot
// differ from HTTP or gRPC in who may call it, what it lifts or what is
// audited.
func handleRetentionOverride(ctx context.Context, env *handlerEnv, args json.RawMessage) (*loommcp.CallToolResult, error) {
	var p struct {
		Op     string `json:"op"`
		Agent  string `json:"agent"`
		Until  string `json:"until"`
		Tenant string `json:"tenant"`
	}
	if len(args) > 0 {
		if err := json.Unmarshal(args, &p); err != nil {
			return toolErr("invalid retention_override arguments: " + err.Error()), nil
		}
	}
	if p.Op == "" {
		p.Op = "list"
	}
	req := connector.RetentionOverrideRequest{Op: p.Op, Tenant: p.Tenant, Agent: p.Agent}
	if p.Until != "" {
		t, err := time.Parse(time.RFC3339, p.Until)
		if err != nil {
			return toolErrValidation("retention_override: until is not an RFC 3339 time: "+err.Error(),
				`pass until like "2026-10-12T18:00:00Z"`), nil
		}
		req.Until = t
	}
	res, err := env.connector.RetentionOverride(ctx, req)
	if err != nil {
		return toolErrFrom("retention_override "+p.Op, err), nil
	}
	return toolResultJSON(res), nil
}
