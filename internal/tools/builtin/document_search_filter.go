package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// Structural filters on op=search and op=related.
//
// RESOLVE IN SQL MEMORY, RESTRICT IN THE MAIN STORE. The structure a filter
// names — path, document, type, status, tags, validity — lives in SQL Memory;
// chunk bodies and their embeddings live in the main store, keyed
// doc.chunk:<chunk_id>. The two cannot be joined in one statement, so the filter
// is resolved ONCE per call to a set of chunk ids, and that set travels into the
// store as an id list every leg applies before its LIMIT. A post-filter cannot do
// this job: each leg is capped at 51 rows, and a small corpus inside a large
// scope never reaches a pool filled from the rest of it.
//
// Resolved at query time, against the tables that own each fact, so a filter is
// right the moment a chunk is moved, retyped, retagged or renamed — there is no
// second copy of the structure to keep in step.

const (
	// searchFilterMaxIDsDefault caps the id list sent to the store when the
	// operator sets nothing (LOOMCYCLE_DOCUMENT_SEARCH_FILTER_MAX_IDS).
	searchFilterMaxIDsDefault = 50000
	// searchFilterBatch bounds one IN list of document ids. sqlite allows 32,766
	// bound variables and Postgres 65,535 per statement; one batch size keeps a
	// single code path well under both.
	searchFilterBatch = 500
	// searchFilterPage is the keyset page for the resolver's listings. A page cut
	// short by a lower operator row cap (sqlmem_max_rows) is followed by the next
	// one, never mistaken for the end of the set.
	searchFilterPage = 5000
)

// searchFilterApplied is the normalised filter, echoed back: singular folded into
// plural, paths canonicalised, duplicates dropped.
type searchFilterApplied struct {
	UnderPaths         []string `json:"under_paths,omitempty"`
	ExcludeUnderPaths  []string `json:"exclude_under_paths,omitempty"`
	DocumentIDs        []string `json:"document_ids,omitempty"`
	ExcludeDocumentIDs []string `json:"exclude_document_ids,omitempty"`
	Types              []string `json:"types,omitempty"`
	Statuses           []string `json:"statuses,omitempty"`
	Tag                string   `json:"tag,omitempty"`
	TagPrefix          string   `json:"tag_prefix,omitempty"`
	AsOf               *int64   `json:"as_of,omitempty"`
}

// searchFilterEcho is the `filter` block of a filtered response: what the filter
// resolved to, so a caller can tell "nothing relevant" from "wrong path".
type searchFilterEcho struct {
	Applied          searchFilterApplied `json:"applied"`
	MatchedDocuments int                 `json:"matched_documents"`
	MatchedChunks    int                 `json:"matched_chunks"`
	TypeExpandedTo   []string            `json:"type_expanded_to,omitempty"`
	Inverted         bool                `json:"inverted"`
}

// searchFilterSet is a resolved filter: the echo, and the one list the store is
// sent — ids (allow) or excludeIDs (deny), never both.
type searchFilterSet struct {
	echo       searchFilterEcho
	ids        []string
	excludeIDs []string
	// documentIDs / excludeDocumentIDs replace the two lists above when the filter
	// names only documents and the scope's rows carry their document id
	// (document_search_bydoc.go). At most one of the four is set.
	documentIDs        []string
	excludeDocumentIDs []string
}

// hasSearchFilter reports whether the call names any structural filter. False
// keeps a call on the code path it took before filters existed — not a new path
// with an empty set — which is what makes a filter-free response byte-identical.
func hasSearchFilter(in docInput) bool {
	nonEmpty := func(vs []string) bool {
		for _, v := range vs {
			if strings.TrimSpace(v) != "" {
				return true
			}
		}
		return false
	}
	return nonEmpty([]string{in.UnderPath, in.ExcludeUnderPath, in.DocumentID, in.Type, in.Status, in.Tag, in.TagPrefix}) ||
		nonEmpty(in.UnderPaths) || nonEmpty(in.ExcludeUnderPaths) || nonEmpty(in.DocumentIDs) ||
		nonEmpty(in.ExcludeDocumentIDs) || nonEmpty(in.Types) || nonEmpty(in.Statuses) || in.AsOf != nil
}

