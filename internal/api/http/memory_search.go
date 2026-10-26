package http

// memory_search.go — POST /v1/_memory/search (RFC BV phase 1).
//
// An OFF-RUN unified semantic search over a scope's memory: it runs with an
// EMPTY key prefix so results span BOTH plain k/v entries AND document-chunk
// bodies (doc.chunk:<id>), which the memory view needs to answer "where did I
// record this" across the whole stack in one query. The in-band Memory tool's
// `search` op is prefix-scoped and run-bound; this is its off-run twin for the
// admin/operator surface, matching the HTTP-only posture of the sibling
// /v1/_memory/* endpoints. Admin-only in this phase (the /v1/_* catch-all gates
// it at ScopeAdmin); a later phase re-gates the family to the tenant axis.

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strings"

	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/memory/backends/inprocess"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// docChunkKeyPrefix is the Memory keyspace namespace the Document tool writes
// chunk bodies under (builtin.chunkBodyKey). A hit on such a key is a document
// chunk, not a plain memory entry — kept as a literal here to avoid importing
// internal/tools/builtin into the HTTP layer.
const docChunkKeyPrefix = "doc.chunk:"

type memorySearchRequest struct {
	Query   string               `json:"query"`
	Scope   string               `json:"scope"`
	ScopeID string               `json:"scope_id"`
	TopK    int                  `json:"top_k"`
	Rank    *memrank.RankConfig  `json:"rank"`
	Dedup   *memrank.DedupConfig `json:"dedup"`
	// Sources narrows which kinds of remembered thing come back (RFC BW). Empty
	// keeps this endpoint's purpose: it exists to answer "where did I record this"
	// across BOTH planes, so unlike the in-band recall it does not default to facts.
	Sources []string `json:"sources"`
	// When is the observed-time predicate, decoded by the SAME parser the in-band
	// tool uses. Two hand-maintained copies of a predicate that decides which rows
	// get DROPPED is how this endpoint and its in-band twin have already drifted
	// twice; one struct and one parser is the fix.
	When *memrank.WhenInput `json:"when,omitempty"`
	// UnderPaths … ExcludeDocumentIDs aim the search at part of the scope's
	// documents, exactly as on the in-run Memory tool's search: they narrow
	// document chunks only (facts and notes pass), before every leg's limit.
	// Resolved by the Document tool in the caller's own tenant and the scope_id
	// below, never in a tenant the body names.
	UnderPaths         []string `json:"under_paths,omitempty"`
	ExcludeUnderPaths  []string `json:"exclude_under_paths,omitempty"`
	DocumentIDs        []string `json:"document_ids,omitempty"`
	ExcludeDocumentIDs []string `json:"exclude_document_ids,omitempty"`
	// DocumentsScope also searches the DOCUMENTS of a second scope in the same
	// call, ranked with this scope's results into one list (the in-run Memory
	// tool's documents_scope). DocumentsScopeID names its subject, as ScopeID
	// names the first scope's: required for agent and user, ignored for tenant.
	// The tenant is the caller's own, never the body's.
	DocumentsScope   string `json:"documents_scope,omitempty"`
	DocumentsScopeID string `json:"documents_scope_id,omitempty"`
}

func (b memorySearchRequest) documentFilter() memrank.DocumentFilter {
	return memrank.DocumentFilter{
		UnderPaths:         b.UnderPaths,
		ExcludeUnderPaths:  b.ExcludeUnderPaths,
		DocumentIDs:        b.DocumentIDs,
		ExcludeDocumentIDs: b.ExcludeDocumentIDs,
	}
}

type memorySearchEmbeddedWith struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

