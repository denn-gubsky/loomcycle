package builtin

// document_duplicates.go — RFC GB A1.2: find_duplicates, the copies in a set of
// documents, each pointed at its original.
//
// DETERMINISTIC, NO MODEL. Two documents are duplicates when their text hashes
// the same (basis sha256) or when their titles are the same once copy markers
// are stripped and they are the same kind of document (basis title). The hash
// is the op's own, over the chunk bodies, and not an import's provenance
// sha256: two exports of one file are rarely byte-identical, while their text
// is.
//
// A GROUP IS A STAR. Three copies of one document are three statements on the
// original, never a chain through each other: a reviewer rejecting one leaves
// the others standing.
//
// PROPOSING GOES THROUGH propose_statement. Each pair is filed by executeInput
// with op=propose_statement, so the method grant, the access-control question
// on both ends, the ontology's kind check and the evidence check are the ones
// a direct proposal meets. The op only decides which pairs, never whether a
// caller may file them. It never merges, hides, deletes or renames a document.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

const (
	// findDuplicatesMaxDocuments bounds one call's set (RFC GB A1.2): every body
	// of the set is read, so a corpus is checked a subtree at a time.
	findDuplicatesMaxDocuments = 5000
	// findDuplicatesMaxListed bounds the pairs in the answer; counts stay exact
	// and every pair is still proposed.
	findDuplicatesMaxListed = 1000
	duplicateKind           = "duplicate_of"
	duplicateBasisSHA256    = "sha256"
	duplicateBasisTitle     = "title"
	// A shared hash is the same text; a shared title only suggests it.
	duplicateConfidenceSHA256 = 1.0
	duplicateConfidenceTitle  = 0.7
)

// duplicatePrefixes are the leading copy markers, longest first where one is a
// prefix of another ("Копия - " before "Копия "). Fixed in v1 (owner decision).
var duplicatePrefixes = []string{"Copy of ", "Копия - ", "Копия ", "Kopie von ", "Copie de "}

// duplicateNumberSuffix is a trailing " (1)" to " (99)", as a file manager or
// editor numbers a copy. One or two digits only: "Budget (2023)" names a year,
// and would otherwise pair with "Budget (2024)".
var duplicateNumberSuffix = regexp.MustCompile(` \([1-9][0-9]?\)$`)

const duplicateCopySuffix = " - Copy"

// dupDoc is one document of the set, as the op compares it.
type dupDoc struct {
	id, title, rootID, typ string
	createdAt              int64
	titleKey               string // title without copy markers, case-folded
	marked                 bool   // the title carried a copy marker
	hash                   string // "" for a document with no text
	mediaType              string // the import provenance's, "" when not imported
	rootBody               string
	// firstChunk / firstLine: the first non-empty line of text, and its chunk,
	// quoted as evidence when the title cannot be.
	firstChunk, firstLine string
}

type dupPair struct {
	copy, orig *dupDoc
	basis      string
	confidence float64
}

// duplicateTitleKey is a title as find_duplicates compares it: trimmed, its
// copy markers stripped as often as they occur, case-folded; marked reports
// that it carried one.
func duplicateTitleKey(title string) (key string, marked bool) {
	s := strings.TrimSpace(title)
	for {
		before := s
		for _, p := range duplicatePrefixes {
			if rest, ok := cutPrefixFold(s, p); ok {
				s = strings.TrimSpace(rest)
			}
		}
		if loc := duplicateNumberSuffix.FindStringIndex(s); loc != nil {
			s = strings.TrimSpace(s[:loc[0]])
		}
		if rest, ok := cutSuffixFold(s, duplicateCopySuffix); ok {
			s = strings.TrimSpace(rest)
		}
		if s == before {
			break
		}
		marked = true
	}
	return strings.ToLower(s), marked
}

// cutPrefixFold is strings.CutPrefix ignoring case, rune by rune so a Cyrillic
// marker compares by letters, not bytes.
func cutPrefixFold(s, prefix string) (string, bool) {
	n := utf8.RuneCountInString(prefix)
	i, k := 0, 0
	for k < n && i < len(s) {
		_, w := utf8.DecodeRuneInString(s[i:])
		i += w
		k++
	}
	if k < n || !strings.EqualFold(s[:i], prefix) {
		return s, false
	}
	return s[i:], true
}

func cutSuffixFold(s, suffix string) (string, bool) {
	if len(s) < len(suffix) || !strings.EqualFold(s[len(s)-len(suffix):], suffix) {
		return s, false
	}
	return s[:len(s)-len(suffix)], true
}