// normaliseSearchFilter folds the singular fields into the plural ones and checks
// what can be checked without a query. An unknown type, status or tag is not an
// error — it matches nothing, and the echo says so.
func normaliseSearchFilter(op string, in docInput) (searchFilterApplied, *tools.Result) {
	var ap searchFilterApplied
	list := func(one string, many []string) []string {
		var out []string
		seen := map[string]bool{}
		for _, v := range append([]string{one}, many...) {
			if v = strings.TrimSpace(v); v != "" && !seen[v] {
				seen[v] = true
				out = append(out, v)
			}
		}
		return out
	}
	paths := func(one string, many []string) ([]string, *tools.Result) {
		var out []string
		seen := map[string]bool{}
		for _, raw := range list(one, many) {
			canonical, err := normalizePath(raw)
			if err != nil {
				r := errValidation(op+": "+err.Error(), docPathFix)
				return nil, &r
			}
			if !seen[canonical] {
				seen[canonical] = true
				out = append(out, canonical)
			}
		}
		return out, nil
	}
	var r *tools.Result
	if ap.UnderPaths, r = paths(in.UnderPath, in.UnderPaths); r != nil {
		return ap, r
	}
	if ap.ExcludeUnderPaths, r = paths(in.ExcludeUnderPath, in.ExcludeUnderPaths); r != nil {
		return ap, r
	}
	ap.DocumentIDs = list(in.DocumentID, in.DocumentIDs)
	ap.ExcludeDocumentIDs = list("", in.ExcludeDocumentIDs)
	ap.Types = list(in.Type, in.Types)
	ap.Statuses = list(in.Status, in.Statuses)
	ap.Tag = strings.TrimSpace(in.Tag)
	ap.TagPrefix = strings.TrimSpace(in.TagPrefix)
	if in.AsOf != nil {
		if *in.AsOf <= 0 {
			r := errValidation(op+": as_of must be a positive instant in unix nanoseconds",
				"Pass as_of as unix nanoseconds, e.g. 1747180800000000000, or omit it.")
			return ap, &r
		}
		v := *in.AsOf
		ap.AsOf = &v
	}
	return ap, nil
}

// searchFilterMaxIDs is the operator's cap on the id list, or the default.
func (d *Document) searchFilterMaxIDs() int {
	if d.Cfg != nil && d.Cfg.Env.DocumentSearchFilterMaxIDs > 0 {
		return d.Cfg.Env.DocumentSearchFilterMaxIDs
	}
	return searchFilterMaxIDsDefault
}

