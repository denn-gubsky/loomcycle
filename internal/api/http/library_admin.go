// library_admin.go — v0.9.x Introspection: bearer-authed read-only
// enumeration of every name declared in each substrate (AgentDef,
// SkillDef, MCPServerDef). Drives the Web UI's `/ui/library` tab.
//
// These three handlers are the missing complement to the existing
// `POST /v1/_*` op-dispatched substrate endpoints. The op-dispatch
// endpoints can list VERSIONS of a single name (`{op:"list", name:X}`),
// but they cannot enumerate the set of declared names. The store
// already exposes `*ListNames` helpers for snapshot + admin code
// paths; these handlers wire those to bearer-authed GET endpoints.
//
// Read-only + bearer-authed. The same `authMiddleware` guards every
// `/v1/_*` route — operator trust posture is identical to the
// substrate write endpoints.
package http

import (
	"encoding/json"
	"net/http"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// handleListAgentDefNames serves GET /v1/_agentdef/names.
// Returns `{names: [AgentDefNameSummary, ...]}` ordered by name ASC.
// Empty store returns `{names: []}` — never `null` — so adapter
// consumers can `.length` without a null-check.
func (s *Server) handleListAgentDefNames(w http.ResponseWriter, r *http.Request) {
	rows, err := s.store.AgentDefListNames(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	if rows == nil {
		rows = []store.AgentDefNameSummary{}
	}
	writeJSONOK(w, map[string]any{"names": rows})
}

// handleListSkillDefNames serves GET /v1/_skilldef/names.
func (s *Server) handleListSkillDefNames(w http.ResponseWriter, r *http.Request) {
	rows, err := s.store.SkillDefListNames(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	if rows == nil {
		rows = []store.SkillDefNameSummary{}
	}
	writeJSONOK(w, map[string]any{"names": rows})
}

// handleListMCPServerDefNames serves GET /v1/_mcpserverdef/names.
func (s *Server) handleListMCPServerDefNames(w http.ResponseWriter, r *http.Request) {
	rows, err := s.store.MCPServerDefListNames(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	if rows == nil {
		rows = []store.MCPServerDefNameSummary{}
	}
	writeJSONOK(w, map[string]any{"names": rows})
}

// handleListScheduleDefNames serves GET /v1/_scheduledef/names.
func (s *Server) handleListScheduleDefNames(w http.ResponseWriter, r *http.Request) {
	rows, err := s.store.ScheduleDefListNames(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	if rows == nil {
		rows = []store.ScheduleDefNameSummary{}
	}
	writeJSONOK(w, map[string]any{"names": rows})
}

// handleAgentChannels serves GET /v1/agents/{agent_name}/channels.
// Returns every channel_cursors row for (scope=agent, scope_id={agent_name}),
// ordered by channel ASC. Drives the v0.9.x Web UI's per-agent
// "channels this agent is subscribed to" sub-tab.
func (s *Server) handleAgentChannels(w http.ResponseWriter, r *http.Request) {
	agentName := r.PathValue("agent_name")
	if !validIdent(agentName) {
		writeJSONError(w, http.StatusBadRequest, "invalid_agent_name", `agent_name must match [A-Za-z0-9_-]{1,128}`)
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
