package builtin

// pathtool_listing.go — RFC HA: ls with_counts and the listing filters
// (name_contains, type, status).
//
// Both are answered from the dirents the one-level ls already loads (the whole
// subtree, to synthesize implicit directories) plus the documents' metadata,
// which lives in the scope's SQL Memory and is asked for in batches. Nothing is
// stored: a count is computed for the entries of the page returned, by the
// caller's access rules and the filters of that request.
//
// What the caller may not read is absent from both: a document it may not read
// is neither counted nor matched, so it never makes a folder appear, and the
// work budget (lsListingBudget) is spent only on documents it may read, so where
// a page stops says nothing about the documents it may not.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// PathDocumentSource is what ls asks of the Document tool for with_counts and
// the filters.
type PathDocumentSource interface {
	// PathDocuments returns this request's view of scope's documents, or false
	// when their metadata cannot be queried here.
	PathDocuments(ctx context.Context, scope string) (PathDocumentLookup, bool)
}

// PathDocumentLookup answers for the documents of one scope.
type PathDocumentLookup interface {
	// Readable keeps, in order, the ids of documents the caller may read.
	Readable(ctx context.Context, ids []string) ([]string, error)
	// Facts returns the metadata of each id that is a document of the scope a
	// listing counts; an id it leaves out is not counted and never matches.
	Facts(ctx context.Context, ids []string) (map[string]PathDocumentFact, error)
	// ExpandType returns the type and its subtypes.
	ExpandType(ctx context.Context, typ string) []string
}

// PathDocumentFact is a document's metadata as the filters read it.
type PathDocumentFact struct {
	Title, Type, Status string
}

const (
	// lsCountCap is where a count stops being exact (RFC HA decision 1).
	lsCountCap = 10000
	// lsListingBudget bounds one counted or filtered page: the documents whose
	// metadata it reads, counting only documents the caller may read. A page
	// that would read past it stops where it is and reports truncated, with a
	// cursor that resumes there — inside a large folder too, so a page always
	// makes progress. Set from measurement on a 100,000-document folder: the
	// metadata of one document costs about 5µs on SQL Memory's file tier and
	// 20µs on its postgres tier, so 25,000 keeps a page's metadata reads near
	// 0.13s and 0.5s — under the 0.7s the same listing spends loading the
	// folder's names — while still covering two capped counts (10,000) a page.
	lsListingBudget = 25000
	// lsFilterMaxLen bounds each filter value.
	lsFilterMaxLen = 256
)

// listingAsked reports whether ls was asked for counts or a filter.
func (in pathInput) listingAsked() bool {
	return in.WithCounts || in.filtering()
}

func (in pathInput) filtering() bool {
	return in.NameContains != "" || in.Type != "" || in.Status != ""
}

// validateListing refuses what the counted or filtered listing does not take.
func validateListing(in pathInput) *tools.Result {
	if !in.listingAsked() {
		return nil
	}
	if in.Recursive {
		r := errValidation("ls: with_counts, name_contains, type and status apply to a one-level listing, not a recursive one",
			"Drop recursive and open each folder with its own ls, passing the same filters.")
		return &r
	}
	for name, v := range map[string]string{"name_contains": in.NameContains, "type": in.Type, "status": in.Status} {
		if utf8.RuneCountInString(v) > lsFilterMaxLen {
			r := errValidation("ls: "+name+" is longer than 256 characters", "")
			return &r
		}
	}
	return nil
}

// listingCursor is where a counted or filtered page stopped. After is the last
// entry decided; Partial, when set, is the folder after it the page ran out of
// budget inside of.
type listingCursor struct {
	After   string
	Partial *listingPartial
}

// listingPartial is the work done on one folder so far: its documents up to
// After (in id order) have been read and N of them matched. Self is whether the
// entry's own document matched, carried so a resumed page never reads it again.
type listingPartial struct {
	Entry string `json:"e"`
	After string `json:"a,omitempty"`
	N     int    `json:"n,omitempty"`
	Self  bool   `json:"m,omitempty"`
}

