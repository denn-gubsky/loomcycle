package builtin

// document_citations.go — Document op=extract_citations (RFC FP §13 R5).
//
// A document class's @citation_grammar is parsed from the ontology but nothing applied
// it. This op applies it to a document's bodies, in Go, and resolves each citation to
// the cited work and part. The links-building agent could run the patterns itself, but
// in its JavaScript they would be backtracking regexes, not RE2: a pattern written for
// a linear-time engine could then run in exponential time on a hostile body.
//
// READ-ONLY. It never files a placeholder for a cited work the scope does not hold; the
// caller does that through resolve_anchor, under resolve_anchor's own checks.

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

const (
	citationsDefaultLimit = 200
	citationsMaxLimit     = 1000
)

// citationResolution is where a citation leads. Exact is true when the citation names a
// part and that part was found in force; otherwise it resolved to the work at most.
//
// A work held by a document the caller may not read resolves to nothing, exactly as a
// work the scope does not hold: saying more would tell a confined reader the work
// exists, which resolve_anchor refuses to tell the same caller.
type citationResolution struct {
	DocumentID  string `json:"document_id,omitempty"`
	ChunkID     string `json:"chunk_id,omitempty"`
	Exact       bool   `json:"exact"`
	Placeholder bool   `json:"placeholder"`
}

// citationHit is one match of the grammar in one body. CharStart/CharEnd are rune
// offsets into the body, the unit statement evidence spans use.
type citationHit struct {
	ChunkID   string             `json:"chunk_id"`
	Quote     string             `json:"quote"`
	CharStart int                `json:"char_start"`
	CharEnd   int                `json:"char_end"`
	Groups    map[string]string  `json:"groups,omitempty"`
	WorkKey   string             `json:"work_key,omitempty"`
	Anchor    string             `json:"anchor,omitempty"`
	Kind      string             `json:"kind,omitempty"`
	Resolved  citationResolution `json:"resolved"`
}

// extractCitations answers op=extract_citations: the citation grammar of the document's
// class applied to the document's bodies (document_id), or to one chunk's (id).
func (d *Document) extractCitations(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, in docInput) (tools.Result, error) {
	if (in.ID == "") == (in.DocumentID == "") {
		return errValidation("extract_citations: pass exactly one of document_id (every part of a document) or id (one chunk)", ""), nil
	}
	limit := in.Limit
	if limit <= 0 {
		limit = citationsDefaultLimit
	}
	if limit > citationsMaxLimit {
		limit = citationsMaxLimit
	}

	docID := in.DocumentID
	var only *chunkRow
	if in.ID != "" {
		row, ok, err := d.getChunkRow(ctx, key, in.ID)
		if err != nil {
			return errFrom("extract_citations: "+err.Error(), err), nil
		}
		if !ok {
			return errNotFound("extract_citations: no such chunk: "+in.ID, "Pass a chunk id of this scope."), nil
		}
		docID, only = row.DocumentID, &row
	}
	res, err := d.query(ctx, key, `SELECT root_chunk_id FROM documents WHERE id = ? LIMIT 1`, docID)
	if err != nil {
		return errFrom("extract_citations: "+err.Error(), err), nil
	}
	if len(res.Rows) == 0 {
		return errNotFound("extract_citations: no such document: "+docID, "Pass a document id of this scope."), nil
	}
	rootID := asStr(res.Rows[0][0])

	out := map[string]any{"document_id": docID, "grammar": nil, "hits": []citationHit{}}
	g, class, issue := d.classGrammar(ctx, key, rootID, "citation_grammar")
	if issue != "" {
		out["grammar_issue"] = issue
	}
	if g.Name == "" {
		return jsonResult(out)
	}
	out["grammar"] = map[string]string{"name": g.Name, "class": class}

	var chunks []chunkRow
	if only != nil {
		chunks = []chunkRow{*only}
	} else {
		at := time.Now().UnixNano()
		if in.AsOf != nil {
			at = *in.AsOf
		}
		if chunks, err = d.chunksInForce(ctx, key, docID, at); err != nil {
			return errFrom("extract_citations: "+err.Error(), err), nil
		}
	}

	res2 := newCitationResolver(d, ctx, key, mscope, in.AsOf)
	hits := []citationHit{}
	truncated := false
	for _, c := range chunks {
		cb, err := d.readBody(ctx, mscope, key, c.ID)
		if err != nil {
			return errFrom("extract_citations: "+err.Error(), err), nil
		}
		for _, h := range matchCitations(g, cb.Body) {
			if len(hits) == limit {
				truncated = true
				break
			}
			h.ChunkID = c.ID
			if h.Resolved, err = res2.resolve(h.WorkKey, h.Anchor, c.ValidAt); err != nil {
				return errFrom("extract_citations: "+err.Error(), err), nil
			}
			hits = append(hits, h)
		}
		if truncated {
			break
		}
	}
	out["hits"] = hits
	if truncated {
		out["truncated"] = true
	}
	return jsonResult(out)
}

