package builtin

// document_path_listing.go — what Path ls asks of the Document tool for
// with_counts and the listing filters (RFC HA): which documents the caller may
// read, and their title, type and status. Dirents live in the main store and
// document metadata in the scope's SQL Memory, so the two cannot be joined in one
// query; Path walks the names it already loaded and asks here in batches.

import (
	"context"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
)

// pathFactsBatch is how many document ids one metadata query names: well under
// both engines' bound on bound parameters, and the batch the rest of the tool
// reads documents in.
const pathFactsBatch = 500

// pathIndexedNames is how many documents one request asks the readability of
// through the reverse name index before it lists the whole scope's names once
// instead. Measured on a 100,000-document tenant with a path-scoped rule: the
// index costs about 230µs a document (11.5s for 50,000), one listing of the
// scope about 0.7s and then 5µs a document. Below a few thousand documents the
// index wins; past that the listing does.
const pathIndexedNames = 2000

// pathDocumentSet is one ls request's view of a scope's documents.
type pathDocumentSet struct {
	d       *Document
	key     sqlmem.ScopeKey
	acl     *docACL
	limited bool
	asked   int // documents asked Readable about so far
}

// PathDocuments implements PathDocumentSource. It answers false when document
// metadata cannot be queried for the scope — no SQL Memory, a scope the run may
// not address as Document does, or a store that will not answer — and Path then
// leaves counts out rather than report a zero it did not compute.
func (d *Document) PathDocuments(ctx context.Context, scope string) (PathDocumentLookup, bool) {
	if d == nil || d.Store == nil || d.SqlMem == nil {
		return nil, false
	}
	key, _, err := d.resolveScope(ctx, scope)
	if err != nil {
		return nil, false
	}
	if _, err := d.query(ctx, key, `SELECT 1 FROM documents LIMIT 1`); err != nil {
		return nil, false
	}
	acl := d.newDocACL(ctx, key)
	// A listing usually asks about a small subtree, so names are first looked up
	// for those documents only (Readable switches to one listing of the scope
	// when the subtree is large).
	acl.useIndex()
	limited, err := acl.limitsReading(ctx)
	if err != nil {
		return nil, false
	}
	return &pathDocumentSet{d: d, key: key, acl: acl, limited: limited}, true
}

// Readable keeps, in order, the documents of ids the caller may read at every
// name they are filed under — the rule every Document listing applies.
//
// It does not ask the annotation-document question (docACL.readable does):
// Facts never returns a folder's annotation document, so whether one is
// withheld changes no count and no match.
func (s *pathDocumentSet) Readable(ctx context.Context, ids []string) ([]string, error) {
	if !s.limited {
		return ids, nil
	}
	s.asked += len(ids)
	if s.asked > pathIndexedNames && s.acl != nil {
		// Large: from here on the names come from one listing of the scope,
		// loaded on the next lookup and kept for the rest of the request.
		s.acl.indexed = false
	}
	if err := s.acl.primeNames(ctx, ids); err != nil {
		return nil, err
	}
	kept := make([]string, 0, len(ids))
	for _, id := range ids {
		ok, err := s.acl.readableByName(ctx, id)
		if err != nil {
			return nil, err
		}
		if ok {
			kept = append(kept, id)
		}
	}
	return kept, nil
}

// Facts returns the title, type and status of each document of ids that exists
// in the scope. A document mid-move into the scope is left out, as
// query_documents leaves it out, and so is a folder's annotation document: it is
// the folder's description, not a document in it, and the explorer shows it as
// the folder's card rather than as a row (RFC GZ), so a count that included it
// would disagree with the rows a reader sees.
func (s *pathDocumentSet) Facts(ctx context.Context, ids []string) (map[string]PathDocumentFact, error) {
	out := make(map[string]PathDocumentFact, len(ids))
	for start := 0; start < len(ids); start += pathFactsBatch {
		batch := ids[start:min(start+pathFactsBatch, len(ids))]
		ph, args := inPlaceholders(batch)
		// Keyset over id, because the store caps the rows one query returns: a cap
		// below the batch cuts the answer short, and the next query resumes after
		// the last id it returned instead of losing the rest.
		after := ""
		for {
			q := `SELECT id, title, type, status FROM documents WHERE id IN (` + ph + `) AND ` + notMoving
			qargs := append([]any{}, args...)
			if after != "" {
				q += ` AND id > ?`
				qargs = append(qargs, after)
			}
			res, err := s.d.query(ctx, s.key, q+` ORDER BY id`, qargs...)
			if err != nil {
				return nil, err
			}
			for _, r := range res.Rows {
				id, typ := asStr(r[0]), asStr(r[2])
				if strings.EqualFold(typ, annotationDocType) {
					continue
				}
				out[id] = PathDocumentFact{Title: asStr(r[1]), Type: typ, Status: asStr(r[3])}
			}
			if !res.Truncated || len(res.Rows) == 0 {
				break
			}
			after = asStr(res.Rows[len(res.Rows)-1][0])
		}
	}
	return out, nil
}

// ExpandType is the type filter's set: the type and its subtypes, as the
// Document tool's type filters read it.
func (s *pathDocumentSet) ExpandType(ctx context.Context, typ string) []string {
	if exp := s.d.expandTypeFilter(ctx, typ); len(exp) > 0 {
		return exp
	}
	return []string{typ}
}
