// library_admin.go — v0.9.x Introspection: bearer-authed read-only
// enumeration of every name declared in each substrate (AgentDef,
// SkillDef, MCPServerDef, …). Drives the Web UI's `/ui/library` tab.
//
// These handlers are the missing complement to the existing
// `POST /v1/_*` op-dispatched substrate endpoints. The op-dispatch
// endpoints can list VERSIONS of a single name (`{op:"list", name:X}`),
// but they cannot enumerate the set of declared names. The store
// already exposes `*ListNames` helpers for snapshot + admin code
// paths; these handlers wire those to bearer-authed GET endpoints.
//
// RFC AS Phase 1: the `*ListNames` store methods are tenant-BLIND (they
// return every tenant's rows), so each handler scopes the result to the
// authenticated principal via scopeNames — admin / legacy / open see all
// (honoring the ?tenant= focus), a substrate:tenant principal sees only its
// own tenant's rows. This closes the cross-tenant name leak (any authenticated
// token could previously enumerate every tenant's def names).
//
// Read-only + bearer-authed. The same `authMiddleware` guards every
// `/v1/_*` route — operator trust posture is identical to the
// substrate write endpoints.
package http

import (
	"encoding/json"
	"net/http"

	"github.com/denn-gubsky/loomcycle/internal/agents"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// scopeNames filters a tenant-blind *DefListNames result to the caller's tenant
// scope (RFC AS Phase 1). `all` (admin / legacy / open) returns the rows
// unchanged (never nil — adapter consumers `.length` without a null-check). A
// substrate:tenant principal keeps only rows whose owning tenant matches;
// tenantOf extracts that tenant from a row.
func scopeNames[T any](rows []T, all bool, tenantID string, tenantOf func(T) string) []T {
	if all {
		if rows == nil {
			return []T{}
		}
		return rows
	}
	out := make([]T, 0, len(rows))
	for _, row := range rows {
		if tenantOf(row) == tenantID {
			out = append(out, row)
		}
	}
	return out
}

// handleListAgentDefNames serves GET /v1/_agentdef/names.
// Returns `{names: [AgentDefNameSummary, ...]}` ordered by name ASC,
// scoped to the caller's tenant (RFC AS Phase 1).
func (s *Server) handleListAgentDefNames(w http.ResponseWriter, r *http.Request) {
	rows, err := s.store.AgentDefListNames(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	tenantID, all := s.principalTenantScope(r.Context(), r.URL.Query().Get("tenant"))
	rows = scopeNames(rows, all, tenantID, func(x store.AgentDefNameSummary) string { return x.TenantID })
	writeJSONOK(w, map[string]any{"names": rows})
}

// handleListSkillDefNames serves GET /v1/_skilldef/names.
func (s *Server) handleListSkillDefNames(w http.ResponseWriter, r *http.Request) {
	rows, err := s.store.SkillDefListNames(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	tenantID, all := s.principalTenantScope(r.Context(), r.URL.Query().Get("tenant"))
	rows = scopeNames(rows, all, tenantID, func(x store.SkillDefNameSummary) string { return x.TenantID })
	writeJSONOK(w, map[string]any{"names": rows})
}

// handleListMCPServerDefNames serves GET /v1/_mcpserverdef/names.
func (s *Server) handleListMCPServerDefNames(w http.ResponseWriter, r *http.Request) {
	rows, err := s.store.MCPServerDefListNames(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	tenantID, all := s.principalTenantScope(r.Context(), r.URL.Query().Get("tenant"))
	rows = scopeNames(rows, all, tenantID, func(x store.MCPServerDefNameSummary) string { return x.TenantID })
	writeJSONOK(w, map[string]any{"names": rows})
}

// handleListScheduleDefNames serves GET /v1/_scheduledef/names.
func (s *Server) handleListScheduleDefNames(w http.ResponseWriter, r *http.Request) {
	rows, err := s.store.ScheduleDefListNames(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	tenantID, all := s.principalTenantScope(r.Context(), r.URL.Query().Get("tenant"))
	rows = scopeNames(rows, all, tenantID, func(x store.ScheduleDefNameSummary) string { return x.TenantID })
	writeJSONOK(w, map[string]any{"names": rows})
}

// handleListA2AServerCardDefNames serves GET /v1/_a2aservercarddef/names.
func (s *Server) handleListA2AServerCardDefNames(w http.ResponseWriter, r *http.Request) {
	rows, err := s.store.A2AServerCardDefListNames(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	tenantID, all := s.principalTenantScope(r.Context(), r.URL.Query().Get("tenant"))
	rows = scopeNames(rows, all, tenantID, func(x store.A2AServerCardDefNameSummary) string { return x.TenantID })
	writeJSONOK(w, map[string]any{"names": rows})
}

// handleListA2AAgentDefNames serves GET /v1/_a2aagentdef/names.
func (s *Server) handleListA2AAgentDefNames(w http.ResponseWriter, r *http.Request) {
	rows, err := s.store.A2AAgentDefListNames(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	tenantID, all := s.principalTenantScope(r.Context(), r.URL.Query().Get("tenant"))
	rows = scopeNames(rows, all, tenantID, func(x store.A2AAgentDefNameSummary) string { return x.TenantID })
	writeJSONOK(w, map[string]any{"names": rows})
}

// handleListWebhookDefNames serves GET /v1/_webhookdef/names.
func (s *Server) handleListWebhookDefNames(w http.ResponseWriter, r *http.Request) {
	rows, err := s.store.WebhookDefListNames(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	tenantID, all := s.principalTenantScope(r.Context(), r.URL.Query().Get("tenant"))
	rows = scopeNames(rows, all, tenantID, func(x store.WebhookDefNameSummary) string { return x.TenantID })
	writeJSONOK(w, map[string]any{"names": rows})
}

// handleListMemoryBackendDefNames serves GET /v1/_memorybackenddef/names.
// RFC I MR-3a / mirrors handleListWebhookDefNames.
func (s *Server) handleListMemoryBackendDefNames(w http.ResponseWriter, r *http.Request) {
	rows, err := s.store.MemoryBackendDefListNames(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	tenantID, all := s.principalTenantScope(r.Context(), r.URL.Query().Get("tenant"))
	rows = scopeNames(rows, all, tenantID, func(x store.MemoryBackendDefNameSummary) string { return x.TenantID })
	writeJSONOK(w, map[string]any{"names": rows})
}

// handleListOperatorTokenDefNames serves GET /v1/_operatortokendef/names.
// RFC L. Returns one summary per token name — NO secret material. Tenant-scoped
// (RFC AS Phase 1): a substrate:tenant principal sees only its own tenant's
// token names, never another tenant's; admin sees all.
func (s *Server) handleListOperatorTokenDefNames(w http.ResponseWriter, r *http.Request) {
	rows, err := s.store.OperatorTokenDefListNames(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	tenantID, all := s.principalTenantScope(r.Context(), r.URL.Query().Get("tenant"))
	rows = scopeNames(rows, all, tenantID, func(x store.OperatorTokenDefNameSummary) string { return x.TenantID })
	writeJSONOK(w, map[string]any{"names": rows})
}

// handleAgentChannels serves GET /v1/agents/{agent_name}/channels.
// Returns every channel_cursors row for (scope=agent, scope_id={agent_name}),
// ordered by channel ASC. Drives the v0.9.x Web UI's per-agent
// "channels this agent is subscribed to" sub-tab.
func (s *Server) handleAgentChannels(w http.ResponseWriter, r *http.Request) {
	// RFC BA agent grouping: names may be `/`-grouped (doc/manager). The Web UI
	// percent-encodes the name (encodeURIComponent → doc%2Fmanager), which Go's
	// ServeMux keeps within a single path segment and PathValue decodes back to
	// "doc/manager" — so the path form carries a grouped name without a separate
	// route. Validate with the same `/`-aware grammar the rest of the agent
	// surface uses.
	agentName := r.PathValue("agent_name")
	if err := agents.ValidateName(agentName); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_agent_name", err.Error())
		return
	}
	rows, err := s.store.ChannelListCursorsForScope(r.Context(), store.MemoryScopeAgent, agentName)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	writeJSONOK(w, map[string]any{"channels": rows})
}

// writeJSONOK is a one-line helper for read-only 200 responses.
func writeJSONOK(w http.ResponseWriter, body any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}
