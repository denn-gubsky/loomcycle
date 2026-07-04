package http

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/auth"
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

// handleSubstrateMCPServerDef serves POST /v1/_mcpserverdef.
// v0.9.x dynamic MCP server registration. Bearer-authed; same
// dispatch shape as the AgentDef + SkillDef admin endpoints.
func (s *Server) handleSubstrateMCPServerDef(w http.ResponseWriter, r *http.Request) {
	s.dispatchSubstrate(w, r, "MCPServerDef", s.MCPServerDef)
}

// handleSubstrateScheduleDef serves POST /v1/_scheduledef.
// v1.x scheduled-runs substrate. Bearer-authed; same dispatch
// shape as the AgentDef + SkillDef + MCPServerDef endpoints.
func (s *Server) handleSubstrateScheduleDef(w http.ResponseWriter, r *http.Request) {
	s.dispatchSubstrate(w, r, "ScheduleDef", s.ScheduleDef)
}

// handleSubstrateA2AServerCardDef serves POST /v1/_a2aservercarddef.
// v1.x RFC G A2A-server-card substrate. Bearer-authed; same dispatch
// shape as the other substrate endpoints.
func (s *Server) handleSubstrateA2AServerCardDef(w http.ResponseWriter, r *http.Request) {
	s.dispatchSubstrate(w, r, "A2AServerCardDef", s.A2AServerCardDef)
}

// handleSubstrateA2AAgentDef serves POST /v1/_a2aagentdef.
// v1.x RFC G A2A-agent substrate. Bearer-authed; same dispatch shape
// as the other substrate endpoints.
func (s *Server) handleSubstrateA2AAgentDef(w http.ResponseWriter, r *http.Request) {
	s.dispatchSubstrate(w, r, "A2AAgentDef", s.A2AAgentDef)
}

// handleSubstrateWebhookDef serves POST /v1/_webhookdef.
// v1.x RFC H Input Webhooks substrate. Bearer-authed; same dispatch
// shape as the other substrate endpoints.
func (s *Server) handleSubstrateWebhookDef(w http.ResponseWriter, r *http.Request) {
	s.dispatchSubstrate(w, r, "WebhookDef", s.WebhookDef)
}

// handleSubstrateMemoryBackendDef serves POST /v1/_memorybackenddef.
// RFC I MR-3a MemoryBackendDef substrate. Bearer-authed; same dispatch
// shape as the other substrate endpoints.
func (s *Server) handleSubstrateMemoryBackendDef(w http.ResponseWriter, r *http.Request) {
	s.dispatchSubstrate(w, r, "MemoryBackendDef", s.MemoryBackendDef)
}

// handleSubstrateVolumeDef serves POST /v1/_volumedef.
// RFC AH Phase 2a dynamic-volume substrate. Bearer-authed; same dispatch
// shape as the other substrate endpoints. The capability gate is the
// agent's volume_def_scopes — substrateAdminCtx grants ["any"] so the
// HTTP-admin caller (operator trust) isn't default-denied.
func (s *Server) handleSubstrateVolumeDef(w http.ResponseWriter, r *http.Request) {
	s.dispatchSubstrate(w, r, "VolumeDef", s.VolumeDef)
}

// handleSubstratePath serves POST /v1/_path.
// RFC AL Path VFS. Bearer-authed; no per-tool scope policy (gated by
// tools; substrateAdminUserCtx grants the ["*"] tool ceiling). Uses
// the USER-aware ctx so an off-run user-scoped op operates on the SAME
// user-scope tree an agent run for this principal uses (see
// substrateAdminUserCtx).
func (s *Server) handleSubstratePath(w http.ResponseWriter, r *http.Request) {
	s.dispatchSubstrateCtx(w, r, "Path", s.Path, s.substrateBrowseCtxFn(r))
}

// handleSubstrateDocument serves POST /v1/_document.
// RFC AK chunked-graph documents. Bearer-authed; requires SQL Memory. Uses the
// USER-aware ctx so off-run user-scoped documents interoperate with the
// principal's agent runs (see substrateAdminUserCtx).
func (s *Server) handleSubstrateDocument(w http.ResponseWriter, r *http.Request) {
	s.dispatchSubstrateCtx(w, r, "Document", s.Document, s.substrateBrowseCtxFn(r))
}

// handleSubstrateCredentialDef serves POST /v1/_credentialdef.
// RFC AR secure per-tenant credential store. Bearer-authed; tenant-confined
// (ScopeTenant via isTenantConfinedDefPath). Uses the USER-aware ctx
// (substrateAdminUserCtx) so a scope:"user" op keys on the principal's OWN
// subject — the same user-scope id an agent run for this principal uses
// (auth.applyPrincipal derives user_id from principal.Subject) — and a
// scope:"tenant" op stamps the principal's authoritative tenant. Identity comes
// from the operator-trust ctx, NEVER the wire (the tool reads RunIdentity, not
// the request body). It uses substrateAdminUserCtx (not substrateBrowseCtxFn) on
// purpose: credentials are confined to the caller's OWN subject — there is no
// cross-subject ?scope_id= browse for secrets. SECURITY: the create op carries a
// plaintext `value`; this handler adds NO logging of the body or value (the tool
// masks the value from the transcript and never echoes it in get/list output).
func (s *Server) handleSubstrateCredentialDef(w http.ResponseWriter, r *http.Request) {
	s.dispatchSubstrateCtx(w, r, "CredentialDef", s.CredentialDef, substrateAdminUserCtx)
}

