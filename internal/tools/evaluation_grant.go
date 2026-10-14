package tools

import (
	"github.com/denn-gubsky/loomcycle/internal/auth"
)

// OffRunEvaluationPolicy is the Evaluation grant for a caller that reaches the
// tool OUTSIDE a run — POST /v1/_evaluation, the Evaluation RPC and the MCP
// `evaluation` tool. A run's grant is its agent's evaluation_scopes; off a run
// there is no agent, so the grant comes from the token instead.
//
// It is the one answer for all three transports. Each used to decide on its
// own: HTTP and gRPC granted every op to every principal, while MCP granted a
// tenant operator submit_self alone. One function makes a fourth answer
// impossible, and the tests in internal/api/evaluationtest fail if a transport
// stops calling it.
//
//   - No principal (open mode, the stdio MCP launcher), substrate:admin
//     (incl. the legacy token) and substrate:tenant: every op. The tool, not
//     this grant, keeps a tenant operator inside its own tenant — it reads the
//     tenant from RunIdentity and answers another tenant's id as missing.
//   - Anyone else (an isolated substrate:user, a tenant member token):
//     submit_self only. Reading another run's scores, or scoring a run that is
//     not one's own, is the operator's selection data.
//
// submit_self is the only value an agent gets by default, so the floor here
// grants nothing an agent run does not already hold.
func OffRunEvaluationPolicy(p auth.Principal, ok bool) EvaluationPolicyValue {
	if !ok || auth.HasScope(p.Scopes, auth.ScopeTenant) {
		return EvaluationPolicyValue{Scopes: []string{"submit_self", "submit_descendants", "submit_any", "read_any"}}
	}
	return EvaluationPolicyValue{Scopes: []string{"submit_self"}}
}