type memorySearchResultEntry struct {
	Key          string                   `json:"key"`
	Value        json.RawMessage          `json:"value"`
	Score        float64                  `json:"score"`      // raw cosine similarity
	RankScore    float64                  `json:"rank_score"` // hybrid rank the row was ordered by
	EmbeddedWith memorySearchEmbeddedWith `json:"embedded_with"`
	// Kind is the row's class: "fact" (a consolidator distilled it), "note" (an agent
	// wrote it directly) or "document" (a Document chunk body). A document hit also
	// carries chunk_id so the viewer can fetch its entity block via get_chunk.
	//
	// WIRE CHANGE (RFC BW §7): this used to be "memory" | "document". A reader that
	// switches on "document" is unaffected; one asserting kind == "memory" must move to
	// fact/note. The refinement is the point — "the user told me this" and "an agent
	// jotted this down" are different claims, and collapsing them is what let document
	// prose read as remembered fact.
	Kind    string `json:"kind"`
	ChunkID string `json:"chunk_id,omitempty"`
	// Document and Title are the hit's READABLE identity — the document title and the
	// chunk's own heading — for document-kind rows only. Both are best-effort: they
	// live in SQL Memory, a different plane from the bodies, so a scope with no
	// document tables or a chunk deleted mid-search simply omits them. ChunkID
	// remains the address; these annotate it.
	Document string `json:"document,omitempty"`
	Title    string `json:"title,omitempty"`
	// MatchedUnit is the Document derived search unit that found this chunk, when
	// one did (the chunk appears once however many of its units matched) — the same
	// field the in-run Memory and Document searches carry.
	MatchedUnit *memrank.MatchedUnit `json:"matched_unit,omitempty"`
	// Scope is the scope the hit came from — on every entry when the request
	// searched a second scope's documents, and absent otherwise.
	Scope string `json:"scope,omitempty"`
}

type memorySearchResponse struct {
	// TimeFilter is omitted unless the request carried a window, so an existing
	// caller sees an unchanged response.
	TimeFilter        *memrank.TimeFilterReport `json:"time_filter,omitempty"`
	Scope             string                    `json:"scope"`
	ScopeID           string                    `json:"scope_id"`
	Entries           []memorySearchResultEntry `json:"entries"`
	QueryEmbeddingDim int                       `json:"query_embedding_dim"`
	Truncated         bool                      `json:"truncated"`
	// Filter is what a document filter resolved to — present only when the
	// request carried one, so a filter-free response is byte-identical.
	Filter *builtin.MemoryDocFilterEcho `json:"filter,omitempty"`
	// DocumentsScope / DocumentsScopeID echo the second scope searched for
	// documents and its store scope id ("" for tenant) — present only when one
	// was, so a one-scope response is byte-identical.
	DocumentsScope   string  `json:"documents_scope,omitempty"`
	DocumentsScopeID *string `json:"documents_scope_id,omitempty"`
}