// dispatchSubstrate is the shared body of the substrate-def handlers.
// connectorFn is the Connector method (already a method value bound to the
// Server). toolName is the label used in error envelopes. The def-tools scope
// on tenant (not user), so they use the plain operator-trust substrateAdminCtx.
func (s *Server) dispatchSubstrate(
	w http.ResponseWriter, r *http.Request, toolName string,
	connectorFn func(ctx context.Context, input json.RawMessage) (connector.ToolResult, error),
) {
	s.dispatchSubstrateCtx(w, r, toolName, connectorFn, substrateAdminCtx)
}

// dispatchSubstrateCtx is the generalized body: ctxFn builds the operator-trust
// ctx (substrateAdminCtx for the tenant-scoped def-tools; substrateAdminUserCtx
// for the user-scope-aware Path/Document tools).
func (s *Server) dispatchSubstrateCtx(
	w http.ResponseWriter, r *http.Request, toolName string,
	connectorFn func(ctx context.Context, input json.RawMessage) (connector.ToolResult, error),
	ctxFn func(context.Context) context.Context,
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

	ctx := ctxFn(r.Context())
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
	// RFC N tenant invariant: the tenant comes from the authoritative
	// principal the auth middleware stamped on ctx, NEVER the wire. The
	// def-tools read tools.RunIdentity(ctx).TenantID to stamp the row's
	// tenant_id; without carrying it here every admin-registered def
	// lands in the shared "" tenant regardless of the caller. Zero value
	// "" when no principal (legacy LOOMCYCLE_AUTH_TOKEN / open mode) →
	// shared tenant, which is the correct single-tenant behavior.
	principal, _ := auth.PrincipalFromContext(ctx)
	ctx = tools.WithRunIdentity(ctx, tools.RunIdentityValue{
		UserID:   substrateAdminUserID,
		AgentID:  substrateAdminAgentID,
		TenantID: principal.TenantID,
	})
	ctx = tools.WithAgentName(ctx, substrateAdminAgentName)
	// AgentTools wildcard: substrate admin = operator trust. Without
	// this, AgentDef.create / SkillDef.create with a non-empty
	// tools overlay refuse with "caller's effective
	// tools not on ctx (runtime misconfiguration)" because
	// the create-time ceiling check (assertToolsSubset against
	// callerTools) treats a nil callerTools as "unknown ceiling →
	// refuse". For operator-trust paths the operator is the security
	// boundary (bearer-authed /v1/_*), so attach a wildcard ceiling
	// the subset check recognises. Regular per-run contexts continue
	// to set WithAgentTools to the agent's actual tools list,
	// so the capability-escalation guard for in-loop AgentDef calls
	// is unchanged.
	ctx = tools.WithAgentTools(ctx, []string{"*"})
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
	// VolumeDef (RFC AH Phase 2a): "any" scope so the HTTP-admin caller can
	// create/delete/purge any dynamic volume name (operator trust).
	ctx = tools.WithVolumeDefPolicy(ctx, tools.VolumeDefPolicyValue{
		Scopes: []string{"any"},
	})
	// ScheduleDef: same operator-trust posture; "any" scope lets the
	// HTTP-admin call create/fork/retire any schedule name regardless
	// of the orchestrator-agent naming used by in-loop callers.
	ctx = tools.WithScheduleDefPolicy(ctx, tools.ScheduleDefPolicyValue{
		Scopes:   []string{"any"},
		SelfName: substrateAdminAgentName,
	})
	// A2AServerCardDef + A2AAgentDef: same operator-trust posture; "any"
	// scope lets the HTTP-admin call create/fork/retire any def name.
	ctx = tools.WithA2AServerCardDefPolicy(ctx, tools.A2AServerCardDefPolicyValue{
		Scopes:   []string{"any"},
		SelfName: substrateAdminAgentName,
	})
	ctx = tools.WithA2AAgentDefPolicy(ctx, tools.A2AAgentDefPolicyValue{
		Scopes:   []string{"any"},
		SelfName: substrateAdminAgentName,
	})
	// WebhookDef: same operator-trust posture; "any" scope lets the
	// HTTP-admin call create/fork/retire any webhook name (RFC H WH-2).
	ctx = tools.WithWebhookDefPolicy(ctx, tools.WebhookDefPolicyValue{
		Scopes:   []string{"any"},
		SelfName: substrateAdminAgentName,
	})
	// MemoryBackendDef: same operator-trust posture; "any" scope lets the
	// HTTP-admin call create/fork/retire any backend name (RFC I MR-3a).
	ctx = tools.WithMemoryBackendDefPolicy(ctx, tools.MemoryBackendDefPolicyValue{
		Scopes:   []string{"any"},
		SelfName: substrateAdminAgentName,
	})
	// Evaluation: all 4 valid scope values.
	ctx = tools.WithEvaluationPolicy(ctx, tools.EvaluationPolicyValue{
		Scopes: []string{"submit_self", "submit_descendants", "submit_any", "read_any"},
	})
	// Context.history: "any" — read every agent's transcript.
	ctx = tools.WithHistoryPolicy(ctx, tools.HistoryPolicyValue{
		Scopes: []string{"any"},
	})
	// OperatorTokenDef: operator-admin (RFC L). The /v1/_* endpoints are
	// bearer-authed against the operator; minting auth tokens is the
	// operator's prerogative. PR2's middleware additionally gates this
	// route behind the substrate:admin scope.
	ctx = tools.WithOperatorTokenDefPolicy(ctx, tools.OperatorTokenDefPolicyValue{Admin: true})
	return ctx
}