// encodeListingCursor keeps the plain cursor's form when no folder is part
// done, so a plain cursor and this one are interchangeable. A part-done folder
// rides after a NUL: a plain ls given such a cursor reads it as a position just
// past After, which is where it would resume anyway.
func encodeListingCursor(c listingCursor) string {
	if c.Partial == nil {
		return encodeLsCursor(c.After)
	}
	b, _ := json.Marshal(c.Partial)
	return base64.RawURLEncoding.EncodeToString([]byte(c.After + "\x00" + string(b)))
}

func decodeListingCursor(cursor string) (listingCursor, error) {
	if cursor == "" {
		return listingCursor{}, nil
	}
	raw, err := decodeLsCursor(cursor)
	if err != nil {
		return listingCursor{}, err
	}
	after, rest, found := strings.Cut(raw, "\x00")
	c := listingCursor{After: after}
	if found {
		var p listingPartial
		if json.Unmarshal([]byte(rest), &p) != nil || p.Entry == "" || p.N < 0 {
			return listingCursor{}, errors.New("cursor is not a value this tool issued — pass back the next_cursor from the previous response")
		}
		c.Partial = &p
	}
	return c, nil
}

// listingScan is one request's evaluation state.
type listingScan struct {
	look      PathDocumentLookup
	in        pathInput
	types     map[string]bool // nil: no type filter
	needle    string          // lower-cased name_contains
	facts     map[string]*PathDocumentFact
	budget    int
	filtering bool
}

// fetch reads the metadata of the ids not read yet, at most the remaining
// budget of them, and returns how many of ids (in order) are now known.
func (s *listingScan) fetch(ctx context.Context, ids []string) (int, error) {
	var need []string
	known := len(ids)
	for i, id := range ids {
		if _, ok := s.facts[id]; ok {
			continue
		}
		if len(need) == s.budget {
			known = i
			break
		}
		need = append(need, id)
	}
	if len(need) == 0 {
		return known, nil
	}
	got, err := s.look.Facts(ctx, need)
	if err != nil {
		return 0, err
	}
	for _, id := range need {
		if f, ok := got[id]; ok {
			s.facts[id] = &f
		} else {
			s.facts[id] = nil
		}
	}
	s.budget -= len(need)
	return known, nil
}

// matches reports whether a read document matches the filters. names are its
// Path names within the folder asked about.
func (s *listingScan) matches(f *PathDocumentFact, names []string) bool {
	if f == nil {
		return false
	}
	if s.types != nil && !s.types[f.Type] {
		return false
	}
	if s.in.Status != "" && f.Status != s.in.Status {
		return false
	}
	if s.needle == "" || strings.Contains(strings.ToLower(f.Title), s.needle) {
		return true
	}
	for _, n := range names {
		if strings.Contains(strings.ToLower(n), s.needle) {
			return true
		}
	}
	return false
}

// subtree is the documents named under one entry: distinct ids in order, and
// each one's names there.
type subtree struct {
	ids   []string
	names map[string][]string
}

func subtreeOf(rows []store.DirentRow, self string) subtree {
	st := subtree{names: map[string][]string{}}
	for _, r := range rows {
		id := direntDocumentID(r)
		if id == "" || id == self {
			continue
		}
		if _, seen := st.names[id]; !seen {
			st.ids = append(st.ids, id)
		}
		st.names[id] = append(st.names[id], r.Name)
	}
	sort.Strings(st.ids)
	return st
}