// resolveSearchFilter resolves a call's structural filter in this scope's SQL
// Memory to the chunk ids a search may return. A refusal (a malformed argument, a
// filter too wide to send) comes back as the *tools.Result; err is a store fault.
//
// Every value a caller names is a bound parameter; the ids bound into the store
// statements come from SQL Memory, never from the caller.
func (d *Document) resolveSearchFilter(ctx context.Context, key sqlmem.ScopeKey, op string, in docInput) (*searchFilterSet, *tools.Result, error) {
	ap, refusal := normaliseSearchFilter(op, in)
	if refusal != nil {
		return nil, refusal, nil
	}
	set := &searchFilterSet{echo: searchFilterEcho{Applied: ap}}

	// ACCESS CONTROL. For a caller whose access-control set can deny it reading
	// part of this scope, the filter is narrowed to the documents it may read
	// BEFORE anything is counted or sent, and the list always travels as an
	// allowlist: a denylist would have to name the unreadable chunks, and its size
	// would depend on them. So no id of a denied document reaches the store, and
	// no count, refusal or `inverted` bit moves when a denied document changes.
	// The listing post-filter still runs on the answer, unchanged; a filter-free
	// search is not narrowed here at all (that pre-filter is a separate change).
	acl := d.newDocACL(ctx, key)
	limited, err := acl.limitsReading(ctx)
	if err != nil {
		return nil, nil, err
	}

	// Document level. A nil include set means "every document"; an include that
	// resolves to nothing makes the whole set empty, as query_chunks does.
	var include map[string]bool
	if len(ap.UnderPaths) > 0 || len(ap.DocumentIDs) > 0 {
		include = map[string]bool{}
		for _, p := range ap.UnderPaths {
			ids, err := d.documentsAtOrUnder(ctx, key, p)
			if err != nil {
				return nil, nil, err
			}
			for _, id := range ids {
				include[id] = true
			}
		}
		// A foreign or unknown id simply has no chunks in this scope's tables, so it
		// resolves to nothing — reported as a count, never as an error that would
		// confirm the id exists somewhere else.
		for _, id := range ap.DocumentIDs {
			include[id] = true
		}
	}
	exclude := map[string]bool{}
	for _, p := range ap.ExcludeUnderPaths {
		ids, err := d.documentsAtOrUnder(ctx, key, p)
		if err != nil {
			return nil, nil, err
		}
		for _, id := range ids {
			exclude[id] = true
		}
	}
	for _, id := range ap.ExcludeDocumentIDs {
		exclude[id] = true
	}
	if include != nil {
		for id := range exclude {
			delete(include, id)
		}
		if len(include) == 0 {
			return set, nil, nil
		}
	}

	where, args := d.chunkFilterPredicate(ctx, ap, &set.echo)
	scopeChunks, scopeDocs, err := d.scopeChunkCounts(ctx, key)
	if err != nil {
		return nil, nil, err
	}
	maxIDs := d.searchFilterMaxIDs()
	if include == nil && where == "" && !limited {
		// Exclusion only: a denylist by nature, never inverted.
		deny, denyDocs, err := d.chunkIDsOfDocuments(ctx, key, docIDsOf(exclude))
		if err != nil {
			return nil, nil, err
		}
		set.echo.MatchedChunks = scopeChunks - len(deny)
		set.echo.MatchedDocuments = scopeDocs - len(denyDocs)
		if set.echo.MatchedChunks <= 0 {
			set.echo.MatchedChunks, set.echo.MatchedDocuments = 0, 0
			return set, nil, nil
		}
		if len(deny) > maxIDs {
			return nil, filterTooWide(op, set.echo.MatchedChunks, scopeChunks, len(deny), maxIDs), nil
		}
		set.excludeIDs = deny
		return set, nil, nil
	}

	var allow []string
	allowDocs := map[string]bool{}
	readable := map[string]bool{}
	keep := func(chunkID, docID string) error {
		if exclude[docID] {
			return nil
		}
		if limited {
			ok, seen := readable[docID]
			if !seen {
				var err error
				if ok, err = acl.readable(ctx, docID); err != nil {
					return err
				}
				readable[docID] = ok
			}
			if !ok {
				return nil
			}
		}
		allow = append(allow, chunkID)
		allowDocs[docID] = true
		return nil
	}
	if include != nil {
		if err := d.eachChunkOfDocuments(ctx, key, docIDsOf(include), where, args, keep); err != nil {
			return nil, nil, err
		}
	} else if err := d.eachChunk(ctx, key, "", where, args, keep); err != nil {
		return nil, nil, err
	}
	set.echo.MatchedChunks, set.echo.MatchedDocuments = len(allow), len(allowDocs)
	if len(allow) == 0 {
		return set, nil, nil
	}
	if limited {
		// Never inverted: see the access-control note above.
		if len(allow) > maxIDs {
			return nil, filterTooWideLimited(op, len(allow), maxIDs), nil
		}
		sort.Strings(allow)
		set.ids = allow
		return set, nil, nil
	}

	// INVERSION. The list is bound into every leg, so send whichever of the set
	// and its complement is smaller. A set larger than half the scope travels as
	// the complement, which then holds every chunk the filter did NOT match.
	if 2*len(allow) <= scopeChunks {
		if len(allow) > maxIDs {
			return nil, filterTooWide(op, len(allow), scopeChunks, len(allow), maxIDs), nil
		}
		sort.Strings(allow)
		set.ids = allow
		return set, nil, nil
	}
	allowed := make(map[string]bool, len(allow))
	for _, id := range allow {
		allowed[id] = true
	}
	var complement []string
	if err := d.eachChunk(ctx, key, "", "", nil, func(chunkID, _ string) error {
		if !allowed[chunkID] {
			complement = append(complement, chunkID)
		}
		return nil
	}); err != nil {
		return nil, nil, err
	}
	if len(complement) > maxIDs {
		return nil, filterTooWide(op, len(allow), scopeChunks, len(complement), maxIDs), nil
	}
	// An empty complement is a filter that matched the whole scope: no list at all
	// is then exactly right, and the echo still reports what was matched.
	set.excludeIDs = complement
	set.echo.Inverted = true
	return set, nil, nil
}

