package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
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
	// conceptFilterMaxConcepts bounds the concepts one concept filter may cover, so
	// its id list stays well under either tier's bound-variable limit.
	conceptFilterMaxConcepts = 1000
)

// Concept match modes, echoed as concept_matched_by.
const (
	conceptByStatement = "statement"
	conceptByTag       = "tag"
)

// searchFilterApplied is the normalised filter, echoed back: singular folded into
// plural, paths canonicalised, duplicates dropped. ConceptMatchedBy follows from
// the scope alone, never from what the ontology holds, so it cannot tell a caller
// whether a concept exists.
type searchFilterApplied struct {
	UnderPaths         []string `json:"under_paths,omitempty"`
	ExcludeUnderPaths  []string `json:"exclude_under_paths,omitempty"`
	DocumentIDs        []string `json:"document_ids,omitempty"`
	ExcludeDocumentIDs []string `json:"exclude_document_ids,omitempty"`
	Types              []string `json:"types,omitempty"`
	Statuses           []string `json:"statuses,omitempty"`
	Tag                string   `json:"tag,omitempty"`
	TagPrefix          string   `json:"tag_prefix,omitempty"`
	Concept            string   `json:"concept,omitempty"`
	ConceptPrefix      string   `json:"concept_prefix,omitempty"`
	ConceptMatchedBy   string   `json:"concept_matched_by,omitempty"`
	AsOf               *int64   `json:"as_of,omitempty"`

	// concepts are the resolved concept filters, one per named field; never echoed.
	concepts []conceptSet
}