// substrateAdminUserCtx is substrateAdminCtx with the user_id resolved to the
// authenticated principal's Subject (not the synthetic http-admin id). It's
// used by the scope-aware Path + Document endpoints so an off-run
// `scope:"user"` op operates on the SAME user-scope tree/documents an agent run
// for this principal uses — runs derive their user_id from principal.Subject
// (auth.applyPrincipal), so without this, externally-created user-scope data
// would be invisible to the user's agents (and differ between the HTTP/gRPC
// clients). Tenant + the synthetic agent identity are unchanged (agent-scope
// stays operator-private out-of-band). Open mode / no principal / empty subject
// → falls back to the synthetic id (single-user behaviour preserved).
func substrateAdminUserCtx(ctx context.Context) context.Context {
	base := substrateAdminCtx(ctx)
	principal, ok := auth.PrincipalFromContext(ctx)
	if !ok || principal.Subject == "" {
		return base
	}
	return tools.WithRunIdentity(base, tools.RunIdentityValue{
		UserID:   principal.Subject,
		AgentID:  substrateAdminAgentID,
		TenantID: principal.TenantID,
	})
}

// substrateBrowseCtxFn returns the scope ctx builder for the off-run Path +
// Document BROWSE endpoints (RFC AS). It extends substrateAdminUserCtx with an
// optional caller-chosen subject (?scope_id=) and tenant (?tenant=) so an
// operator can inspect a tree/document owned by a subject OTHER than its own —
// the fix for "a doc an MCP agent created under its own subject is invisible to
// every human login". Authz mirrors principalTenantScope:
//
//   - admin / legacy / open: may browse ANY (tenant, scope_id) — sees every
//     primitive across the deployment.
//   - substrate:tenant: the tenant is FORCED to the principal's own (a tenant
//     can't read another tenant); scope_id may be any subject WITHIN that tenant
//     (the whole-tenant model — subjects share their tenant's workspace).
//
// scope_id overrides BOTH the user-scope id (RunIdentity.UserID) and the
// agent-scope id (agent name), so it selects the right tree whichever `scope`
// the request asks for. With neither param set the behaviour is byte-identical
// to substrateAdminUserCtx (the caller's own subject), so existing clients are
// unaffected.
func (s *Server) substrateBrowseCtxFn(r *http.Request) func(context.Context) context.Context {
	reqScopeID := strings.TrimSpace(r.URL.Query().Get("scope_id"))
	reqTenant := strings.TrimSpace(r.URL.Query().Get("tenant"))
	return func(ctx context.Context) context.Context {
		base := substrateAdminUserCtx(ctx)
		if reqScopeID == "" && reqTenant == "" {
			return base // no override → caller's own subject (back-compat)
		}
		p, ok := auth.PrincipalFromContext(ctx)
		// Open mode (no principal) is unrestricted (is_admin-equivalent), like
		// handleWhoami/principalTenantScope — honor both overrides.
		admin := !ok || p.Legacy || auth.HasScope(p.Scopes, auth.ScopeAdmin)

		tenantID := ""
		subject := ""
		if ok {
			tenantID = p.TenantID
			subject = p.Subject
		}
		if admin {
			if reqTenant != "" {
				tenantID = reqTenant
			}
			if reqScopeID != "" {
				subject = reqScopeID
			}
		} else {
			// substrate:tenant — tenant stays the principal's own (reqTenant
			// IGNORED, never widens); scope_id may target any subject in it.
			if reqScopeID != "" {
				subject = reqScopeID
			}
		}
		// Override BOTH the user id and the agent name so resolveScope picks the
		// requested tree for scope=user OR scope=agent.
		return tools.WithAgentName(
			tools.WithRunIdentity(base, tools.RunIdentityValue{
				UserID:   subject,
				AgentID:  substrateAdminAgentID,
				TenantID: tenantID,
			}),
			subject,
		)
	}
}

// handleSubstrateOperatorTokenDef serves POST /v1/_operatortokendef.
// RFC L OSS multi-tenant authorization. Bearer-authed operator admin.
func (s *Server) handleSubstrateOperatorTokenDef(w http.ResponseWriter, r *http.Request) {
	s.dispatchSubstrate(w, r, "OperatorTokenDef", s.OperatorTokenDef)
}
