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
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

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
	// derived_from is an edge to the source, so the source is held to what
	// link_chunks asks of an endpoint; one the caller may not link answers
	// exactly like a missing chunk.
	linkable, err := d.endpointLinkable(ctx, key, d.newDocACL(ctx, key), in.SourceChunkID)
	if err != nil {
		return nil, nil, err
	}
	if !linkable {
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
	// Checked here rather than left to the evidence write, which would refuse it
	// after the fact was stored and leave the fact without its derived_from.
	if utf8.RuneCountInString(quote) > edgeEvidenceQuoteMax {
		r := errValidation(fmt.Sprintf("upsert_chunk: source_quote is longer than %d characters", edgeEvidenceQuoteMax),
			"Quote the sentence the fact was taken from, not the whole passage.")
		return nil, &r, nil
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

// endpointLinkable reports whether chunkID exists and the caller may make it an
// endpoint of an edge: read and link on its document, what link_chunks asks of
// both ends. A missing chunk and a denied one answer alike, so a skip or refusal
// built on this never says which it was. acl nil: no set, nothing is denied.
func (d *Document) endpointLinkable(ctx context.Context, key sqlmem.ScopeKey, acl *docACL, chunkID string) (bool, error) {
	row, ok, err := d.getChunkRow(ctx, key, chunkID)
	if err != nil || !ok || acl == nil {
		return ok, err
	}
	for _, verb := range []string{"read", "link"} {
		r, err := acl.document(ctx, "upsert_chunk", verb, row.DocumentID)
		if err != nil || r != nil {
			return false, err
		}
	}
	return true, nil
}

// afterFactUpsert writes the edges a fact upsert asked for: `derived_from` to its
// source chunk, and `about` to each subject node in about_ids. Runtime writes, so
// the reserved kinds are the runtime's own; a subject that is not a chunk of this
// scope, or one the caller may not link, is skipped and counted, never fatal — a
// missing edge costs reachability from one subject, never the fact.
//
// about_ids RESTATES what the fact is about: an `about` edge to a subject no
// longer named, and a relation the fact no longer states, stop being live. An
// upsert without about_ids (a body-only edit) leaves them as they were. The
// retirement is not asked of the ACL: the edges are the fact's own, derived from
// it, and the caller just wrote the fact.
func (d *Document) afterFactUpsert(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, factID string, in docInput, src *factSource, out map[string]any) {
	if src != nil {
		d.writeDerivedFrom(ctx, key, mscope, factID, src, out)
	}
	if len(in.AboutIDs) == 0 {
		return
	}
	acl := d.newDocACL(ctx, key)
	written, skipped := 0, 0
	named := map[string]bool{}
	var given []string
	linked := map[string]bool{}
	for _, id := range in.AboutIDs {
		id = strings.TrimSpace(id)
		if id == "" || id == factID || named[id] {
			continue
		}
		named[id] = true
		given = append(given, id)
		if ok, err := d.endpointLinkable(ctx, key, acl, id); err != nil || !ok {
			skipped++
			continue
		}
		res, refuse, err := d.writeStatement(ctx, key, mscope, edgeWrite{
			from: factID, to: id, kind: aboutEdgeKind, method: edgeMethodRuntime, status: edgeStatusAsserted,
		})
		if err != nil || refuse != nil {
			skipped++
			continue
		}
		d.reviveFactEdge(ctx, key, res.ID)
		written++
		linked[id] = true
	}
	d.retireUnnamedAbout(ctx, key, factID, named)
	out["about_written"] = written
	if skipped > 0 {
		out["about_skipped"] = skipped
	}
	keep, ok := "", true
	if strings.TrimSpace(in.Relation) != "" {
		keep, ok = d.factRelation(ctx, key, mscope, acl, factID, in, given, linked, out)
	}
	if ok {
		d.releaseFactRelations(ctx, key, factID, keep, strings.TrimSpace(in.Body))
	}
}

// writeDerivedFrom links a fact to the passage it was taken from. A fact has ONE
// live source: re-derived from another passage, the edge to the old one goes, and
// re-derived from the same passage, so does the evidence for a quote it no longer
// rests on — otherwise the judge, the deletion snapshot and recall would each read
// whichever source happened to be oldest.
func (d *Document) writeDerivedFrom(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, factID string, src *factSource, out map[string]any) {
	s, e := src.start, src.end
	res, refuse, err := d.writeStatement(ctx, key, mscope, edgeWrite{
		from: factID, to: src.chunkID, kind: "derived_from", method: edgeMethodRuntime, status: edgeStatusAsserted,
		evidence: []edgeEvidenceIn{{ChunkID: src.chunkID, Quote: src.quote, CharStart: &s, CharEnd: &e}},
	})
	if err != nil || refuse != nil {
		return
	}
	// Removed only once the new edge is written, so a failed write keeps the old source.
	if d.deleteEdgesWhere(ctx, key, "", `from_id = ? AND kind = 'derived_from' AND id <> ?`, factID, res.ID) != nil ||
		d.exec(ctx, key, `DELETE FROM chunk_edge_evidence WHERE edge_id = ? AND quote <> ?`, res.ID, src.quote) != nil {
		return
	}
	out["derived_from"] = map[string]any{"statement_id": res.ID, "chunk_id": src.chunkID, "char_start": s, "char_end": e}
}

// reviveFactEdge makes live again an edge the fact names once more after an
// earlier upsert retired it. Only the fact's own runtime and consolidator edges
// are ever retired, so only those are revived.
func (d *Document) reviveFactEdge(ctx context.Context, key sqlmem.ScopeKey, edgeID string) {
	_ = d.exec(ctx, key, `UPDATE chunk_edges SET expired_at = NULL
		WHERE id = ? AND expired_at IS NOT NULL AND method IN ('`+edgeMethodRuntime+`', '`+edgeMethodConsolidator+`')`, edgeID)
}

// retireUnnamedAbout retires the fact's runtime `about` edges to subjects the
// upsert did not name. Best-effort, like the edges themselves.
func (d *Document) retireUnnamedAbout(ctx context.Context, key sqlmem.ScopeKey, factID string, named map[string]bool) {
	stmt := `UPDATE chunk_edges SET expired_at = ? WHERE from_id = ? AND kind = '` + aboutEdgeKind + `'
		AND method = '` + edgeMethodRuntime + `' AND expired_at IS NULL`
	args := []any{time.Now().UnixNano(), factID}
	if len(named) > 0 {
		stmt += ` AND to_id NOT IN (` + placeholders(len(named)) + `)`
		for id := range named {
			args = append(args, id)
		}
	}
	_ = d.exec(ctx, key, stmt, args...)
}

// factRelation records the relation a fact states between the first two subjects
// the caller named as a statement (RFC FN P3b §6.3): subject —kind→ other subject,
// method consolidator, the fact chunk as evidence (its claim the quote), the
// fact's own confidence and validity. The fact stays a node; the statement is its
// index. When either of those two could not be linked, the relation is skipped
// rather than drawn between whichever two happened to be written.
//
// A kind the vocabulary does not accept is still written, PROPOSED and marked
// ontology_valid=false (annotate mode); a reserved kind is skipped. A second live
// statement of the same kind from the same subject to a DIFFERENT one is counted
// as a possible contradiction and reported — nothing is retired, because
// automatic retirement has no measured false-retirement rate yet.
//
// It returns the statement it wrote ("" for none) and whether the fact's other
// relations may be released: not after a store fault, which says nothing about
// what the fact states.
func (d *Document) factRelation(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, acl *docACL, factID string, in docInput, given []string, linked map[string]bool, out map[string]any) (string, bool) {
	kind := strings.TrimSpace(in.Relation)
	if len(given) < 2 {
		out["relation_skipped"] = "a relation needs two subjects the fact is about"
		return "", true
	}
	from, to := given[0], given[1]
	if !linked[from] || !linked[to] {
		out["relation_skipped"] = "a subject of the relation could not be linked"
		return "", true
	}
	claim := strings.TrimSpace(in.Body)
	w := edgeWrite{
		from: from, to: to, kind: kind, method: edgeMethodConsolidator, status: edgeStatusAsserted,
		confidence: in.Confidence, validAt: in.ValidAt, invalidAt: in.InvalidAt, proposeIfInvalid: true,
	}
	if claim != "" {
		w.evidence = []edgeEvidenceIn{{ChunkID: factID, Quote: claim}}
	}
	res, refuse, err := d.writeStatement(ctx, key, mscope, w)
	if err != nil {
		out["relation_skipped"] = "the relation could not be written"
		return "", false
	}
	if refuse != nil {
		// A rejection's own text carries the operator's reason, which is not this
		// caller's to read; every other refusal describes the caller's input.
		out["relation_skipped"] = refuse.Text
		if refuse.Error != nil && refuse.Error.Category == tools.CategoryBusiness {
			out["relation_skipped"] = "an operator rejected this relation, so it is not recorded again"
		}
		return "", true
	}
	d.reviveFactEdge(ctx, key, res.ID)
	rel := map[string]any{"statement_id": res.ID, "kind": kind, "status": res.Status}
	if !res.OntologyValid {
		rel["ontology_valid"] = false
	}
	out["relation"] = rel
	if n := d.relationConflicts(ctx, key, acl, factID, from, kind, to); n > 0 {
		out["relation_conflicts"] = n
	}
	return res.ID, true
}

// relationConflicts counts the live statements of kind from `from` to a subject
// other than `to` that the caller may read, leaving out those this fact itself
// evidences. Report-only.
func (d *Document) relationConflicts(ctx context.Context, key sqlmem.ScopeKey, acl *docACL, factID, from, kind, to string) int {
	res, err := d.query(ctx, key, `SELECT e.to_id FROM chunk_edges e
		WHERE e.from_id = ? AND e.kind = ? AND e.to_id <> ? AND e.status = '`+edgeStatusAsserted+`' AND `+edgeLiveClause+`
		  AND NOT EXISTS (SELECT 1 FROM chunk_edge_evidence ev WHERE ev.edge_id = e.id AND ev.chunk_id = ?)`, from, kind, to, factID)
	if err != nil {
		return 0
	}
	n := 0
	for _, r := range res.Rows {
		if acl != nil {
			row, ok, err := d.getChunkRow(ctx, key, asStr(r[0]))
			if err != nil || !ok {
				continue
			}
			if readable, err := acl.readable(ctx, row.DocumentID); err != nil || !readable {
				continue
			}
		}
		n++
	}
	return n
}

// releaseFactRelations detaches a fact from the consolidator relations it no
// longer states — every one but keep — and retires those no other live source
// still supports. It also drops the fact's evidence on keep that quotes an older
// wording, so a relation's evidence from a fact is what the fact says now. A
// manual statement the fact happened to evidence is left alone: it stands on its
// author's word, not the fact's. Best-effort.
func (d *Document) releaseFactRelations(ctx context.Context, key sqlmem.ScopeKey, factID, keep, claim string) {
	if d.expireUnsupportedRelations(ctx, key, "", keep, `{col} = ?`, factID) != nil {
		return
	}
	_ = d.exec(ctx, key, `DELETE FROM chunk_edge_evidence WHERE chunk_id = ?
		AND edge_id IN (SELECT id FROM chunk_edges WHERE method = '`+edgeMethodConsolidator+`' AND from_id <> ? AND to_id <> ?)
		AND (edge_id <> ? OR quote <> ?)`, factID, factID, factID, keep, claim)
}

// expireUnsupportedRelations retires each live consolidator relation evidenced by
// a chunk in the set — except the statement `except` — that nothing outside the
// set still supports: no evidence from a chunk outside it that still exists and
// is not a retired fact, and none from a chat turn or another scope. Run when
// facts are retired or deleted, so a relation lives exactly as long as some fact
// still states it.
// inSet is a SQL condition on a chunk id column written as {col}, with its args,
// as for snapshotDerivedQuotes; txnID "" runs outside a transaction.
func (d *Document) expireUnsupportedRelations(ctx context.Context, key sqlmem.ScopeKey, txnID, except, inSet string, args ...any) error {
	stmt := `UPDATE chunk_edges SET expired_at = ?
		WHERE method = '` + edgeMethodConsolidator + `' AND expired_at IS NULL AND id <> ?
		  AND id IN (SELECT ev.edge_id FROM chunk_edge_evidence ev WHERE ` + strings.ReplaceAll(inSet, "{col}", "ev.chunk_id") + `)
		  AND NOT EXISTS (SELECT 1 FROM chunk_edge_evidence o WHERE o.edge_id = chunk_edges.id
		        AND (o.chunk_id IS NULL OR (NOT (` + strings.ReplaceAll(inSet, "{col}", "o.chunk_id") + `)
		             AND EXISTS (SELECT 1 FROM chunks c WHERE c.id = o.chunk_id)
		             AND NOT EXISTS (SELECT 1 FROM chunk_memory_meta m WHERE m.chunk_id = o.chunk_id AND m.expired_at IS NOT NULL))))`
	all := append(append([]any{time.Now().UnixNano(), except}, args...), args...)
	if txnID != "" {
		return d.execTxn(ctx, txnID, stmt, all...)
	}
	return d.exec(ctx, key, stmt, all...)
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
	// The newest edge and its newest quote: a fact written before facts kept a
	// single source may still carry an older one.
	res, err := d.query(ctx, key, `SELECT e.id, e.to_id, ev.quote FROM chunk_edges e
		JOIN chunk_edge_evidence ev ON ev.edge_id = e.id
		WHERE e.from_id = ? AND e.kind = 'derived_from' ORDER BY e.created_at DESC, ev.created_at DESC LIMIT 1`, factID)
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