// filterTooWide refuses a filter whose id list is over the cap — the smaller of
// the set and its complement, or an exclusion's own list. Never a silent fall-back
// to a post-filter: that is the wrong answer this filter exists to replace.
func filterTooWide(op string, matched, scope, listed, maxIDs int) *tools.Result {
	r := errBusiness(fmt.Sprintf("%s: filter_too_wide: the filter matches %d of this scope's %d chunks and would send the search a list of %d chunk ids, over the limit of %d (matched_chunks=%d scope_chunks=%d max_ids=%d)",
		op, matched, scope, listed, maxIDs, matched, scope, maxIDs),
		"Narrow the filter with type, status or a tag, or search a smaller subtree.")
	return &r
}

// filterTooWideLimited is filterTooWide for a caller an access-control set
// limits: its list is the readable matches, so that is all it reports — the
// scope's size would describe documents it may not read.
func filterTooWideLimited(op string, matched, maxIDs int) *tools.Result {
	r := errBusiness(fmt.Sprintf("%s: filter_too_wide: the filter matches %d chunks you may read, over the limit of %d chunk ids a search can be sent (matched_chunks=%d max_ids=%d)",
		op, matched, maxIDs, matched, maxIDs),
		"Narrow the filter with type, status or a tag, or search a smaller subtree.")
	return &r
}

// chunkFilterPredicate renders the chunk-level predicates (type with subtype
// expansion, status, tag, tag_prefix, as_of) over `chunks c` and, for as_of, the
// sidecar `chunk_memory_meta m`. The same SQL query_chunks and graph_recall use,
// so one filter means one thing on every op. Returns "" for none.
func (d *Document) chunkFilterPredicate(ctx context.Context, ap searchFilterApplied, echo *searchFilterEcho) (string, []any) {
	var parts []string
	var args []any
	if len(ap.Types) > 0 {
		var types []string
		seen := map[string]bool{}
		widened := false
		for _, t := range ap.Types {
			exp := d.expandTypeFilter(ctx, t)
			if len(exp) > 1 {
				widened = true
			}
			for _, x := range exp {
				if !seen[x] {
					seen[x] = true
					types = append(types, x)
				}
			}
		}
		frag, targs := typeFilterSQL("c.type", types)
		parts = append(parts, frag)
		args = append(args, targs...)
		// REPORTED, as query_chunks reports it: the filter that ran is wider than
		// the one asked for.
		if widened {
			echo.TypeExpandedTo = types
		}
	}
	if len(ap.Statuses) > 0 {
		frag, sargs := typeFilterSQL("c.status", ap.Statuses)
		parts = append(parts, frag)
		args = append(args, sargs...)
	}
	// Tags keep query_chunks' semantics, including its tolerance of an unescaped
	// '%' or '_' in a prefix: a false positive only widens a filter the caller can
	// see in the echo.
	if ap.Tag != "" {
		parts = append(parts, "c.id IN (SELECT chunk_id FROM chunk_tags WHERE tag = ?)")
		args = append(args, ap.Tag)
	}
	if ap.TagPrefix != "" {
		parts = append(parts, "c.id IN (SELECT chunk_id FROM chunk_tags WHERE tag = ? OR tag LIKE ?)")
		args = append(args, ap.TagPrefix, ap.TagPrefix+"/%")
	}
	if ap.AsOf != nil {
		// BOTH intervals must contain the instant: the chunk's own and, for a
		// fact-tier chunk, its sidecar's. Two stated intervals that disagree are two
		// claims, and "true then?" is answered yes only when both say so.
		frag, targs := chunkIntervalClause("c", *ap.AsOf)
		parts = append(parts, frag)
		args = append(args, targs...)
		frag, targs = graphTemporalClause(docInput{AsOf: ap.AsOf})
		parts = append(parts, frag)
		args = append(args, targs...)
	}
	return strings.Join(parts, " AND "), args
}

// chunkIntervalClause is the chunk's OWN validity test at an instant, over the
// chunks table reached as ref: half-open [valid_at, invalid_at), NULL on either
// side unbounded — the boundary convention the sidecar and the k/v row use.
func chunkIntervalClause(ref string, asOf int64) (string, []any) {
	return "(" + ref + ".valid_at IS NULL OR " + ref + ".valid_at <= ?) AND (" +
			ref + ".invalid_at IS NULL OR " + ref + ".invalid_at > ?)",
		[]any{asOf, asOf}
}