// chunksInForce lists a document's chunks in force at `at`, in document order (depth
// first, by position), so hits read in the order the text does.
func (d *Document) chunksInForce(ctx context.Context, key sqlmem.ScopeKey, docID string, at int64) ([]chunkRow, error) {
	res, err := d.query(ctx, key, `SELECT `+chunkSelectCols+` FROM chunks WHERE document_id = ?
		AND (valid_at IS NULL OR valid_at <= ?) AND (invalid_at IS NULL OR invalid_at > ?)`, docID, at, at)
	if err != nil {
		return nil, err
	}
	children := map[string][]chunkRow{}
	ids := map[string]bool{}
	for _, r := range res.Rows {
		c := scanChunkRow(res.Columns, r)
		children[c.ParentID] = append(children[c.ParentID], c)
		ids[c.ID] = true
	}
	var out []chunkRow
	var walk func(parent string)
	walk = func(parent string) {
		kids := children[parent]
		sort.SliceStable(kids, func(i, j int) bool { return kids[i].Position < kids[j].Position })
		for _, k := range kids {
			out = append(out, k)
			walk(k.ID)
		}
	}
	// Roots of the walk: chunks whose parent is not itself in force (the document root,
	// and a part whose parent closed in an earlier edition), in position order.
	var roots []string
	for parent := range children {
		if parent == "" || !ids[parent] {
			roots = append(roots, parent)
		}
	}
	sort.Strings(roots)
	for _, p := range roots {
		walk(p)
	}
	return out, nil
}

// matchCitations finds the grammar's citations in one body: every pattern's matches, in
// text order, an overlap going to the match that starts first (then to the earlier
// pattern). Offsets are runes.
func matchCitations(g memrank.OntologyGrammar, body string) []citationHit {
	type match struct {
		loc   []int
		re    *regexp.Regexp
		order int
	}
	var all []match
	for i, p := range g.Patterns {
		// Compiled per call rather than kept on the grammar: the parse is shared through
		// the ontology cache and stays plain data. RE2 — linear time, no backtracking —
		// and the parser has already capped each pattern's length.
		re, err := regexp.Compile(p)
		if err != nil {
			continue
		}
		for _, loc := range re.FindAllStringSubmatchIndex(body, -1) {
			if loc[1] > loc[0] {
				all = append(all, match{loc: loc, re: re, order: i})
			}
		}
	}
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].loc[0] != all[j].loc[0] {
			return all[i].loc[0] < all[j].loc[0]
		}
		return all[i].order < all[j].order
	})
	workTmpl := resolveTemplate(g.Resolve, "work_key")
	anchorTmpl := resolveTemplate(g.Resolve, "anchor")
	kindTmpl := resolveTemplate(g.Resolve, "kind")
	var out []citationHit
	end, runeAt, byteAt := -1, 0, 0
	for _, m := range all {
		if m.loc[0] < end {
			continue
		}
		end = m.loc[1]
		names := m.re.SubexpNames()
		strs := make([]string, len(names))
		groups := map[string]string{}
		for i := range names {
			if s, e := m.loc[2*i], m.loc[2*i+1]; s >= 0 {
				strs[i] = body[s:e]
				if names[i] != "" {
					groups[names[i]] = body[s:e]
				}
			}
		}
		runeAt += utf8.RuneCountInString(body[byteAt:m.loc[0]])
		start := runeAt
		runeAt += utf8.RuneCountInString(body[m.loc[0]:m.loc[1]])
		byteAt = m.loc[1]
		h := citationHit{Quote: body[m.loc[0]:m.loc[1]], CharStart: start, CharEnd: runeAt}
		if len(groups) > 0 {
			h.Groups = groups
		}
		if workTmpl != "" {
			h.WorkKey = fillTemplate(workTmpl, names, strs)
		}
		if anchorTmpl != "" {
			h.Anchor = fillTemplate(anchorTmpl, names, strs)
		}
		if kindTmpl != "" {
			h.Kind = fillTemplate(kindTmpl, names, strs)
		}
		out = append(out, h)
	}
	return out
}

// citationResolver resolves citations through resolve_anchor, never filing a
// placeholder, and remembers each answer for the call: a document cites the same work
// many times.
type citationResolver struct {
	d      *Document
	ctx    context.Context
	key    sqlmem.ScopeKey
	mscope store.MemoryScope
	asOf   *int64
	seen   map[string]citationResolution
}

func newCitationResolver(d *Document, ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, asOf *int64) *citationResolver {
	return &citationResolver{d: d, ctx: ctx, key: key, mscope: mscope, asOf: asOf, seen: map[string]citationResolution{}}
}

// resolve finds the cited work and part as of the call's as_of or, without one, as of
// the citing passage's own edition (its valid_at): a 2019 edition citing "Article 12"
// means the Article 12 in force in 2019.
func (r *citationResolver) resolve(workKey, anchor string, passageValidAt *int64) (citationResolution, error) {
	if workKey == "" {
		return citationResolution{}, nil
	}
	at := time.Now().UnixNano()
	switch {
	case r.asOf != nil:
		at = *r.asOf
	case passageValidAt != nil:
		at = *passageValidAt
	}
	memo := workKey + "\x00" + anchor + "\x00" + strconv.FormatInt(at, 10)
	if v, ok := r.seen[memo]; ok {
		return v, nil
	}
	res, err := r.d.resolveAnchor(r.ctx, r.key, r.mscope, docInput{WorkKey: workKey, Anchor: anchor, AsOf: &at})
	if err != nil {
		return citationResolution{}, err
	}
	var v citationResolution
	if res.IsError {
		if res.Error == nil || res.Error.Category != tools.CategoryPermission {
			return citationResolution{}, errors.New("resolving a citation: " + strings.TrimSpace(res.Text))
		}
		// Unreadable: left unresolved (see citationResolution).
	} else {
		var body struct {
			Found       bool   `json:"found"`
			DocumentID  string `json:"document_id"`
			ChunkID     string `json:"chunk_id"`
			Placeholder bool   `json:"placeholder"`
		}
		if err := json.Unmarshal([]byte(res.Text), &body); err != nil {
			return citationResolution{}, err
		}
		if body.Found {
			v = citationResolution{DocumentID: body.DocumentID, ChunkID: body.ChunkID, Placeholder: body.Placeholder,
				Exact: anchor != "" && body.ChunkID != ""}
		}
	}
	r.seen[memo] = v
	return v, nil
}