// handleMemorySearch serves POST /v1/_memory/search.
func (s *Server) handleMemorySearch(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "store_unavailable",
			"store not configured; Memory admin requires a persistent store")
		return
	}

	// A query string is small; cap the body so an off-run caller can't stream a
	// large payload into an admin endpoint.
	var body memorySearchRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_body",
			"request body must be valid JSON: "+err.Error())
		return
	}
	if strings.TrimSpace(body.Query) == "" {
		writeJSONError(w, http.StatusBadRequest, "missing_query", "query is required")
		return
	}
	if !validAdminMemoryScope(body.Scope) {
		writeJSONError(w, http.StatusBadRequest, "invalid_scope", "scope must be one of: agent, user, tenant")
		return
	}
	if adminMemoryScopeIDRequired(body.Scope) && strings.TrimSpace(body.ScopeID) == "" {
		writeJSONError(w, http.StatusBadRequest, "missing_scope_id", "scope_id is required")
		return
	}
	// tenant scope searches the single tenant-wide keyspace (store scope_id "");
	// any scope_id the caller sent is a placeholder and is dropped.
	storeScopeID := adminMemoryStoreScopeID(body.Scope, body.ScopeID)

	topK := body.TopK
	if topK <= 0 {
		topK = 10
	}
	if topK > 50 {
		topK = 50
	}
	rankCfg := memrank.DefaultRankConfig()
	if body.Rank != nil {
		rankCfg = *body.Rank
	}
	var dedupCfg memrank.DedupConfig
	if body.Dedup != nil {
		dedupCfg = *body.Dedup
	}

	// TENANT STAMPING — security-critical. The in-process backend resolves the
	// tenant from tools.RunIdentity(ctx).TenantID ONLY (backends/inprocess.runTenant).
	// Off-run there is no RunIdentity, so without stamping the authenticated
	// principal's tenant the search would run at the shared "" tenant and could
	// read/return another tenant's rows. tenantFromCtx is caller/config-
	// authoritative — never a request-body field.
	ctx := tools.WithRunIdentity(r.Context(), tools.RunIdentityValue{TenantID: tenantFromCtx(r.Context())})

	// Empty Prefix so the search spans both k/v entries and doc.chunk bodies unless the
	// caller narrows it with sources.
	backend := inprocess.New(s.store, s.embedder)
	when, werr := memrank.ParseWhen(body.When)
	if werr != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_when", werr.Error())
		return
	}
	sources, serr := memrank.ParseSources(body.Sources)
	if serr != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_sources", serr.Error())
		return
	}
	// documents_scope equal to scope (same subject) is documents added to
	// sources and nothing else; a different one is planned below, after this
	// scope's own filter.
	legScope, legStoreScopeID, sources, dsErr := memorySearchDocumentsScope(body, storeScopeID, sources)
	if dsErr != nil {
		writeJSONError(w, http.StatusBadRequest, dsErr.code, dsErr.msg)
		return
	}
	sq := memrank.SearchQuery{
		QueryText: body.Query, Prefix: "", TopK: topK,
		Sources: sources,
		When:    when,
	}
	// A document filter is resolved to chunk ids BEFORE the search, and only when
	// one was given: a filter-free search stays on exactly the old path.
	var filterEcho *builtin.MemoryDocFilterEcho
	docCtx := memorySearchFilterCtx(r, body.Scope, storeScopeID)
	docs := &builtin.Document{Store: s.store, SqlMem: s.sqlMem}
	// With a second scope, the document filter aims at its documents instead.
	if df := body.documentFilter(); !df.IsZero() && legScope == "" {
		p, refusal := builtin.PlanMemoryDocumentFilter(docCtx, docs, "search",
			store.MemoryScope(body.Scope), storeScopeID, df, sq, builtin.MemorySourcesWhy("", sources))
		if refusal != nil {
			status, code := memorySearchFilterRefusalStatus(*refusal)
			if status == http.StatusInternalServerError {
				slog.Warn("memory search document filter failed", "scope", body.Scope, "scope_id", body.ScopeID, "err", refusal.Text)
				writeJSONError(w, status, code, "search failed")
				return
			}
			writeJSONError(w, status, code, refusal.Text)
			return
		}
		echo := p.Echo()
		filterEcho = &echo
		// Nothing matched and nothing but documents could: answer without
		// embedding the query or touching the store.
		if p.NothingToSearch() {
			writeJSON(w, http.StatusOK, memorySearchResponse{
				Scope: body.Scope, ScopeID: storeScopeID,
				Entries: []memorySearchResultEntry{}, Filter: filterEcho,
			})
			return
		}
		p.Apply(&sq)
	}
	// The second scope's documents, through the Document tool's gate for that
	// scope under the caller's identity — the same gate, grants and subject
	// rules the first scope's filter and /v1/_document use — so this reaches
	// no document the caller could not search there. A refusal refuses the
	// whole call; it never answers from one scope.
	var legCtx context.Context
	var legPlan *builtin.DocumentsLegPlan
	if legScope != "" {
		legCtx = memorySearchFilterCtx(r, string(legScope), legStoreScopeID)
		plan, refusal := builtin.PlanDocumentsLeg(legCtx, docs, "search", legScope, legStoreScopeID, body.documentFilter())
		if refusal != nil {
			status, code := memorySearchFilterRefusalStatus(*refusal)
			if status == http.StatusInternalServerError {
				slog.Warn("memory search documents_scope failed", "documents_scope", legScope, "documents_scope_id", body.DocumentsScopeID, "err", refusal.Text)
				writeJSONError(w, status, code, "search failed")
				return
			}
			writeJSONError(w, status, code, refusal.Text)
			return
		}
		legPlan, sq.Documents = plan, plan.Leg()
		filterEcho = plan.FilterEcho()
	}
	res, err := backend.Search(ctx, store.MemoryScope(body.Scope), storeScopeID, sq, rankCfg, dedupCfg)
	if err != nil {
		// The three typed refusals are operator-actionable (no embedder / no
		// vector index / a model swap left the stored dimension stale), so surface
		// them verbatim as a 400 rather than a blind 500.
		if errors.Is(err, memrank.ErrSourcesNotExpressible) ||
			errors.Is(err, memrank.ErrTracesNotCombinable) {
			writeJSONError(w, http.StatusBadRequest, "invalid_sources", err.Error())
			return
		}
		if errors.Is(err, store.ErrDimensionMismatch) ||
			errors.Is(err, store.ErrVectorUnsupported) ||
			errors.Is(err, store.ErrEmbedderNotConfigured) {
			writeJSONError(w, http.StatusBadRequest, "search_unavailable", err.Error())
			return
		}
		slog.Warn("memory search failed", "scope", body.Scope, "scope_id", body.ScopeID, "err", err)
		writeJSONError(w, http.StatusInternalServerError, "internal", "search failed")
		return
	}
	// A document the caller's access-control set denies it reading is left out,
	// as Document op=search and the Memory tool in a run leave it out: chunk
	// bodies share this keyspace, so without it this search is a second door to
	// them. Checked by key, so a fact homed in a denied chunk and a chunk found
	// through its derived unit go too. A caller whose set denies no document
	// gets the old answer with no lookup. A failed check fails the search; the
	// hits are never returned unchecked.
	// With a second scope each scope's hits are checked in that scope, under
	// the identity its documents resolved with.
	if err := builtin.DropUnreadableSearchHits(docCtx, docs, store.MemoryScope(body.Scope), storeScopeID, &res,
		hitsFrom(legPlan, res.Scopes, store.MemoryScope(body.Scope), store.MemoryScope(body.Scope))); err != nil {
		slog.Error("memory search access check failed", "scope", body.Scope, "scope_id", body.ScopeID, "err", err)
		writeJSONError(w, http.StatusInternalServerError, "internal", "search failed")
		return
	}
	if legPlan != nil {
		if err := builtin.DropUnreadableSearchHits(legCtx, docs, legScope, legStoreScopeID, &res,
			hitsFrom(legPlan, res.Scopes, store.MemoryScope(body.Scope), legScope)); err != nil {
			slog.Error("memory search access check failed", "documents_scope", legScope, "documents_scope_id", body.DocumentsScopeID, "err", err)
			writeJSONError(w, http.StatusInternalServerError, "internal", "search failed")
			return
		}
	}

	entries := make([]memorySearchResultEntry, 0, len(res.Entries))

	// Collect the document hits' chunk ids up front so their labels resolve in ONE
	// batched query rather than one per row.
	var docChunkIDs, legChunkIDs []string
	for i, e := range res.Entries {
		if memrank.Class(e) == store.MemoryRowDocument {
			id := strings.TrimPrefix(e.Key, docChunkKeyPrefix)
			if legPlan != nil && hitScope(res.Scopes, i, store.MemoryScope(body.Scope)) == legScope {
				legChunkIDs = append(legChunkIDs, id)
			} else {
				docChunkIDs = append(docChunkIDs, id)
			}
		}
	}
	labels := builtin.ChunkLabelsFor(ctx, s.sqlMem, tenantFromCtx(r.Context()),
		store.MemoryScope(body.Scope), storeScopeID, docChunkIDs)
	var legLabels map[string]builtin.ChunkLabel
	if len(legChunkIDs) > 0 {
		legLabels = builtin.ChunkLabelsFor(ctx, s.sqlMem, tenantFromCtx(r.Context()), legScope, legStoreScopeID, legChunkIDs)
	}

	for i, e := range res.Entries {
		entry := memorySearchResultEntry{
			Key:       e.Key,
			Value:     e.Value,
			Score:     e.Score,
			RankScore: res.RankScores[i],
			EmbeddedWith: memorySearchEmbeddedWith{
				Provider: e.EmbeddedWith.Provider,
				Model:    e.EmbeddedWith.Model,
			},
			// Classified by the SAME function the in-band tool and the filter use, so
			// the label cannot disagree with what the selector selected.
			Kind: string(memrank.Class(e)),
		}
		// A doc.chunk hit may be an entity fact OR a plain document chunk; the
		// viewer calls get_chunk to see its entity block. The ENTITY sidecar is still
		// not read here — but the chunk's document + heading are, because a page of
		// opaque `doc.chunk:<hex>` keys cannot be attributed without a fetch per row.
		// One batched, best-effort query above; a fault costs the labels, not the hit.
		if legPlan != nil {
			entry.Scope = string(hitScope(res.Scopes, i, store.MemoryScope(body.Scope)))
		}
		if entry.Kind == string(store.MemoryRowDocument) {
			entry.ChunkID = strings.TrimPrefix(e.Key, docChunkKeyPrefix)
			from := labels
			if legPlan != nil && entry.Scope == string(legScope) {
				from = legLabels
			}
			if lb, ok := from[entry.ChunkID]; ok {
				entry.Document, entry.Title = lb.Document, lb.Title
			}
		}
		if i < len(res.MatchedUnits) {
			entry.MatchedUnit = res.MatchedUnits[i]
		}
		entries = append(entries, entry)
	}

	out := memorySearchResponse{
		TimeFilter:        res.TimeFilter,
		Scope:             body.Scope,
		ScopeID:           storeScopeID,
		Entries:           entries,
		QueryEmbeddingDim: res.QueryEmbeddingDim,
		Truncated:         res.Truncated,
		Filter:            filterEcho,
	}
	if legPlan != nil {
		out.DocumentsScope, out.DocumentsScopeID = string(legScope), &legStoreScopeID
	}
	writeJSON(w, http.StatusOK, out)
}

