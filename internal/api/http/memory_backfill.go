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
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
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
	// SkippedEmpty counts rows with NO text to embed — a document root or a
	// section heading legitimately has an empty body. They are neither embedded
	// nor failed, and they PERMANENTLY remain candidates, which is why they have
	// to be reported: without this the operator sees `candidates` stop falling
	// with `embedded` at 0 and no stated reason.
	SkippedEmpty int `json:"skipped_empty,omitempty"`
	// More reports that the caller's limit was reached with candidates still
	// outstanding, so another call has work to do. It is the honest replacement for
	// "re-invoke until candidates reaches 0", which unembeddable rows make
	// unreachable.
	More bool `json:"more,omitempty"`
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
	if adminMemoryScopeIDRequired(scope) && scopeID == "" {
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
	tenant, all := s.principalTenantScope(r.Context(), r.URL.Query().Get("tenant"))
	// Same refusal as the erasure, directory and orphan-repair surfaces, for the same
	// reason: an admin naming no tenant resolves to the DEFAULT tenant, and memory
	// rows are keyed on the raw tenant — so the sweep would report a truthful-looking
	// "candidates: 0" against a tenant the operator never meant and leave the intended
	// one untouched. Verified live on a three-tenant deployment.
	if all && !r.URL.Query().Has("tenant") {
		writeJSONError(w, http.StatusBadRequest, "tenant_required",
			"an admin token must name the tenant: memory rows are keyed on it, so omitting "+
				"it silently targets the default tenant and reports 0 candidates. "+
				"Pass ?tenant=<id>, or ?tenant= for the default tenant.")
		return
	}

	// tenant scope backfills the single tenant-wide keyspace (store scope_id "");
	// the placeholder is dropped. Reused across the paging reads + the write-back.
	storeScopeID := adminMemoryStoreScopeID(scope, scopeID)
	rows, err := s.store.MemoryEmbedListMissing(r.Context(), tenant,
		store.MemoryScope(scope), storeScopeID, prefix, "", limit)
	if err != nil {
		// ErrVectorUnsupported / the vec-tier pending error are the honest answers
		// on a tier without this capability — surface them rather than reporting
		// "0 candidates", which would read as "nothing to do".
		writeJSONError(w, http.StatusServiceUnavailable, "backfill_unavailable", err.Error())
		return
	}

	resp := memoryBackfillResponse{
		Scope: scope, ScopeID: storeScopeID, Prefix: prefix, DryRun: dryRun,
		Candidates: len(rows),
		CurrentEmbedder: memoryReembedConfigured{
			Provider:  s.embedder.Provider(),
			Model:     s.embedder.Model(),
			Dimension: s.embedder.Dimension(),
		},
	}
	resp.Notes = []string{
		"candidates is bounded by limit — a non-zero count after a live run means MORE " +
			"remain. Every embedded row leaves the candidate set, so re-invoking is safe " +
			"and resumes rather than restarts.",
		"RE-INVOKE WHILE `more` IS TRUE. Do not wait for candidates to reach 0: a row " +
			"with nothing to embed — no body text AND no usable title — never gains an " +
			"embedding, so it stays a candidate permanently and candidates converges to " +
			"skipped_empty instead. `limit` bounds how many rows are EMBEDDED, not how " +
			"many are examined — the sweep pages past what it cannot embed.",
		"A document chunk with an empty body falls back to its TITLE, matching the " +
			"write path: a heading is real language and a real answer to a search.",
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

	// embedOne does the per-row work. A closure so the paging loop below stays about
	// paging.
	embedOne := func(row store.MemoryEntry, text string) {
		markFailed := func() {
			resp.Failed++
			if len(resp.FailedKeys) < backfillFailedKeyCap {
				resp.FailedKeys = append(resp.FailedKeys, row.Key)
			}
		}
		vecs, err := s.embedder.Embed(r.Context(), []string{text})
		if err != nil || len(vecs) == 0 {
			markFailed()
			return
		}
		if err := s.store.MemoryEmbedSet(r.Context(), tenant, store.MemoryScope(scope), storeScopeID, row.Key,
			store.MemoryEmbedding{
				Provider:  s.embedder.Provider(),
				Model:     s.embedder.Model(),
				Dimension: len(vecs[0]),
				Vector:    vecs[0],
				EmbedText: text,
				CreatedAt: time.Now().UTC(),
			}); err != nil {
			markFailed()
			return
		}
		resp.Embedded++
	}

	// PAGE PAST WHAT WE CANNOT EMBED. `limit` bounds how many rows get EMBEDDED, not
	// how many are looked at, and that difference is what makes the sweep finish.
	//
	// A row with no body text never gains an embedding, so it stays a candidate
	// forever. With a single fixed window those rows accumulate at the front of the
	// key order until they fill it, and throughput reaches ZERO with work still
	// outstanding — the residue obeys R' = R + p·(limit − R), whose fixed point is
	// R = limit. Measured live on a 3,143-chunk scope before this loop existed:
	// 189 / 179 / 169 / 160 / 147 embedded per 200-row window, heading for 0. The
	// keyset cursor steps over them.
	page := rows
	lastKey := ""
	for {
		for _, row := range page {
			// Advance the cursor for EVERY row seen, embedded or not — that is the
			// whole point.
			lastKey = row.Key
			// A document chunk body is a JSON envelope, so embedTextForRow unwraps it;
			// embedding the raw JSON would index the field names.
			text := embedTextForRow(row)
			if text == "" {
				// A document chunk with no body is usually a HEADING, and a heading is
				// real language ("RFC BE — History Tool", "Phase 2 — name-links").
				// The write path embeds the title in that case, so the sweep must too,
				// or existing documents stay permanently less searchable than new ones.
				//
				// The resolution lives in the builtin package: it needs the chunk
				// title (a SQL Memory read this handler has no view of) AND the
				// ""→"default" SQL-tenant rule, and restating that rule here is how
				// the tenant axis drifts.
				text = builtin.TitleFallbackForBodyKey(r.Context(), s.sqlMem, tenant,
					store.MemoryScope(scope), scopeID, row.Key)
			}
			if text == "" {
				// Nothing to embed, ever — no body and no usable title. Counted rather
				// than silently passed over.
				resp.SkippedEmpty++
				continue
			}
			embedOne(row, text)
			if resp.Embedded+resp.Failed >= limit {
				break
			}
		}
		if resp.Embedded+resp.Failed >= limit {
			// Hit the caller's budget. There may be more, so say so.
			resp.More = true
			break
		}
		if len(page) < limit {
			// A short page means the candidate set is exhausted — nothing to step to.
			break
		}
		var perr error
		page, perr = s.store.MemoryEmbedListMissing(r.Context(), tenant,
			store.MemoryScope(scope), storeScopeID, prefix, lastKey, limit)
		if perr != nil {
			resp.Notes = append(resp.Notes, "paging stopped early: "+perr.Error())
			resp.More = true
			break
		}
		if len(page) == 0 {
			break
		}
	}

	if resp.Failed > 0 {
		resp.Notes = append(resp.Notes,
			"some rows failed (see failed_keys, capped). They remain candidates, so a "+
				"re-invocation retries exactly those — nothing is lost by a partial failure.")
	}
	if resp.SkippedEmpty > 0 {
		resp.Notes = append(resp.Notes,
			"skipped_empty rows have neither body text nor a usable title (a title with no "+
				"letters carries no language). They stay candidates permanently — that is "+
				"expected, not a failure.")
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
// Ordinary rows keep the existing behaviour: their whole value is the text. So this
// narrows nothing and only improves the case the prefix targets.
//
// AND IT ROUTES THROUGH builtin.IndexableText, the same predicate the write path
// applies. Without that this sweep would happily CREATE the markdown-scaffolding
// embeddings ("```sh", "---") that embedBody rejects — the writer and the sweep
// disagreeing about what is worth indexing, which also left the purge unable to
// recognise them. One function, three callers.
func embedTextForRow(row store.MemoryEntry) string {
	var env struct {
		Body *string `json:"body"`
	}
	if err := json.Unmarshal(row.Value, &env); err == nil && env.Body != nil {
		return builtin.IndexableText(*env.Body)
	}
	return builtin.IndexableText(string(row.Value))
}