// scopeChunkCounts is the scope's chunk and document counts — what inversion and
// an exclusion-only filter measure against.
func (d *Document) scopeChunkCounts(ctx context.Context, key sqlmem.ScopeKey) (chunks, docs int, err error) {
	res, err := d.query(ctx, key, `SELECT count(*), count(DISTINCT document_id) FROM chunks`)
	if err != nil {
		return 0, 0, err
	}
	if len(res.Rows) == 0 || len(res.Rows[0]) < 2 {
		return 0, 0, nil
	}
	return asInt(res.Rows[0][0]), asInt(res.Rows[0][1]), nil
}

// eachChunk calls fn with every (chunk id, document id) matching where (over
// `chunks c`, plus `chunk_memory_meta m` when where names it) and, when docIN is
// set, restricted by that document IN fragment. Keyset-paged on the chunk id, so
// the operator's row cap never truncates the set silently.
func (d *Document) eachChunk(ctx context.Context, key sqlmem.ScopeKey, docIN, where string, args []any, fn func(chunkID, docID string) error) error {
	from := "chunks c"
	if strings.Contains(where, "m.") {
		from += " LEFT JOIN chunk_memory_meta m ON m.chunk_id = c.id"
	}
	cond := "c.id > ?"
	if docIN != "" {
		cond += " AND " + docIN
	}
	if where != "" {
		cond += " AND " + where
	}
	stmt := "SELECT c.id, c.document_id FROM " + from + " WHERE " + cond + " ORDER BY c.id LIMIT ?"
	after := ""
	for {
		res, err := d.query(ctx, key, stmt, append(append([]any{after}, args...), searchFilterPage)...)
		if err != nil {
			return err
		}
		for _, r := range res.Rows {
			if err := fn(asStr(r[0]), asStr(r[1])); err != nil {
				return err
			}
		}
		if len(res.Rows) == 0 || (!res.Truncated && len(res.Rows) < searchFilterPage) {
			return nil
		}
		after = asStr(res.Rows[len(res.Rows)-1][0])
	}
}

// eachChunkOfDocuments is eachChunk over a document set, in IN batches.
func (d *Document) eachChunkOfDocuments(ctx context.Context, key sqlmem.ScopeKey, docIDs []string, where string, args []any, fn func(chunkID, docID string) error) error {
	for start := 0; start < len(docIDs); start += searchFilterBatch {
		end := min(start+searchFilterBatch, len(docIDs))
		ph, dargs := inPlaceholders(docIDs[start:end])
		// The document batch binds BEFORE the predicate's own arguments, matching
		// the order the fragments appear in the statement.
		if err := d.eachChunk(ctx, key, "c.document_id IN ("+ph+")", where, append(dargs, args...), fn); err != nil {
			return err
		}
	}
	return nil
}

// chunkIDsOfDocuments lists the chunks of a document set, and which of those
// documents had any.
func (d *Document) chunkIDsOfDocuments(ctx context.Context, key sqlmem.ScopeKey, docIDs []string) ([]string, map[string]bool, error) {
	var ids []string
	docs := map[string]bool{}
	err := d.eachChunkOfDocuments(ctx, key, docIDs, "", nil, func(chunkID, docID string) error {
		ids = append(ids, chunkID)
		docs[docID] = true
		return nil
	})
	sort.Strings(ids)
	return ids, docs, err
}

// documentsAtOrUnder is the documents named AT a canonical path or anywhere under
// it, in the caller's own tree (tenant + scope + scope id): another tenant's path
// spelled the same way is simply absent here.
func (d *Document) documentsAtOrUnder(ctx context.Context, key sqlmem.ScopeKey, canonical string) ([]string, error) {
	ids, err := d.documentsUnderPath(ctx, key, canonical)
	if err != nil {
		return nil, err
	}
	parent, name, isRoot := splitPath(canonical)
	if isRoot {
		return ids, nil
	}
	row, err := d.Store.DirentGet(ctx, direntTenant(ctx), key.Scope, direntScopeID(key), parent, name)
	if err != nil {
		var nf *store.ErrNotFound
		if asNotFound(err, &nf) {
			return ids, nil
		}
		return nil, err
	}
	if row.Kind == "document" {
		var ref struct {
			DocumentID string `json:"document_id"`
		}
		_ = json.Unmarshal(row.ResourceRef, &ref)
		if ref.DocumentID != "" {
			ids = append(ids, ref.DocumentID)
		}
	}
	return ids, nil
}

func docIDsOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