// memorySearchRefusal is a 400 the documents_scope check answers with.
type memorySearchRefusal struct{ code, msg string }

// memorySearchDocumentsScope validates documents_scope. Equal to scope with
// the same subject, it adds documents to sources (when sources names kinds at
// all — an empty selector already spans documents) and returns no second
// scope, so the call is exactly that one. Otherwise it returns the second
// scope and its store scope id. The same scope with a different subject is
// refused: hits name their scope, not their subject, so the caller could not
// tell the two apart.
func memorySearchDocumentsScope(body memorySearchRequest, storeScopeID string, sources []memrank.Source) (store.MemoryScope, string, []memrank.Source, *memorySearchRefusal) {
	if body.DocumentsScope == "" {
		return "", "", sources, nil
	}
	if !validAdminMemoryScope(body.DocumentsScope) {
		return "", "", nil, &memorySearchRefusal{"invalid_documents_scope", "documents_scope must be one of: agent, user, tenant"}
	}
	legStoreScopeID := adminMemoryStoreScopeID(body.DocumentsScope, body.DocumentsScopeID)
	if body.DocumentsScope == body.Scope {
		if strings.TrimSpace(body.DocumentsScopeID) != "" && legStoreScopeID != storeScopeID {
			return "", "", nil, &memorySearchRefusal{"invalid_documents_scope",
				"documents_scope names the same scope as scope with a different documents_scope_id; search the other subject in a second call"}
		}
		if len(sources) == 0 || slices.Contains(sources, memrank.SourceDocuments) {
			return "", "", sources, nil
		}
		return "", "", append(slices.Clone(sources), memrank.SourceDocuments), nil
	}
	if adminMemoryScopeIDRequired(body.DocumentsScope) && strings.TrimSpace(body.DocumentsScopeID) == "" {
		return "", "", nil, &memorySearchRefusal{"missing_documents_scope_id", "documents_scope_id is required when documents_scope is agent or user"}
	}
	if slices.Contains(sources, memrank.SourceTraces) {
		return "", "", nil, &memorySearchRefusal{"invalid_documents_scope",
			"documents_scope cannot be combined with sources [traces]; search conversation turns on their own"}
	}
	return store.MemoryScope(body.DocumentsScope), legStoreScopeID, sources, nil
}