// duplicateContentHash is the sha256 of a document's bodies in tree order with
// every whitespace run collapsed to one space and the ends trimmed, so a
// re-flowed paragraph or a chunk split elsewhere hashes the same. "" when there
// is no text: two empty stubs are not duplicates of each other.
func duplicateContentHash(bodies []string) string {
	text := strings.Join(strings.Fields(strings.Join(bodies, "\n")), " ")
	if text == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// duplicateOriginal picks a group's original: with markers applied, an
// unmarked member over a marked one (when the group has both), then the
// earliest created, then the lowest id.
func duplicateOriginal(members []*dupDoc, markers bool) *dupDoc {
	cands := members
	if markers {
		var unmarked []*dupDoc
		for _, m := range members {
			if !m.marked {
				unmarked = append(unmarked, m)
			}
		}
		if len(unmarked) > 0 && len(unmarked) < len(members) {
			cands = unmarked
		}
	}
	best := cands[0]
	for _, m := range cands[1:] {
		if m.createdAt < best.createdAt || (m.createdAt == best.createdAt && m.id < best.id) {
			best = m
		}
	}
	return best
}

// findDuplicates is op=find_duplicates.
func (d *Document) findDuplicates(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, in docInput) (tools.Result, error) {
	const op = "find_duplicates"
	bySHA, byTitle := true, true
	if in.Basis != nil {
		bySHA, byTitle = false, false
		for _, b := range in.Basis {
			switch b {
			case duplicateBasisSHA256:
				bySHA = true
			case duplicateBasisTitle:
				byTitle = true
			default:
				return errValidation(fmt.Sprintf("%s: basis %q is not sha256 or title", op, b), "Pass basis as a list of sha256 and/or title, or omit it for both."), nil
			}
		}
		if !bySHA && !byTitle {
			return errValidation(op+": basis is empty", "Pass sha256 and/or title, or omit basis for both."), nil
		}
	}
	ids, unreadable, refused := d.copySet(ctx, key, op, in, false, findDuplicatesMaxDocuments)
	if refused != nil {
		return *refused, nil
	}
	// A named document the caller may not read is left out as one that does
	// not exist: the answer must not say whether it has a copy.
	readable := ids[:0:0]
	for _, id := range ids {
		if !unreadable[id] {
			readable = append(readable, id)
		}
	}
	docs, err := d.loadDuplicateDocs(ctx, key, mscope, readable, bySHA)
	if err != nil {
		return errFrom(op+": "+err.Error(), err), nil
	}
	pairs := duplicatePairs(docs, bySHA, byTitle)
	existing, err := d.existingDuplicateStatements(ctx, key, docs)
	if err != nil {
		return errFrom(op+": "+err.Error(), err), nil
	}

	counts := map[string]int{"documents": len(docs), "pairs": len(pairs), duplicateBasisSHA256: 0, duplicateBasisTitle: 0,
		"proposed": 0, "skipped_existing": 0, "refused": 0}
	out := make([]map[string]any, 0, len(pairs))
	for _, p := range pairs {
		counts[p.basis]++
		row := map[string]any{
			"copy_id": p.copy.id, "original_id": p.orig.id,
			"copy_root_id": p.copy.rootID, "original_root_id": p.orig.rootID,
			"copy_title": p.copy.title, "original_title": p.orig.title,
			"basis": p.basis, "confidence": p.confidence,
		}
		switch {
		case existing[rootPairKey(p.copy.rootID, p.orig.rootID)]:
			// Already said, or refused, in one direction or the other: filing it
			// again would re-open what a reviewer closed.
			row["skipped"] = "exists"
			counts["skipped_existing"]++
		case in.Propose:
			d.proposeDuplicate(ctx, in, p, row, counts)
		}
		out = append(out, row)
	}
	truncated := len(out) > findDuplicatesMaxListed
	if truncated {
		out = out[:findDuplicatesMaxListed]
	}
	return jsonResult(map[string]any{"pairs": out, "counts": counts, "truncated": truncated})
}

func rootPairKey(a, b string) [2]string {
	if a > b {
		a, b = b, a
	}
	return [2]string{a, b}
}

// existingDuplicateStatements is every pair of the set's root chunks joined by
// a duplicate_of statement in either direction, whatever its status, validity
// or expiry. Both ends of a pair are in the set, so asking by from_id alone
// finds both directions.
func (d *Document) existingDuplicateStatements(ctx context.Context, key sqlmem.ScopeKey, docs []*dupDoc) (map[[2]string]bool, error) {
	roots := make([]string, 0, len(docs))
	for _, x := range docs {
		roots = append(roots, x.rootID)
	}
	out := map[[2]string]bool{}
	for _, batch := range batchesOf(roots, copyBatch) {
		ph := strings.TrimSuffix(strings.Repeat("?,", len(batch)), ",")
		err := d.copyRows(ctx, key, `SELECT from_id, to_id FROM chunk_edges WHERE kind = ? AND from_id IN (`+ph+`)`,
			"from_id, to_id, id", append([]any{duplicateKind}, anySlice(batch)...), func(_ []string, r []any) error {
				out[rootPairKey(asStr(r[0]), asStr(r[1]))] = true
				return nil
			})
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// proposeDuplicate files one pair through propose_statement and records the
// outcome on its row.
func (d *Document) proposeDuplicate(ctx context.Context, in docInput, p dupPair, row map[string]any, counts map[string]int) {
	conf := p.confidence
	pin := docInput{
		Op: "propose_statement", Scope: in.Scope, Method: in.Method,
		FromID: p.copy.rootID, ToID: p.orig.rootID, Kind: duplicateKind,
		Attributes: map[string]any{"basis": p.basis}, Confidence: &conf,
		Evidence: append(duplicateEvidence(p.copy, p.basis), duplicateEvidence(p.orig, p.basis)...),
	}
	res, err := d.executeInput(ctx, pin, nil)
	if err != nil || res.IsError {
		code := "error"
		if err == nil && res.Error != nil {
			code = string(res.Error.Category)
		}
		row["skipped"] = "refused:" + code
		if err == nil {
			row["detail"] = res.Text
		}
		counts["refused"]++
		return
	}
	var filed struct {
		ID            string `json:"id"`
		Status        string `json:"status"`
		OntologyValid bool   `json:"ontology_valid"`
	}
	_ = json.Unmarshal([]byte(res.Text), &filed)
	row["statement_id"], row["status"], row["ontology_valid"] = filed.ID, filed.Status, filed.OntologyValid
	counts["proposed"]++
}

// duplicateEvidence is one side's evidence: its title, quoted from its root
// chunk when the root's body holds it. A document's title is not part of any
// body, and evidence must quote a body (checkEvidence), so for a content match
// whose title is not in the root body the first line of its text stands in.
// A title match whose title is in no body has no quotable evidence.
func duplicateEvidence(doc *dupDoc, basis string) []edgeEvidenceIn {
	if t := strings.TrimSpace(doc.title); t != "" && strings.Contains(doc.rootBody, t) {
		return []edgeEvidenceIn{{ChunkID: doc.rootID, Quote: t}}
	}
	if basis == duplicateBasisSHA256 && doc.firstLine != "" {
		return []edgeEvidenceIn{{ChunkID: doc.firstChunk, Quote: doc.firstLine}}
	}
	return nil
}

// duplicatePairs groups docs and turns each group into copy → original pairs:
// content groups first, then title groups, a pair already found by content
// left out of the title groups.
func duplicatePairs(docs []*dupDoc, bySHA, byTitle bool) []dupPair {
	var pairs []dupPair
	seen := map[[2]string]bool{}
	pairKey := func(a, b string) [2]string {
		if a > b {
			a, b = b, a
		}
		return [2]string{a, b}
	}
	emit := func(groups [][]*dupDoc, basis string, conf float64, markers func([]*dupDoc) bool) {
		for _, g := range groups {
			orig := duplicateOriginal(g, markers(g))
			for _, m := range g {
				k := pairKey(m.id, orig.id)
				if m == orig || seen[k] {
					continue
				}
				seen[k] = true
				pairs = append(pairs, dupPair{copy: m, orig: orig, basis: basis, confidence: conf})
			}
		}
	}
	if bySHA {
		// The marker rule is a title match's: it applies to a content group
		// only when every member's title matches too.
		emit(groupDocs(docs, func(x *dupDoc) string { return x.hash }), duplicateBasisSHA256, duplicateConfidenceSHA256,
			func(g []*dupDoc) bool {
				for _, m := range g[1:] {
					if m.titleKey != g[0].titleKey || m.titleKey == "" {
						return false
					}
				}
				return true
			})
	}
	if byTitle {
		emit(titleGroups(docs), duplicateBasisTitle, duplicateConfidenceTitle, func([]*dupDoc) bool { return true })
	}
	return pairs
}

// groupDocs buckets docs by a non-empty key into the groups of two or more, in
// a stable order (by the lowest id in each).
func groupDocs(docs []*dupDoc, keyOf func(*dupDoc) string) [][]*dupDoc {
	buckets := map[string][]*dupDoc{}
	for _, x := range docs {
		if k := keyOf(x); k != "" {
			buckets[k] = append(buckets[k], x)
		}
	}
	var groups [][]*dupDoc
	for _, g := range buckets {
		if len(g) > 1 {
			sort.Slice(g, func(i, j int) bool { return g[i].id < g[j].id })
			groups = append(groups, g)
		}
	}
	sort.Slice(groups, func(i, j int) bool { return groups[i][0].id < groups[j][0].id })
	return groups
}

// titleGroups groups by title key and root type. Two imported documents of
// different media types are not one document; a bucket whose members carry
// more than one media type is split by it, and the documents with none form
// their own group rather than joining one of them (which would be a guess).
func titleGroups(docs []*dupDoc) [][]*dupDoc {
	var out [][]*dupDoc
	for _, g := range groupDocs(docs, func(x *dupDoc) string {
		if x.titleKey == "" {
			return ""
		}
		return x.titleKey + "\x00" + x.typ
	}) {
		types := map[string]bool{}
		for _, m := range g {
			if m.mediaType != "" {
				types[m.mediaType] = true
			}
		}
		if len(types) <= 1 {
			out = append(out, g)
			continue
		}
		out = append(out, groupDocs(g, func(x *dupDoc) string { return "m" + x.mediaType })...)
	}
	return out
}

// loadDuplicateDocs reads each document's row, its root body and, for the
// content basis, every chunk's body in tree order.
func (d *Document) loadDuplicateDocs(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, ids []string, withBodies bool) ([]*dupDoc, error) {
	var docs []*dupDoc
	for _, batch := range batchesOf(ids, copyBatch) {
		ph := strings.TrimSuffix(strings.Repeat("?,", len(batch)), ",")
		err := d.copyRows(ctx, key, `SELECT dd.id, dd.title, dd.root_chunk_id, COALESCE(c.type, ''), dd.created_at
			FROM documents dd LEFT JOIN chunks c ON c.id = dd.root_chunk_id
			WHERE dd.id IN (`+ph+`) AND dd.`+notMoving, "dd.id", anySlice(batch), func(_ []string, r []any) error {
			x := &dupDoc{id: asStr(r[0]), title: asStr(r[1]), rootID: asStr(r[2]), typ: asStr(r[3])}
			x.createdAt, _ = asInt64(r[4])
			x.titleKey, x.marked = duplicateTitleKey(x.title)
			docs = append(docs, x)
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	sort.Slice(docs, func(i, j int) bool { return docs[i].id < docs[j].id })
	for _, x := range docs {
		cb, err := d.readBody(ctx, mscope, key, x.rootID)
		if err != nil {
			return nil, err
		}
		x.rootBody = cb.Body
		if p := d.importedProvenance(ctx, mscope, key, x.rootID, cb); p != nil {
			x.mediaType, _ = p[importFieldMediaType].(string)
		}
	}
	if !withBodies {
		return docs, nil
	}
	chunks := map[string][]snapshotChunk{}
	for _, batch := range batchesOf(ids, copyBatch) {
		ph := strings.TrimSuffix(strings.Repeat("?,", len(batch)), ",")
		err := d.copyRows(ctx, key, `SELECT id, document_id, COALESCE(parent_id, ''), position FROM chunks WHERE document_id IN (`+ph+`)`,
			"document_id, position, id", anySlice(batch), func(_ []string, r []any) error {
				c := snapshotChunk{id: asStr(r[0]), docID: asStr(r[1]), parentID: asStr(r[2]), position: asInt(r[3])}
				chunks[c.docID] = append(chunks[c.docID], c)
				return nil
			})
		if err != nil {
			return nil, err
		}
	}
	for _, x := range docs {
		rows := chunks[x.id]
		order, _ := snapshotTreeOrder(x.rootID, rows)
		bodies := make([]string, 0, len(order))
		for _, i := range order {
			body := x.rootBody
			if id := rows[i].id; id != x.rootID {
				cb, err := d.readBody(ctx, mscope, key, id)
				if err != nil {
					return nil, err
				}
				body = cb.Body
			}
			bodies = append(bodies, body)
			if x.firstLine == "" {
				if line := firstTextLine(body); line != "" {
					x.firstChunk, x.firstLine = rows[i].id, line
				}
			}
		}
		x.hash = duplicateContentHash(bodies)
	}
	return docs, nil
}

// firstTextLine is body's first non-blank line, trimmed and cut to the longest
// quote evidence takes: still a verbatim substring of body.
func firstTextLine(body string) string {
	for _, line := range strings.Split(body, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			if utf8.RuneCountInString(line) > edgeEvidenceQuoteMax {
				line, _ = cutRunes(line, edgeEvidenceQuoteMax)
			}
			return line
		}
	}
	return ""
}