// conceptSet is one resolved concept filter: the concepts it covers. Empty matches
// nothing — an unknown concept, and one the caller may not read, alike.
type conceptSet struct {
	ids []string // concept chunk ids, matched by statement
	// tags are the set's TOP concepts' tags; a tag under one of them is matched by
	// prefix, as tag_prefix matches.
	tags []string
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
	return nonEmpty([]string{in.UnderPath, in.ExcludeUnderPath, in.DocumentID, in.Type, in.Status, in.Tag, in.TagPrefix, in.Concept, in.ConceptPrefix}) ||
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
	ap.Concept = strings.TrimSpace(in.Concept)
	ap.ConceptPrefix = strings.TrimSpace(in.ConceptPrefix)
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
	if refusal, err := d.resolveConceptFilters(ctx, key, op, &ap); err != nil || refusal != nil {
		return nil, refusal, err
	}
	set := &searchFilterSet{echo: searchFilterEcho{Applied: ap}}

	// ACCESS CONTROL. For a caller whose access-control set can deny it reading
	// part of this scope, the filter is narrowed to the documents it may read
	// BEFORE anything is counted or sent, and the list always travels as an
	// allowlist: a denylist would have to name the unreadable chunks, and its size
	// would depend on them. So no id of a denied document reaches the store, and
	// no count, refusal or `inverted` bit moves when a denied document changes.
	// The listing post-filter still runs on the answer, unchanged; a filter-free
	// search is narrowed by resolveSearchACL instead.
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

// searchACLList is what a filter-free search sends the store for a caller whose
// access-control set hides part of the scope: ids (allow) or excludeIDs (deny),
// or none when the caller may read no chunk at all.
type searchACLList struct {
	ids, excludeIDs []string
	none            bool
}

// errSearchACLTooWide stops collecting a list that has passed the cap.
var errSearchACLTooWide = errors.New("search access-control list over the cap")

// resolveSearchACL withholds, BEFORE the search cuts its answer at limit, the
// chunks of the documents the caller may not read. Dropped after the cut, as the
// listing filter drops them, they shorten the page — a reader denied part of the
// scope gets fewer hits while more readable ones exist, which tells it hidden
// documents outrank them — and they take the expansion's frontier places. Sent
// as a list every leg applies before its LIMIT, the search ranks only what the
// caller may read: the answer, rank_score included, is the one it would get if
// the hidden documents did not exist.
//
// The list is the smaller side, read from names alone. A rule can only single a
// document out by a Path-tree name, so every document filed nowhere shares the
// bare scope's verdict. When that verdict is "readable", the hidden documents are
// all named ones and travel as a denylist (typically a few denied subtrees);
// when it is "unreadable", the readable ones are all named and travel as an
// allowlist. Neither needs a scan of the scope's chunks.
//
// nil: send nothing. The caller is not limited, or nothing it may not read
// exists — a plain search then, byte-identical to one with no set. nil too when
// the list would pass the operator's cap: the search then runs unnarrowed and the
// listing filter drops the hidden hits afterwards, so the page can come back
// short. That is the bound, accepted rather than refusing a call that names no
// filter; a filter_too_wide refusal here would be new to a caller who asked for
// nothing but a search.
func (d *Document) resolveSearchACL(ctx context.Context, key sqlmem.ScopeKey) (*searchACLList, error) {
	acl := d.newDocACL(ctx, key)
	limited, err := acl.limitsReading(ctx)
	if err != nil || !limited {
		return nil, err
	}
	// Limited, and no rule names part of the scope: the bare scope is denied, so
	// is every document in it.
	if !acl.pathSensitive {
		return &searchACLList{none: true}, nil
	}
	unnamedReadable, err := acl.readable(ctx, "")
	if err != nil {
		return nil, err
	}
	if _, err := acl.namesOf(ctx, ""); err != nil { // loads every name in the scope
		return nil, err
	}
	// The named documents whose verdict differs from the bare scope's: the hidden
	// ones when the bare scope is readable, the readable ones when it is not.
	var docs []string
	for id := range acl.paths {
		ok, err := acl.readable(ctx, id)
		if err != nil {
			return nil, err
		}
		if ok != unnamedReadable {
			docs = append(docs, id)
		}
	}
	if len(docs) == 0 {
		if unnamedReadable {
			return nil, nil
		}
		return &searchACLList{none: true}, nil
	}
	sort.Strings(docs)
	maxIDs := d.searchFilterMaxIDs()
	var chunks []string
	err = d.eachChunkOfDocuments(ctx, key, docs, "", nil, func(chunkID, _ string) error {
		if len(chunks) == maxIDs {
			return errSearchACLTooWide
		}
		chunks = append(chunks, chunkID)
		return nil
	})
	if errors.Is(err, errSearchACLTooWide) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	sort.Strings(chunks)
	switch {
	case unnamedReadable:
		if len(chunks) == 0 {
			return nil, nil // the hidden documents hold no chunk to keep out
		}
		return &searchACLList{excludeIDs: chunks}, nil
	case len(chunks) == 0:
		// An empty allowlist would mean "every chunk" to the store.
		return &searchACLList{none: true}, nil
	default:
		return &searchACLList{ids: chunks}, nil
	}
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
	for _, cs := range ap.concepts {
		frag, cargs := d.conceptPredicate(ctx, ap.ConceptMatchedBy, cs, ap.AsOf)
		parts = append(parts, frag)
		args = append(args, cargs...)
	}
	if ap.AsOf != nil {
		frag, targs := asOfClause(*ap.AsOf)
		parts = append(parts, frag)
		args = append(args, targs...)
	}
	return strings.Join(parts, " AND "), args
}

// CONCEPT FILTERS. A concept lives in the tenant ontology's concept schemes, and
// the closure "this concept and everything narrower" is read from the parsed
// scheme tree — nothing writes `broader` statements, and the mirror tags on the
// concept chunks are a copy an operator refreshes, so the tree is the authority.
//
// How a chunk matches depends on the scope searched. In the tenant scope, where
// the concept chunks are, a chunk matches by a live `subject` statement (or a
// sub-kind of it) to a concept of the set. Statements never cross scopes, so in a
// user or agent scope no statement can reach a concept; there a chunk matches by
// carrying a concept's tag, or a tag under it.
//
// A caller who may not read the ontology document resolves every concept to the
// empty set: the same answer, echo and all, as a concept that does not exist.

// resolveConceptFilters resolves the call's concept and concept_prefix into
// ap.concepts. A refusal is an ambiguous concept name or a set too large to send.
func (d *Document) resolveConceptFilters(ctx context.Context, key sqlmem.ScopeKey, op string, ap *searchFilterApplied) (*tools.Result, error) {
	if ap.Concept == "" && ap.ConceptPrefix == "" {
		return nil, nil
	}
	ap.ConceptMatchedBy = conceptByTag
	if key.Scope == "tenant" {
		ap.ConceptMatchedBy = conceptByStatement
	}
	schemes, err := d.readableSchemes(ctx)
	if err != nil {
		return nil, err
	}
	var sets []conceptSet
	if ap.Concept != "" {
		top, refusal := findConcept(op, schemes, ap.Concept)
		if refusal != nil {
			return refusal, nil
		}
		sets = append(sets, conceptClosure(schemes, top))
	}
	if ap.ConceptPrefix != "" {
		sets = append(sets, conceptClosure(schemes, conceptTagOf(ap.ConceptPrefix)))
	}
	for _, s := range sets {
		if len(s.ids) > conceptFilterMaxConcepts {
			r := errBusiness(fmt.Sprintf("%s: concept_too_wide: the concept filter covers %d concepts, over the limit of %d",
				op, len(s.ids), conceptFilterMaxConcepts), "Name a narrower concept.")
			return &r, nil
		}
	}
	ap.concepts = sets
	return nil, nil
}

// readableSchemes is the confirmed tenant ontology's concept schemes, or nil when
// there are none or the caller may not read the ontology document. The ontology
// is read through the same cache the relation gates use; the read verb is asked
// as describe_ontology asks it.
func (d *Document) readableSchemes(ctx context.Context) ([]memrank.OntologyScheme, error) {
	tkey, _, err := d.ontologyTenantKey(ctx)
	if err != nil {
		return nil, err
	}
	read, ok := d.tenantOntologyRead(ctx)
	if !ok {
		// A fault, not an absence: answering "no such concept" would hide it.
		return nil, fmt.Errorf("the tenant ontology could not be read")
	}
	if !read.Confirmed || read.DocumentID == "" || len(read.Schemes) == 0 {
		return nil, nil
	}
	if acl := d.newDocACL(ctx, tkey); acl != nil {
		readable, err := acl.readable(ctx, read.DocumentID)
		if err != nil || !readable {
			return nil, err
		}
	}
	return read.Schemes, nil
}

// findConcept resolves a concept named by chunk id, tag or name to its tag; ""
// when no concept answers to it. A name two concepts share is refused with their
// tags, which name each one uniquely.
func findConcept(op string, schemes []memrank.OntologyScheme, v string) (string, *tools.Result) {
	tag := conceptTagOf(v)
	var named []string
	for _, s := range schemes {
		for _, c := range s.Concepts {
			switch {
			case c.ChunkID == v:
				return c.Tag, nil
			case strings.Contains(v, "/") && c.Tag == tag:
				return c.Tag, nil
			case !strings.Contains(v, "/") && memrank.MirrorSegment(c.Name) == tag:
				named = append(named, c.Tag)
			}
		}
	}
	if len(named) > 1 {
		sort.Strings(named)
		r := errValidation(fmt.Sprintf("%s: concept %q names %d concepts: %s", op, v, len(named), strings.Join(named, ", ")),
			"Pass the concept's tag instead of its name, e.g. concept="+named[0]+".")
		return "", &r
	}
	if len(named) == 1 {
		return named[0], nil
	}
	return "", nil
}

// conceptTagOf folds a name or a tag path the way mirror tags are written, segment
// by segment, so "Topics/Risk Management" and "topics/risk-management" are one.
func conceptTagOf(v string) string {
	var segs []string
	for _, p := range strings.Split(v, "/") {
		if seg := memrank.MirrorSegment(p); seg != "" {
			segs = append(segs, seg)
		}
	}
	return strings.Join(segs, "/")
}

// conceptClosure is every concept whose tag is tag or lies under it.
func conceptClosure(schemes []memrank.OntologyScheme, tag string) conceptSet {
	var cs conceptSet
	if tag == "" {
		return cs
	}
	var tags []string
	for _, s := range schemes {
		for _, c := range s.Concepts {
			if c.Tag == tag || strings.HasPrefix(c.Tag, tag+"/") {
				cs.ids = append(cs.ids, c.ChunkID)
				tags = append(tags, c.Tag)
			}
		}
	}
	sort.Strings(cs.ids)
	sort.Strings(tags)
	for _, t := range tags {
		covered := false
		for _, top := range cs.tags {
			covered = covered || strings.HasPrefix(t, top+"/")
		}
		if !covered {
			cs.tags = append(cs.tags, t)
		}
	}
	return cs
}

// conceptPredicate renders one concept filter over `chunks c`. The concept ids
// come from the parsed ontology, never from the caller.
//
// With as_of, a statement counts only while it is in force at that instant, as a
// graph walk reads it: a chunk that WAS about the concept then is about it as of
// then, and one that only became so later is not. Without as_of its dates are not
// consulted.
func (d *Document) conceptPredicate(ctx context.Context, by string, cs conceptSet, asOf *int64) (string, []any) {
	if by == conceptByStatement {
		if len(cs.ids) == 0 {
			return "1=0", nil
		}
		kinds, _ := d.expandKindFilter(ctx, []string{"subject"})
		kph, args := inPlaceholders(kinds)
		iph, iargs := inPlaceholders(cs.ids)
		live, largs := statementVisibility{asOf: asOf}.clause()
		args = append(append(args, iargs...), largs...)
		return "c.id IN (SELECT e.from_id FROM chunk_edges e WHERE e.kind IN (" + kph + ") AND e.to_id IN (" + iph +
			") AND " + live + ")", args
	}
	if len(cs.tags) == 0 {
		return "1=0", nil
	}
	var ors []string
	var args []any
	for _, t := range cs.tags {
		ors = append(ors, "tag = ? OR tag LIKE ?")
		args = append(args, t, t+"/%")
	}
	return "c.id IN (SELECT chunk_id FROM chunk_tags WHERE " + strings.Join(ors, " OR ") + ")", args
}

// asOfClause is THE as_of rule over `chunks c` LEFT JOIN `chunk_memory_meta m`:
// BOTH intervals must contain the instant — the chunk's own and, for a fact-tier
// chunk, its sidecar's. Two stated intervals that disagree are two claims, and
// "true then?" is answered yes only when both say so. search, related and
// graph_recall all take it from here, so one as_of means one thing.
func asOfClause(asOf int64) (string, []any) {
	frag, args := chunkIntervalClause("c", asOf)
	side, sargs := graphTemporalClause(docInput{AsOf: &asOf})
	return "(" + frag + " AND " + side + ")", append(args, sargs...)
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
