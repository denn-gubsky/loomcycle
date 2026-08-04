package http

// memory_backfill.go — POST /v1/_memory/backfill_embeddings (RFC BU phase 2).
//
// Embeds live memory rows that have NO embedding, optionally under a key prefix.
// The motivating case is `doc.chunk:` — phase 1 embeds bodies on write, so a
// deployment's existing documents stay unsearchable until they are swept once.
//
// WHY THIS IS NOT THE EXISTING /v1/_memory/reembed. That endpoint walks rows whose
// stored embedder DIFFERS from the configured one, and its candidate query starts
// FROM memory_embeddings and INNER JOINs memory — a row with no embedding row is
// structurally invisible to it. Backfill is the inverse direction, so it needs its
// own query and gets its own route rather than a mode flag on a differently-shaped
// one.
//
// WHY NOT A MIGRATION. A boot-time sweep would make thousands of embedder calls
// while the runtime is trying to start, and on a metered embedder that is a
// surprise bill nobody approved. It is an operator action, invoked when they choose.

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

type memoryBackfillResponse struct {
	Scope   string `json:"scope"`
	ScopeID string `json:"scope_id"`
	Prefix  string `json:"prefix,omitempty"`
	DryRun  bool   `json:"dry_run"`

	// Candidates is how many unembedded rows this call SAW, bounded by limit.
	Candidates int `json:"candidates"`
	// Embedded / Failed describe a live run.
	Embedded int `json:"embedded,omitempty"`
	Failed   int `json:"failed,omitempty"`
	// FailedKeys is capped — a systematic failure (embedder down) would otherwise
	// return one line per row.
	FailedKeys []string `json:"failed_keys,omitempty"`
	// SampleKeys shows what a dry run would touch.
	SampleKeys []string `json:"sample_keys,omitempty"`

	CurrentEmbedder memoryReembedConfigured `json:"current_embedder"`
	Notes           []string                `json:"notes"`
}

const backfillFailedKeyCap = 20

// handleMemoryBackfillEmbeddings serves POST
// /v1/_memory/backfill_embeddings?scope=&scope_id=&prefix=&limit=&dry_run=
//
// RESUMABLE WITHOUT STATE. Every row embedded drops out of the candidate query, so
// a call that dies half-way leaves the rest still listed — an operator re-invokes
// until `candidates` reaches zero. There is no cursor to persist, and therefore no
// way for a crash to double-embed a row or skip one.
func (s *Server) handleMemoryBackfillEmbeddings(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "store_unavailable",
			"store not configured; backfill requires a persistent store")
		return
	}
	if s.embedder == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "embedder_not_configured",
			"no embedder configured; set memory.embedder in operator yaml")
		return
	}
	scope := r.URL.Query().Get("scope")
	scopeID := r.URL.Query().Get("scope_id")
	if !validAdminMemoryScope(scope) {
		writeJSONError(w, http.StatusBadRequest, "invalid_scope",
			"scope must be one of: agent, user, tenant")
		return
	}
	if scopeID == "" {
		writeJSONError(w, http.StatusBadRequest, "missing_scope_id", "scope_id is required")
		return
	}
	// dry_run defaults TRUE, matching /v1/_memory/reembed: an operator typing a
	// bare `curl -X POST` gets a preview, not thousands of embedder calls.
	dryRun := true
	if v := r.URL.Query().Get("dry_run"); v != "" {
		dryRun = v != "false" && v != "0"
	}
	limit := 200
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	prefix := r.URL.Query().Get("prefix")
	tenant, _ := s.principalTenantScope(r.Context(), r.URL.Query().Get("tenant"))

	rows, err := s.store.MemoryEmbedListMissing(r.Context(), tenant,
		store.MemoryScope(scope), scopeID, prefix, limit)
	if err != nil {
		// ErrVectorUnsupported / the vec-tier pending error are the honest answers
		// on a tier without this capability — surface them rather than reporting
		// "0 candidates", which would read as "nothing to do".
		writeJSONError(w, http.StatusServiceUnavailable, "backfill_unavailable", err.Error())
		return
	}

	resp := memoryBackfillResponse{
		Scope: scope, ScopeID: scopeID, Prefix: prefix, DryRun: dryRun,
		Candidates: len(rows),
		CurrentEmbedder: memoryReembedConfigured{
			Provider:  s.embedder.Provider(),
			Model:     s.embedder.Model(),
			Dimension: s.embedder.Dimension(),
		},
	}
	resp.Notes = []string{
		"candidates is bounded by limit — a non-zero count after a live run means " +
			"MORE remain; re-invoke until it reaches 0. Every embedded row leaves the " +
			"candidate set, so re-invoking is safe and resumes rather than restarts.",
	}

	if dryRun {
		for i, row := range rows {
			if i == backfillFailedKeyCap {
				break
			}
			resp.SampleKeys = append(resp.SampleKeys, row.Key)
		}
		resp.Notes = append(resp.Notes, "DRY RUN — nothing was embedded. Re-send with dry_run=false.")
		writeJSON(w, http.StatusOK, resp)
		return
	}

	for _, row := range rows {
		// Embed the row's VALUE text. A document chunk body is a JSON envelope, so
		// embedText unwraps it — embedding the raw JSON would index the field names.
		text := embedTextForRow(row)
		if text == "" {
			continue
		}
		vecs, err := s.embedder.Embed(r.Context(), []string{text})
		if err != nil || len(vecs) == 0 {
			resp.Failed++
			if len(resp.FailedKeys) < backfillFailedKeyCap {
				resp.FailedKeys = append(resp.FailedKeys, row.Key)
			}
			continue
		}
		if err := s.store.MemoryEmbedSet(r.Context(), tenant, store.MemoryScope(scope), scopeID, row.Key,
			store.MemoryEmbedding{
				Provider:  s.embedder.Provider(),
				Model:     s.embedder.Model(),
				Dimension: len(vecs[0]),
				Vector:    vecs[0],
				EmbedText: text,
				CreatedAt: time.Now().UTC(),
			}); err != nil {
			resp.Failed++
			if len(resp.FailedKeys) < backfillFailedKeyCap {
				resp.FailedKeys = append(resp.FailedKeys, row.Key)
			}
			continue
		}
		resp.Embedded++
	}
	if resp.Failed > 0 {
		resp.Notes = append(resp.Notes,
			"some rows failed (see failed_keys, capped). They remain candidates, so a "+
				"re-invocation retries exactly those — nothing is lost by a partial failure.")
	}
	writeJSON(w, http.StatusOK, resp)
}

// embedTextForRow extracts the text worth embedding from a stored row value.
//
// A document chunk body is a JSON envelope — {"body": "...", "fields": {...}} —
// so embedding row.Value verbatim (which is what /v1/_memory/reembed does) would
// index the literal tokens `body` and `fields` alongside the prose, and for a
// chunk with a short body the envelope could outweigh the content.
//
// Ordinary rows keep the existing behaviour: their whole value is the text. So
// this narrows nothing and only improves the case the prefix targets.
func embedTextForRow(row store.MemoryEntry) string {
	var env struct {
		Body *string `json:"body"`
	}
	if err := json.Unmarshal(row.Value, &env); err == nil && env.Body != nil {
		return strings.TrimSpace(*env.Body)
	}
	return strings.TrimSpace(string(row.Value))
}
