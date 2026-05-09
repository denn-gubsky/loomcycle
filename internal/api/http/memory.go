package http

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// Memory admin endpoints — drive the v0.8.0 Web UI Memory page. All
// three are read-only operator views; the /v1/_memory namespace
// matches the existing /v1/_users / /v1/_resolver convention for
// admin / introspection routes.
//
// Wire shape mirrors the store types directly. Bearer auth is
// applied at the mux layer (recoveryMiddleware → authMiddleware).
//
// Routes:
//   GET /v1/_memory/scopes
//     → list scope kinds the operator can browse (constant set —
//       agent + user; forward-compatible for new scope kinds when
//       they ship).
//   GET /v1/_memory/scopes/{scope}
//     → list scope_ids under one scope, with summary stats.
//   GET /v1/_memory/scopes/{scope}/{scope_id}/keys?prefix=&limit=
//     → list keys + values for one (scope, scope_id) tuple.
//   GET /v1/_memory/scopes/{scope}/{scope_id}/keys/{key}
//     → one entry. (Used by the UI's deep-link / refresh path; the
//       list response already includes the value, so this is mostly
//       for direct API consumers.)

type memoryScopesResponse struct {
	Scopes []memoryScopeKind `json:"scopes"`
}

type memoryScopeKind struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

type memoryScopeIDsResponse struct {
	Scope    string                       `json:"scope"`
	ScopeIDs []store.MemoryScopeIDSummary `json:"scope_ids"`
}

type memoryEntriesResponse struct {
	Scope     string              `json:"scope"`
	ScopeID   string              `json:"scope_id"`
	Entries   []store.MemoryEntry `json:"entries"`
	Truncated bool                `json:"truncated"`
}

type memoryEntryResponse struct {
	Scope   string            `json:"scope"`
	ScopeID string            `json:"scope_id"`
	Entry   store.MemoryEntry `json:"entry"`
}

// handleListMemoryScopes serves GET /v1/_memory/scopes. Returns the
// operator-browsable scope kinds. v0.8.0 is `agent` + `user`; future
// versions extend this list.
func (s *Server) handleListMemoryScopes(w http.ResponseWriter, _ *http.Request) {
	resp := memoryScopesResponse{
		Scopes: []memoryScopeKind{
			{Name: "agent", Description: "Per-yaml-agent keyspace, shared across users"},
			{Name: "user", Description: "Per-user keyspace, shared across agents"},
		},
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// handleListMemoryScopeIDs serves GET /v1/_memory/scopes/{scope}.
func (s *Server) handleListMemoryScopeIDs(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "store_unavailable",
			"store not configured; Memory admin requires a persistent store")
		return
	}
	scope := r.PathValue("scope")
	if !validAdminMemoryScope(scope) {
		writeJSONError(w, http.StatusBadRequest, "invalid_scope",
			"scope must be one of: agent, user")
		return
	}
	rows, err := s.store.MemoryListScopeIDs(r.Context(), store.MemoryScope(scope))
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if rows == nil {
		// JSON-encode an empty array rather than null so the UI can
		// `.map` without a guard.
		rows = []store.MemoryScopeIDSummary{}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(memoryScopeIDsResponse{Scope: scope, ScopeIDs: rows})
}

// handleListMemoryEntries serves
// GET /v1/_memory/scopes/{scope}/{scope_id}/keys?prefix=&limit=.
func (s *Server) handleListMemoryEntries(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "store_unavailable",
			"store not configured; Memory admin requires a persistent store")
		return
	}
	scope := r.PathValue("scope")
	scopeID := r.PathValue("scope_id")
	if !validAdminMemoryScope(scope) {
		writeJSONError(w, http.StatusBadRequest, "invalid_scope",
			"scope must be one of: agent, user")
		return
	}
	if strings.TrimSpace(scopeID) == "" {
		writeJSONError(w, http.StatusBadRequest, "missing_scope_id", "scope_id is required")
		return
	}
	prefix := r.URL.Query().Get("prefix")
	limit := 200
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > 1000 {
		limit = 1000
	}
	entries, truncated, err := s.store.MemoryList(r.Context(), store.MemoryScope(scope), scopeID, prefix, limit)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if entries == nil {
		entries = []store.MemoryEntry{}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(memoryEntriesResponse{
		Scope:     scope,
		ScopeID:   scopeID,
		Entries:   entries,
		Truncated: truncated,
	})
}

// handleGetMemoryEntry serves
// GET /v1/_memory/scopes/{scope}/{scope_id}/keys/{key}.
func (s *Server) handleGetMemoryEntry(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "store_unavailable",
			"store not configured; Memory admin requires a persistent store")
		return
	}
	scope := r.PathValue("scope")
	scopeID := r.PathValue("scope_id")
	key := r.PathValue("key")
	if !validAdminMemoryScope(scope) {
		writeJSONError(w, http.StatusBadRequest, "invalid_scope",
			"scope must be one of: agent, user")
		return
	}
	if strings.TrimSpace(scopeID) == "" || strings.TrimSpace(key) == "" {
		writeJSONError(w, http.StatusBadRequest, "missing_path",
			"scope_id and key are required")
		return
	}
	entry, err := s.store.MemoryGet(r.Context(), store.MemoryScope(scope), scopeID, key)
	if err != nil {
		var nf *store.ErrNotFound
		if errors.As(err, &nf) {
			writeJSONError(w, http.StatusNotFound, "not_found",
				"no entry at "+scope+"/"+scopeID+"/"+key)
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(memoryEntryResponse{
		Scope:   scope,
		ScopeID: scopeID,
		Entry:   entry,
	})
}

// validAdminMemoryScope mirrors the closed scope set the runtime
// accepts. Distinct from the agent yaml allowlist (which is per-
// agent + caller-side); this is the admin-side gate so the UI can't
// poke a never-shipped scope into the store.
func validAdminMemoryScope(s string) bool {
	switch s {
	case "agent", "user":
		return true
	}
	return false
}