// countUnder counts the documents of st the caller may read that match, from
// the document after resume.After on, starting at resume.N. It stops at the
// first match when only membership is asked (no counts), at the cap, or when the
// budget runs out — then done is false and partial says where it stopped.
func (s *listingScan) countUnder(ctx context.Context, st subtree, resume *listingPartial) (n int, capped, done bool, at string, err error) {
	ids := st.ids
	if resume != nil {
		n, at = resume.N, resume.After
		ids = ids[sort.Search(len(ids), func(i int) bool { return ids[i] > resume.After }):]
	}
	// One past the cap, so a folder of exactly lsCountCap is exact, not capped.
	enough := func() bool { return n > lsCountCap || (!s.in.WithCounts && n > 0) }
	for start := 0; start < len(ids) && !enough(); start += pathFactsBatch {
		batch := ids[start:min(start+pathFactsBatch, len(ids))]
		readable, err := s.look.Readable(ctx, batch)
		if err != nil {
			return 0, false, false, "", err
		}
		known, err := s.fetch(ctx, readable)
		if err != nil {
			return 0, false, false, "", err
		}
		for _, id := range readable[:known] {
			if f := s.facts[id]; f != nil && (!s.filtering || s.matches(f, st.names[id])) {
				n++
			}
			at = id
			if enough() {
				break
			}
		}
		if !enough() && known < len(readable) {
			// The budget ran out: resume after the last document read. Unreadable
			// ids before the next readable one are asked again then, at no cost
			// to the budget, which counts only documents the caller may read.
			return n, false, false, at, nil
		}
	}
	if n > lsCountCap {
		return lsCountCap, true, true, "", nil
	}
	return n, false, true, "", nil
}

// lsListing is one-level ls with counts or filters. entries are the sorted,
// already read-filtered entries of the directory; implicit maps each child name
// to the dirents beneath it.
func (p *Path) lsListing(ctx context.Context, canonical, scope string, entries []pathEntry, implicit map[string][]store.DirentRow, in pathInput) (tools.Result, error) {
	var look PathDocumentLookup
	ok := false
	if p.Documents != nil {
		look, ok = p.Documents.PathDocuments(ctx, scope)
	}
	if !ok {
		if in.filtering() {
			return errBusiness("ls: name_contains, type and status read the documents' metadata, which this scope cannot be asked for "+
				"(no SQL Memory, or this run may not query the scope's documents)",
				"Drop the filters and list the folder plainly."), nil
		}
		// Counts cannot be computed here: the field is absent, never zero.
		return lsPage(canonical, entries, in, func(e pathEntry) string { return e.Name }, p.summarize(ctx, scope, in))
	}
	cur, err := decodeListingCursor(in.Cursor)
	if err != nil {
		return errValidation("ls: "+err.Error(), ""), nil
	}
	s := &listingScan{look: look, in: in, facts: map[string]*PathDocumentFact{}, budget: lsListingBudget,
		filtering: in.filtering(), needle: strings.ToLower(in.NameContains)}
	if p.listingBudget > 0 {
		s.budget = p.listingBudget
	}
	var typeExpansion []string
	if in.Type != "" {
		typeExpansion = look.ExpandType(ctx, in.Type)
		s.types = map[string]bool{}
		for _, t := range typeExpansion {
			s.types[t] = true
		}
	}
	limit := in.Limit
	if limit <= 0 {
		limit = lsDefaultLimit
	}
	limit = min(limit, lsMaxLimit)

	start := sort.Search(len(entries), func(i int) bool { return entries[i].Name > cur.After })
	page := make([]pathEntry, 0)
	decided := cur.After
	var stop *listingCursor
	for i := start; i < len(entries); i++ {
		if len(page) == limit {
			stop = &listingCursor{After: decided}
			break
		}
		if s.filtering && (i-start)%pathFactsBatch == 0 {
			// The entries' own documents, a batch at a time, rather than one
			// query per entry when a folder of documents is filtered.
			if err := s.prefetchSelves(ctx, entries[i:min(i+pathFactsBatch, len(entries))]); err != nil {
				return errFrom("ls: "+err.Error(), err), nil
			}
		}
		e := entries[i]
		var resume *listingPartial
		if i == start && cur.Partial != nil && cur.Partial.Entry == e.Name {
			resume = cur.Partial
		}
		keep, partial, err := s.decide(ctx, &e, implicit[e.Name], resume)
		if err != nil {
			return errFrom("ls: "+err.Error(), err), nil
		}
		if partial != nil {
			stop = &listingCursor{After: decided}
			if partial.Entry != "" {
				stop.Partial = partial
			}
			break
		}
		decided = e.Name
		if keep {
			page = append(page, e)
		}
	}
	if decorate := p.summarize(ctx, scope, in); decorate != nil {
		decorate(page)
	}
	out := map[string]any{"path": canonical, "entries": page}
	if stop != nil {
		out["truncated"] = true
		out["next_cursor"] = encodeListingCursor(*stop)
	}
	if s.filtering {
		applied := map[string]string{}
		if in.NameContains != "" {
			applied["name_contains"] = in.NameContains
		}
		if in.Type != "" {
			applied["type"] = in.Type
		}
		if in.Status != "" {
			applied["status"] = in.Status
		}
		out["filters"] = applied
	}
	if len(typeExpansion) > 1 {
		out["type_expanded_to"] = typeExpansion
	}
	return jsonResult(out)
}

