package builtin

import (
	"context"
	"fmt"

	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
)

// document_subject_refs.go — the reader contract: a fact is reachable from EVERY
// subject it is about, not only the one it is filed under.
//
// Subject-homing files a fact under one subject, because containment is
// single-parent: the tree needs exactly one place for each chunk, and that is what
// makes a dossier one read. A fact about two things — "Dave works at the shop" —
// therefore lives in Dave's document and REFERENCES the shop. Read Dave and you see
// it; read the shop by walking its tree and you do not, even though the edge saying
// so has been there all along.
//
// That asymmetry is accepted by design and has to be PAID FOR by the readers. A
// subject's facts are its children PLUS the facts that reference it: one document
// read plus one indexed query, not a traversal. `chunk_edges_to_kind` (to_id, kind)
// exists for exactly this direction.
//
// WHY BOTH HALVES, and not the edge alone. Every fact the consolidation pass files
// also gets an `about` edge to its home subject, so edges alone would nearly always
// do. Nearly: the edge write is deliberately best-effort — "an unreachable fact is a
// retrieval gap, a missing fact is data loss, and only the second may fail a write"
// — and a `remember`-written fact carries its subject on the chunk itself and has no
// edge at all. Containment catches both. Using only containment loses the relation
// fact; using only the edge loses the operator's. The union is the honest answer.

// subjectRef is one fact that references a subject from outside its document.
type subjectRef struct {
	ID         string `json:"id"`
	Title      string `json:"title,omitempty"`
	Type       string `json:"type,omitempty"`
	DocumentID string `json:"document_id"`
	Kind       string `json:"kind"`
}

// subjectRefsCap bounds the reference block a document read carries. It is a
// SUMMARY — "these facts elsewhere are about this subject" — so it is capped low
// and says so when it clips; the caller that wants the whole set has
// `list_facts about:<entity chunk id>`, which pages.
const subjectRefsCap = 100

// documentOfChunk resolves the document a chunk belongs to. Returns ok=false when
// the chunk does not exist, which callers report rather than treating as an empty
// result: a filter naming a chunk that is not there is a caller mistake, and
// answering it with "no facts" hides that.
func (d *Document) documentOfChunk(ctx context.Context, key sqlmem.ScopeKey, chunkID string) (string, bool, error) {
	res, err := d.query(ctx, key, `SELECT document_id FROM chunks WHERE id = ?`, chunkID)
	if err != nil {
		return "", false, err
	}
	if len(res.Rows) == 0 {
		return "", false, nil
	}
	return asStr(res.Rows[0][0]), true, nil
}

// factsAboutSubjectSQL is the predicate "this chunk is a fact about that subject",
// stated once so the listing and any later reader cannot disagree about what a
// subject's facts are. Takes the subject's entity chunk id and the document it
// roots; returns the fragment and its arguments in placeholder order.
func factsAboutSubjectSQL(entityChunkID, documentID string) (string, []any) {
	return `(c.document_id = ? OR EXISTS (SELECT 1 FROM chunk_edges e ` +
			`WHERE e.from_id = c.id AND e.to_id = ? AND e.kind = ?))`,
		[]any{documentID, entityChunkID, aboutEdgeKind}
}

// inboundSubjectRefs lists the facts that reference this subject from ANOTHER
// document — the half of its fact set that walking its tree cannot reach.
//
// Same-document references are excluded deliberately: they are the subject's own
// children, already in the dossier, and repeating them would make the block read as
// "extra" when it is not.
func (d *Document) inboundSubjectRefs(ctx context.Context, key sqlmem.ScopeKey,
	entityChunkID, documentID string) ([]subjectRef, bool, error) {

	res, err := d.query(ctx, key,
		`SELECT c.id, coalesce(c.title, ''), coalesce(c.type, ''), c.document_id, e.kind
		   FROM chunk_edges e JOIN chunks c ON c.id = e.from_id
		  WHERE e.to_id = ? AND e.kind = ? AND c.document_id <> ?
		  ORDER BY c.id LIMIT ?`,
		entityChunkID, aboutEdgeKind, documentID, subjectRefsCap+1)
	if err != nil {
		return nil, false, err
	}
	rows := res.Rows
	truncated := len(rows) > subjectRefsCap || res.Truncated
	if len(rows) > subjectRefsCap {
		rows = rows[:subjectRefsCap]
	}
	out := make([]subjectRef, 0, len(rows))
	for _, r := range rows {
		if len(r) < 5 {
			continue
		}
		out = append(out, subjectRef{
			ID: asStr(r[0]), Title: asStr(r[1]), Type: asStr(r[2]),
			DocumentID: asStr(r[3]), Kind: asStr(r[4]),
		})
	}
	return out, truncated, nil
}

// subjectRefsNote is what a clipped reference block tells the caller to do about it.
func subjectRefsNote(entityChunkID string) string {
	return fmt.Sprintf("more than %d facts reference this subject from other documents; "+
		"list them all with list_facts about:%s", subjectRefsCap, entityChunkID)
}
