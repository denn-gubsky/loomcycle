package http

import (
	"context"
	"encoding/json"
	"io"
	"net/http"

	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// v0.8.22 substrate admin endpoints. Two handlers (AgentDef +
// SkillDef) accept the same op-discriminated JSON body the in-
// process tools accept, dispatch through the Connector with an
// operator-trust ctx, and return the tool's output as JSON.
//
// Same connector path as the MCP `agentdef` / `skilldef`
// meta-tools — different transport, identical semantics. This
// gives external HTTP callers (the TypeScript adapter, curl) a
// wire to drive substrate ops without an in-band agent run.
//
// Operator-trust posture: bearer-authed callers are the operator
// (same trust as the MCP launcher in `loomcycle mcp ...`). We
// synthesise a permissive policy with scope=[any] so the in-
// process tool's default-deny scope gate doesn't refuse the call.

const (
	// substrateAdminAgentID is the synthetic agent_id stamped on
	// the ctx for substrate admin dispatches. Distinct from the
	// MCP operator agent_id so audit queries can tell HTTP-admin
	// activity apart from MCP-direct activity.
	substrateAdminAgentID = "a_http-admin"
	// substrateAdminUserID is the corresponding user_id. Same
	// rationale as above.
	substrateAdminUserID = "http-admin"
	// substrateAdminAgentName is the synthetic agent name.
	substrateAdminAgentName = "http-admin"
)

// handleSubstrateAgentDef serves POST /v1/_agentdef.
func (s *Server) handleSubstrateAgentDef(w http.ResponseWriter, r *http.Request) {
	s.dispatchSubstrate(w, r, "AgentDef", s.AgentDef)
}

// handleSubstrateSkillDef serves POST /v1/_skilldef.
func (s *Server) handleSubstrateSkillDef(w http.ResponseWriter, r *http.Request) {
	s.dispatchSubstrate(w, r, "SkillDef", s.SkillDef)
}

// dispatchSubstrate is the shared body of the two handlers.
// connectorFn is the Connector method (already a method value
// bound to the Server). toolName is the label used in error
// envelopes.
func (s *Server) dispatchSubstrate(
	w http.ResponseWriter, r *http.Request, toolName string,
	connectorFn func(ctx context.Context, input json.RawMessage) (connector.ToolResult, error),
) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20)) // 1 MB cap
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "bad_request", "read body: "+err.Error())
		return
	}
	if len(body) == 0 {
		writeJSONError(w, http.StatusBadRequest, "bad_request", "empty body — expected substrate tool input JSON")
		return
	}
	// Validate the body is JSON before we forward it. Catches the
	// "operator sent XML" class of errors here rather than letting
	// the in-process tool barf with a confusing message.
	if !json.Valid(body) {
		writeJSONError(w, http.StatusBadRequest, "bad_request", "body is not valid JSON")
		return
	}

	ctx := substrateAdminCtx(r.Context())
	result, err := connectorFn(ctx, body)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}

	// Mirror the in-process tool's IsError convention onto HTTP.
	// The tool's Text payload is JSON when it succeeded, plain
	// error text when it failed. We pass through verbatim — the
	// caller parses based on the status code.
	if result.IsError {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnprocessableEntity)
		// Wrap the error text in a canonical envelope so the
		// caller can branch on `error` without parsing the
		// human-readable message.
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code":  "tool_refused",
			"error": result.Text,
			"tool":  toolName,
		})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	// result.Text is the tool's JSON output (e.g. the row body
	// from a create/fork, or the error envelope from a refused
	// scope check). Pass through verbatim.
	_, _ = w.Write([]byte(result.Text))
}

// substrateAdminCtx stamps the operator-trust ctx for HTTP
// substrate admin dispatches. Mirror of the MCP operatorCtx —
// same six policy grants, distinct synthetic identifiers so
// audit logs distinguish HTTP-admin from MCP-direct.
//
// Why all six policies even though the only handlers today are
// AgentDef + SkillDef: dispatchSubstrate is already generalized
// (it accepts any connectorFn). A future endpoint like
// POST /v1/_memory or POST /v1/_evaluation reusing this helper
// would silently default-deny without these grants. Keep them
// symmetric with operatorCtx so the helper stays safe to extend.
func substrateAdminCtx(ctx context.Context) context.Context {
	ctx = tools.WithRunIdentity(ctx, tools.RunIdentityValue{
		UserID:  substrateAdminUserID,
		AgentID: substrateAdminAgentID,
	})
	ctx = tools.WithAgentName(ctx, substrateAdminAgentName)
	// Memory: full scope access. QuotaBytes=0 falls back to the
	// global default (LOOMCYCLE_MEMORY_MAX_SCOPE_BYTES).
	ctx = tools.WithMemoryPolicy(ctx, tools.MemoryPolicyValue{
		AllowedScopes: []string{"agent", "user", "global"},
	})
	// Channel: "*" wildcard matches every channel name.
	ctx = tools.WithChannelPolicy(ctx, tools.ChannelPolicyValue{
		Publish:   []string{"*"},
		Subscribe: []string{"*"},
	})
	// AgentDef + SkillDef: "any" scope (operator-blessed admin).
	ctx = tools.WithAgentDefPolicy(ctx, tools.AgentDefPolicyValue{
		Scopes:   []string{"any"},
		SelfName: substrateAdminAgentName,
	})
	ctx = tools.WithSkillDefPolicy(ctx, tools.SkillDefPolicyValue{
		Scopes: []string{"any"},
	})
	// Evaluation: all 4 valid scope values.
	ctx = tools.WithEvaluationPolicy(ctx, tools.EvaluationPolicyValue{
		Scopes: []string{"submit_self", "submit_descendants", "submit_any", "read_any"},
	})
	// Context.history: "any" — read every agent's transcript.
	ctx = tools.WithHistoryPolicy(ctx, tools.HistoryPolicyValue{
		Scopes: []string{"any"},
	})
	return ctx
}