// prefetchSelves reads, within the budget, the metadata of the documents the
// given entries name that the caller may read.
func (s *listingScan) prefetchSelves(ctx context.Context, entries []pathEntry) error {
	var ids []string
	for _, e := range entries {
		if e.Kind == "document" {
			if id := direntDocumentID(store.DirentRow{Kind: e.Kind, ResourceRef: e.ResourceRef}); id != "" {
				ids = append(ids, id)
			}
		}
	}
	if len(ids) == 0 {
		return nil
	}
	readable, err := s.look.Readable(ctx, ids)
	if err != nil {
		return err
	}
	_, err = s.fetch(ctx, readable)
	return err
}

// decide evaluates one entry: whether a filtered listing keeps it, and its
// count. A non-nil partial means the budget ran out and the page stops at this
// entry: with an Entry, part way through the folder below it; without one,
// before the entry began.
func (s *listingScan) decide(ctx context.Context, e *pathEntry, under []store.DirentRow, resume *listingPartial) (keep bool, partial *listingPartial, err error) {
	self := ""
	if e.Kind == "document" {
		self = direntDocumentID(store.DirentRow{Kind: e.Kind, ResourceRef: e.ResourceRef})
	}
	selfMatch := false
	switch {
	case resume != nil:
		selfMatch = resume.Self
	case s.filtering && self != "":
		// The entry's own document matches only if the caller may read it: its
		// title, type and status are not to be probed through a filter.
		readable, err := s.look.Readable(ctx, []string{self})
		if err != nil {
			return false, nil, err
		}
		if len(readable) == 1 {
			known, err := s.fetch(ctx, readable)
			if err != nil {
				return false, nil, err
			}
			if known == 0 {
				// Out of budget before this entry began: the next page starts
				// at it afresh (an Entry-less partial).
				return false, &listingPartial{}, nil
			}
			selfMatch = s.matches(s.facts[self], []string{e.Name})
		}
	}
	folder := e.Kind == "directory" || e.HasChildren
	// A document that matches is kept whatever is under it; with no counts asked
	// the folder below it need not be read.
	if !folder || (selfMatch && !s.in.WithCounts) {
		if selfMatch {
			e.Matched = true
		}
		return !s.filtering || selfMatch, nil, nil
	}
	n, capped, done, at, err := s.countUnder(ctx, subtreeOf(under, self), resume)
	if err != nil {
		return false, nil, err
	}
	if !done {
		return false, &listingPartial{Entry: e.Name, After: at, N: n, Self: selfMatch}, nil
	}
	if s.in.WithCounts {
		count := n
		e.Documents = &count
		e.DocumentsCapped = capped
	}
	e.Matched = selfMatch
	return !s.filtering || selfMatch || n > 0, nil, nil
}