// hitScope is the scope of result i: the backend's report when a second scope
// was searched, else the call's own.
func hitScope(scopes []store.MemoryScope, i int, own store.MemoryScope) store.MemoryScope {
	if i < len(scopes) && scopes[i] != "" {
		return scopes[i]
	}
	return own
}

// hitsFrom selects the results from scope want, for a check made per scope;
// nil — every result — when no second scope was searched.
func hitsFrom(legPlan *builtin.DocumentsLegPlan, scopes []store.MemoryScope, own, want store.MemoryScope) func(int) bool {
	if legPlan == nil {
		return nil
	}
	return func(i int) bool { return hitScope(scopes, i, own) == want }
}

// memorySearchFilterCtx is the identity a document filter resolves under, and
// the one the hits' document access check runs under: the
// principal's own tenant (as the search itself), the requested scope's subject,
// and the operator-plane grants the off-run Document routes run with
// (substrateBrowseCtx → substrateAdminCtx). The principal stays on ctx, so the
// Document tool's access-control narrowing reads the caller's own set. The
// subject is the scope_id this handler already accepts for the search; no
// tenant focus is honoured, because the search does not honour one either.
func memorySearchFilterCtx(r *http.Request, scope, storeScopeID string) context.Context {
	subject := ""
	if scope != string(store.MemoryScopeTenant) {
		subject = storeScopeID
	}
	return substrateBrowseCtx(r.Context(), subject, "")
}

// memorySearchFilterRefusalStatus maps a refused document filter to its HTTP
// answer by failure class — the class the Memory tool reports in a run. A
// refusal of no known class is a fault.
func memorySearchFilterRefusalStatus(res tools.Result) (int, string) {
	if res.Error == nil {
		return http.StatusInternalServerError, "internal"
	}
	switch res.Error.Category {
	case tools.CategoryPermission:
		return http.StatusForbidden, "permission_denied"
	case tools.CategoryValidation:
		return http.StatusBadRequest, "invalid_filter"
	case tools.CategoryBusiness:
		return http.StatusBadRequest, "filter_unavailable"
	case tools.CategoryTransient:
		return http.StatusServiceUnavailable, "filter_unavailable"
	}
	return http.StatusInternalServerError, "internal"
}
