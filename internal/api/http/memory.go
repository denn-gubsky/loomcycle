package http

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

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
	// EmbeddingMetadata is populated only when the request sets
	// ?include_embedding_metadata=true (RFC I MR-6 / Decision 4). It is a
	// per-key map (key → {provider, model, dimension}) so the /ui/memory
	// introspection view can show which rows are embedded + under which
	// model, without a second round-trip per row. Absent keys simply have
	// no entry (the row has no embedding).
	EmbeddingMetadata map[string]memoryEmbedMeta `json:"embedding_metadata,omitempty"`
}

// memoryEmbedMeta is the per-key embedding metadata surfaced to the
// introspection UI. It deliberately EXCLUDES the vector itself — operators
// triage shape (model/dimension), not raw float arrays.
type memoryEmbedMeta struct {
	Provider  string `json:"provider"`
	Model     string `json:"model"`
	Dimension int    `json:"dimension"`
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
	rows, err := s.store.MemoryListScopeIDs(r.Context(), "", store.MemoryScope(scope))
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
	entries, truncated, err := s.store.MemoryList(r.Context(), "", store.MemoryScope(scope), scopeID, prefix, limit)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if entries == nil {
		entries = []store.MemoryEntry{}
	}
	resp := memoryEntriesResponse{
		Scope:     scope,
		ScopeID:   scopeID,
		Entries:   entries,
		Truncated: truncated,
	}
	// RFC I MR-6: optional per-key embedding metadata for the /ui/memory
	// introspection view. Best-effort + non-fatal — a backend without
	// vectors (SupportsVectors()==false) or a per-key lookup error simply
	// omits that key's metadata rather than failing the list. The vector
	// itself is never included (MemoryEmbedGet's Vector is dropped here).
	if r.URL.Query().Get("include_embedding_metadata") == "true" && s.store.SupportsVectors() {
		meta := make(map[string]memoryEmbedMeta)
		for _, e := range entries {
			emb, err := s.store.MemoryEmbedGet(r.Context(), "", store.MemoryScope(scope), scopeID, e.Key)
			if err != nil || emb.Dimension == 0 {
				continue // no embedding for this key (or a transient error) → omit
			}
			meta[e.Key] = memoryEmbedMeta{
				Provider:  emb.Provider,
				Model:     emb.Model,
				Dimension: emb.Dimension,
			}
		}
		resp.EmbeddingMetadata = meta
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
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
	entry, err := s.store.MemoryGet(r.Context(), "", store.MemoryScope(scope), scopeID, key)
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

// memoryEntryPutBody is the wire shape for PUT
// /v1/_memory/scopes/{scope}/{scope_id}/keys/{key}. Value is opaque
// JSON; the optional `embed` flag (also accepted as ?embed=true
// query param) triggers a synchronous embed on the configured
// embedder. ttl_seconds > 0 sets an expiry on the row; <=0 stores
// without expiry.
type memoryEntryPutBody struct {
	Value      json.RawMessage `json:"value"`
	Embed      bool            `json:"embed,omitempty"`
	TTLSeconds int             `json:"ttl_seconds,omitempty"`
}

// memoryEntryPutResponse mirrors the in-band Memory tool's set ack
// so HTTP callers see a stable shape. Echoes the embed result for
// callers that opted in (or `null` when not requested).
type memoryEntryPutResponse struct {
	Scope        string `json:"scope"`
	ScopeID      string `json:"scope_id"`
	Key          string `json:"key"`
	Embedded     bool   `json:"embedded"`
	EmbedWarning string `json:"embed_warning,omitempty"`
}

// handlePutMemoryEntry serves
// PUT /v1/_memory/scopes/{scope}/{scope_id}/keys/{key}. Idempotent
// upsert by full (scope, scope_id, key) triple. Bearer-authed.
func (s *Server) handlePutMemoryEntry(w http.ResponseWriter, r *http.Request) {
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
	var body memoryEntryPutBody
	// Bound the body to the configured per-value limit (default 64 KiB)
	// plus JSON-envelope headroom; 0 (values uncapped) falls back to a
	// generous wire ceiling so a deliberately-uncapped deployment still
	// can't be OOM'd by a single oversized request. The tool layer's
	// MaxValueBytes / quota check remains the authoritative limit.
	maxBody := int64(16 << 20)
	if v := s.cfg.Env.MemoryMaxValueBytes; v > 0 {
		maxBody = int64(v) + (1 << 16)
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_body",
			"invalid request body: "+err.Error())
		return
	}
	if len(body.Value) == 0 {
		writeJSONError(w, http.StatusBadRequest, "missing_value",
			"value is required")
		return
	}
	if !json.Valid(body.Value) {
		writeJSONError(w, http.StatusBadRequest, "invalid_value",
			"value must be valid JSON")
		return
	}
	embed := body.Embed
	if r.URL.Query().Get("embed") == "true" {
		embed = true
	}
	ttl := time.Duration(0)
	if body.TTLSeconds > 0 {
		ttl = time.Duration(body.TTLSeconds) * time.Second
	}
	if err := s.store.MemorySet(r.Context(), "", store.MemoryScope(scope), scopeID, key, body.Value, ttl); err != nil {
		if errors.Is(err, store.ErrMemoryQuotaExceeded) {
			writeJSONError(w, http.StatusRequestEntityTooLarge, "memory_quota_exceeded", err.Error())
			return
		}
		if errors.Is(err, store.ErrMemoryValueTooLarge) {
			writeJSONError(w, http.StatusRequestEntityTooLarge, "memory_value_too_large", err.Error())
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	resp := memoryEntryPutResponse{Scope: scope, ScopeID: scopeID, Key: key}
	if embed {
		// Best-effort: same surface as v0.9.0's Memory.set embed —
		// transient failures surface as embedded:false + warning, the
		// k/v row stays.
		if err := s.embedMemoryEntry(r.Context(), store.MemoryScope(scope), scopeID, key, body.Value); err != nil {
			resp.EmbedWarning = err.Error()
		} else {
			resp.Embedded = true
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// handleDeleteMemoryEntry serves
// DELETE /v1/_memory/scopes/{scope}/{scope_id}/keys/{key}. 204 on
// success regardless of whether the row existed (idempotent delete
// matches the in-band Memory tool's semantics).
func (s *Server) handleDeleteMemoryEntry(w http.ResponseWriter, r *http.Request) {
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
	if _, err := s.store.MemoryDelete(r.Context(), "", store.MemoryScope(scope), scopeID, key); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// embedMemoryEntry mirrors the in-band Memory.set embed path —
// computes an embedding from the (already-stored) value and writes
// it via MemoryEmbedUpsert. No-ops gracefully when the embedder is
// not wired or the store doesn't support vectors.
func (s *Server) embedMemoryEntry(ctx context.Context, scope store.MemoryScope, scopeID, key string, value json.RawMessage) error {
	if s.embedder == nil {
		return errEmbedderUnconfigured
	}
	if !s.store.SupportsVectors() {
		return errStoreVectorsUnsupported
	}
	// Embed the JSON-as-string. Mirrors the same input shape the in-
	// band tool uses for the default-no-embed_text path.
	text := string(value)
	vecs, err := s.embedder.Embed(ctx, []string{text})
	if err != nil {
		return err
	}
	if len(vecs) != 1 {
		return errors.New("embedder returned unexpected vector count")
	}
	return s.store.MemoryEmbedSet(ctx, "", scope, scopeID, key, store.MemoryEmbedding{
		Provider:  s.embedder.Provider(),
		Model:     s.embedder.Model(),
		Dimension: s.embedder.Dimension(),
		Vector:    vecs[0],
		EmbedText: text,
		CreatedAt: time.Now().UTC(),
	})
}

var (
	errEmbedderUnconfigured    = errors.New("embedder not configured on this loomcycle instance")
	errStoreVectorsUnsupported = errors.New("store does not support vectors (use pgvector or sqlite-vec build)")
)
