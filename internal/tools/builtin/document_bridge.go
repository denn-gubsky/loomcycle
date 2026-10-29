package builtin

// document_bridge.go — the document ↔ memory bridge (RFC FN P3a).
//
// A fact distilled from a document used to say only its quote: what it was taken
// from was a sentence of text, not a place one could go. So a fact could not be
// checked against the passage it came from once the passage changed, retrieval
// could not reach through from the fact to the clause, and deleting the document
// silently turned the fact's evidence into an orphan quote. Here a fact written
// with a source chunk carries a `derived_from` statement to it, with the quote
// and its span as evidence — the same evidence any relation carries.
//
// It also moves `about` — the edge that joins a fact to the subject it is about —
// onto the runtime's own write path: upsert_chunk takes the subjects as
// about_ids and writes the edges itself, so link_chunks can refuse a manual
// `about` from anything that is not a fact.

import (
	"context"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// migrateSourceGone adds chunk_memory_meta.source_gone: 1 once the chunk a fact
// was derived from has been deleted. Its quote was snapshotted onto the fact first,
// so the fact keeps its evidence; the flag says the place it came from is gone.
func (d *Document) migrateSourceGone(ctx context.Context, key sqlmem.ScopeKey) error {
	if d.tableHasColumn(ctx, key, "chunk_memory_meta", "source_gone") {
		return nil
	}
	return d.exec(ctx, key, `ALTER TABLE chunk_memory_meta ADD COLUMN source_gone INTEGER NOT NULL DEFAULT 0`)
}

// factSource is what upsert_chunk was told a fact came from, checked before the
// fact is written so a bad source refuses the whole write rather than leaving a
// fact without the link it was meant to carry.
type factSource struct {
	chunkID    string
	quote      string
	start, end int
}

// checkFactSource validates source_chunk_id (+ char_start/char_end, + the
// source_quote) against the source chunk's body. With a span and no quote, the
// quote is the span's text; with a quote, it must occur there (at the span when
// one is given).
func (d *Document) checkFactSource(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, in docInput) (*factSource, *tools.Result, error) {
	if in.SourceChunkID == "" {
		if in.CharStart != nil || in.CharEnd != nil {
			r := errValidation("upsert_chunk: char_start/char_end locate the quote in source_chunk_id, which is missing", "")
			return nil, &r, nil
		}
		return nil, nil, nil
	}
	readable, err := d.chunkReadable(ctx, key, in.SourceChunkID)
	if err != nil {
		return nil, nil, err
	}
	if !readable {
		r := errNotFound("upsert_chunk: source_chunk_id: no such chunk: "+in.SourceChunkID, "Pass the chunk of this scope the fact was taken from.")
		return nil, &r, nil
	}
	cb, err := d.readBody(ctx, mscope, key, in.SourceChunkID)
	if err != nil {
		return nil, nil, err
	}
	quote := in.SourceQuote
	if quote == "" {
		if in.CharStart == nil || in.CharEnd == nil {
			r := errValidation("upsert_chunk: a fact with source_chunk_id needs source_quote, or char_start and char_end", "Copy the sentence the fact was taken from as source_quote.")
			return nil, &r, nil
		}
		runes := []rune(cb.Body)
		s, e := *in.CharStart, *in.CharEnd
		if s < 0 || e <= s || e > len(runes) {
			r := errValidation("upsert_chunk: char_start/char_end are outside the source chunk's text", "")
			return nil, &r, nil
		}
		quote = string(runes[s:e])
	}
	s, e, found := quoteSpan(cb.Body, quote, in.CharStart, in.CharEnd)
	if !found {
		r := errValidation("upsert_chunk: evidence_quote_not_found — source_quote does not occur in source_chunk_id"+
			ifThen(in.CharStart != nil, " at char_start", ""),
			"Copy the sentence exactly from the chunk (op=get_chunk); a paraphrase is not a source.")
		return nil, &r, nil
	}
	return &factSource{chunkID: in.SourceChunkID, quote: quote, start: s, end: e}, nil, nil
}

func ifThen(c bool, a, b string) string {
	if c {
		return a
	}
	return b
}

// afterFactUpsert writes the edges a fact upsert asked for: `derived_from` to its
// source chunk, and `about` to each subject node in about_ids. Runtime writes, so
// the reserved kinds are the runtime's own; a subject that is not a chunk of this
// scope is skipped and counted, never fatal — a missing edge costs reachability
// from one subject, never the fact.
func (d *Document) afterFactUpsert(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, factID string, in docInput, src *factSource, out map[string]any) {
	if src != nil {
		s, e := src.start, src.end
		res, refuse, err := d.writeStatement(ctx, key, mscope, edgeWrite{
			from: factID, to: src.chunkID, kind: "derived_from", method: edgeMethodRuntime, status: edgeStatusAsserted,
			evidence: []edgeEvidenceIn{{ChunkID: src.chunkID, Quote: src.quote, CharStart: &s, CharEnd: &e}},
		})
		if err == nil && refuse == nil {
			out["derived_from"] = map[string]any{"statement_id": res.ID, "chunk_id": src.chunkID, "char_start": s, "char_end": e}
		}
	}
	if len(in.AboutIDs) == 0 {
		return
	}
	written, skipped := 0, 0
	seen := map[string]bool{}
	var linked []string
	for _, id := range in.AboutIDs {
		id = strings.TrimSpace(id)
		if id == "" || id == factID || seen[id] {
			continue
		}
		seen[id] = true
		if _, refuse, err := d.writeStatement(ctx, key, mscope, edgeWrite{
			from: factID, to: id, kind: aboutEdgeKind, method: edgeMethodRuntime, status: edgeStatusAsserted,
		}); err != nil || refuse != nil {
			skipped++
			continue
		}
		written++
		linked = append(linked, id)
	}
	out["about_written"] = written
	if skipped > 0 {
		out["about_skipped"] = skipped
	}
	if strings.TrimSpace(in.Relation) != "" {
		d.factRelation(ctx, key, mscope, factID, in, linked, out)
	}
}

// factRelation records the relation a fact states between its first two subjects
// as a statement (RFC FN P3b §6.3): subject —kind→ other subject, method
// consolidator, the fact chunk as evidence (its claim the quote), the fact's own
// confidence and validity. The fact stays a node; the statement is its index.
//
// A kind the vocabulary does not accept is still written, PROPOSED and marked
// ontology_valid=false (annotate mode); a reserved kind is skipped. A second live
// statement of the same kind from the same subject to a DIFFERENT one is counted
// as a possible contradiction and reported — nothing is retired, because
// automatic retirement has no measured false-retirement rate yet.
func (d *Document) factRelation(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, factID string, in docInput, linked []string, out map[string]any) {
	kind := strings.TrimSpace(in.Relation)
	if len(linked) < 2 {
		out["relation_skipped"] = "a relation needs two subjects the fact is about"
		return
	}
	claim := strings.TrimSpace(in.Body)
	w := edgeWrite{
		from: linked[0], to: linked[1], kind: kind, method: edgeMethodConsolidator, status: edgeStatusAsserted,
		confidence: in.Confidence, validAt: in.ValidAt, invalidAt: in.InvalidAt, proposeIfInvalid: true,
	}
	if claim != "" {
		w.evidence = []edgeEvidenceIn{{ChunkID: factID, Quote: claim}}
	}
	res, refuse, err := d.writeStatement(ctx, key, mscope, w)
	if err != nil {
		out["relation_skipped"] = "the relation could not be written"
		return
	}
	if refuse != nil {
		out["relation_skipped"] = refuse.Text
		return
	}
	rel := map[string]any{"statement_id": res.ID, "kind": kind, "status": res.Status}
	if !res.OntologyValid {
		rel["ontology_valid"] = false
	}
	out["relation"] = rel
	if n := d.relationConflicts(ctx, key, linked[0], kind, linked[1]); n > 0 {
		out["relation_conflicts"] = n
	}
}

// relationConflicts counts the live statements of kind from `from` to a subject
// other than `to`. Report-only.
func (d *Document) relationConflicts(ctx context.Context, key sqlmem.ScopeKey, from, kind, to string) int {
	res, err := d.query(ctx, key, `SELECT COUNT(*) FROM chunk_edges e
		WHERE e.from_id = ? AND e.kind = ? AND e.to_id <> ? AND e.status = '`+edgeStatusAsserted+`' AND `+edgeLiveClause, from, kind, to)
	if err != nil || len(res.Rows) == 0 {
		return 0
	}
	return asInt(res.Rows[0][0])
}

// snapshotDerivedQuotes runs before chunks are deleted: every fact OUTSIDE the
// deleted set that was derived from a chunk INSIDE it keeps the quote (copied onto
// its source_quote when it had none) and is marked source_gone. Retention's rule
// for a transcript, applied to a document: snapshot the span before its source
// leaves. inSet is a SQL condition on a chunk id column written as {col}, with
// its args.
func (d *Document) snapshotDerivedQuotes(ctx context.Context, txnID, inSet string, args ...any) error {
	cond := strings.ReplaceAll(inSet, "{col}", "e.to_id")
	factCond := strings.ReplaceAll(inSet, "{col}", "chunk_memory_meta.chunk_id")
	stmt := `UPDATE chunk_memory_meta SET source_gone = 1,
		source_quote = COALESCE(NULLIF(source_quote, ''), (
			SELECT ev.quote FROM chunk_edges e JOIN chunk_edge_evidence ev ON ev.edge_id = e.id
			 WHERE e.from_id = chunk_memory_meta.chunk_id AND e.kind = 'derived_from' AND ` + cond + `
			 ORDER BY ev.created_at LIMIT 1))
		WHERE chunk_id IN (SELECT e.from_id FROM chunk_edges e WHERE e.kind = 'derived_from' AND ` + cond + `)
		  AND NOT (` + factCond + `)`
	all := append(append(append([]any{}, args...), args...), args...)
	return d.execTxn(ctx, txnID, stmt, all...)
}

// sourceStillHolds reports whether a fact's derived_from quote still occurs in its
// source chunk. ok=false with checked=true means the source changed under the
// fact (an empty or unreadable body counts as changed); checked=false means there
// is nothing to check it against.
//
// A DELETED source is not a changed one. Its quote was snapshotted onto the fact
// (source_gone) before the passage left — retention's rule for an archived chat —
// so a judge still has the span the claim rests on; what this guards against is a
// passage that now says something else.
//
// A source the CALLER may not read is not checked either: refusing would tell
// them whether text they cannot see still occurs in a document they cannot open.
func (d *Document) sourceStillHolds(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, factID string) (ok, checked bool, err error) {
	res, err := d.query(ctx, key, `SELECT e.id, e.to_id, ev.quote FROM chunk_edges e
		JOIN chunk_edge_evidence ev ON ev.edge_id = e.id
		WHERE e.from_id = ? AND e.kind = 'derived_from' ORDER BY ev.created_at LIMIT 1`, factID)
	if err != nil || len(res.Rows) == 0 {
		return true, false, err
	}
	edgeID, chunkID, quote := asStr(res.Rows[0][0]), asStr(res.Rows[0][1]), asStr(res.Rows[0][2])
	if readable, rerr := d.chunkReadable(ctx, key, chunkID); rerr != nil || !readable {
		return true, false, rerr
	}
	cb, err := d.readBody(ctx, mscope, key, chunkID)
	if err != nil {
		return true, false, err
	}
	if strings.Contains(cb.Body, quote) {
		return true, true, nil
	}
	_ = d.exec(ctx, key, `UPDATE chunk_edge_evidence SET stale = 1 WHERE edge_id = ? AND chunk_id = ?`, edgeID, chunkID)
	return false, true, nil
}

// rememberAboutTarget finds the entity node a remembered fact's subject names,
// when the scope already has one: a subject node (subjectNodesWhere — the same
// test a [[link]] uses to call itself a mention) carrying that subject, of the
// remembered type when one was given. remember never mints a subject — that is
// the curator's call — so with no node there is no edge, as before.
func (d *Document) rememberAboutTarget(ctx context.Context, key sqlmem.ScopeKey, subject, typ string) string {
	if strings.TrimSpace(subject) == "" {
		return ""
	}
	q := subjectNodesWhere + ` AND lower(subject) = lower(?)`
	args := []any{strings.TrimSpace(subject)}
	if t := strings.TrimSpace(typ); t != "" {
		q += ` AND natural_key LIKE ?`
		args = append(args, t+":%")
	}
	q += ` ORDER BY created_at LIMIT 1`
	res, err := d.query(ctx, key, q, args...)
	if err != nil || len(res.Rows) == 0 {
		return ""
	}
	return asStr(res.Rows[0][0])
}
