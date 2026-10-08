package builtin

import (
	"context"
	"encoding/json"

	"github.com/denn-gubsky/loomcycle/internal/auth"
)

// teamDefOpScope is the token scope an op needs when it is called from an
// operator surface, on top of whatever admitted the caller to the tool. Most
// TeamDef ops author definitions, which a tenant member may do whatever its
// scopes; these three act on RUNS, and are held to the scope the run routes
// need (POST /v1/runs, the Run RPC, the spawn_run and get_run tools). An op
// with no entry needs nothing more.
var teamDefOpScope = map[string]string{
	// A walk spawns member runs and spends tokens.
	"run":    auth.ScopeRunsCreate,
	"cancel": auth.ScopeRunsCreate,
	"poll":   auth.ScopeRunsRead,
}

// TeamDefMissingScope reports the scope the principal on ctx lacks for the op
// in a TeamDef input, and that op; scope is "" when the call may proceed.
//
// It is for the OPERATOR surfaces only (the HTTP route, the MCP tool), which
// call it with the request's own ctx before dispatching. It must never be
// called from Execute: a run's ctx can carry the principal of whoever started
// or last steered the run, and what an agent may do with this tool is decided
// by the agent's own grants, not by that token's scopes.
//
// No principal (no authentication configured, or the stdio launcher) is the
// operator and lacks nothing. The input is read into the tool's own input type
// so the op judged here is the op Execute will run; an input that does not
// parse is left for Execute to report.
func TeamDefMissingScope(ctx context.Context, raw json.RawMessage) (op, scope string) {
	p, ok := auth.PrincipalFromContext(ctx)
	if !ok {
		return "", ""
	}
	var in teamDefInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return "", ""
	}
	if need := teamDefOpScope[in.Op]; need != "" && !auth.HasScope(p.Scopes, need) {
		return in.Op, need
	}
	return "", ""
}
