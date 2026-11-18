package builtin

// document_ontology_delete.go — an ontology entry that something rests on is retired,
// never deleted (RFC GQ D3, plan phase P2.2).
//
// FOR EVERY CALLER. guardOntologyWrite refuses an agent's delete on the ontology before
// any handler runs, but the operator plane skips that guard by design (the Web UI
// editor, POST /v1/_document). So the rule lives inside the delete paths themselves:
// delete_chunk, delete_document and DeleteOntologyChunk ask ontologyDeleteGuard before
// they remove anything, whoever is calling.
//
// ONE RULE. "Has dependents" is the plan's own answer (RFC GQ P1): each in-force entry
// the delete would remove is planned as op=delete, and the delete is refused when any
// of them comes back risk "destroys" — a row keyed by a chunk id of its subtree would
// go with it, or a row keyed by its name would be left naming nothing. No second
// counter. An entry nothing rests on may still be deleted; a proposal, a section
// header with no entries in use, a grammar and the root are not entries.
//
// FAILS CLOSED. Not knowing which document is the ontology, a read that errors, or a
// plan that could not count every user and agent scope in its budget refuses the
// delete: an incomplete count of zero is not "no dependents".
//
// THE WINDOW. The plan reads outside the delete's transaction (on SQLite a scope has
// one connection, so it cannot run inside it). delete_chunk and delete_document close
// the cheap half of it: inside the transaction, ontologyDeleteRecheck re-asserts that
// the chunks going are exactly the ones the guard read, with the statuses it read, and
// that the subtrees it found quiet have gained no statement.
//
// THE WAYS AROUND IT, closed here too: an entry taken out of the read first and then
// deleted as a non-entry — demoted to an inert status (ontologyDemotionRefusal), moved
// where the reader stops (ontologyMoveRefusal) — or closed by a supersede so that the
// retention prune deletes it unguarded (ontologyCloseRefusal; PruneRetiredChunks also
// skips the ontology document).

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/connector"
	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// entryHasDependentsCode is the refusal's code, at the start of its text as the other
// ontology refusals carry theirs (entry_retired).
const entryHasDependentsCode = "entry_has_dependents"

// ontologyDeleteNamed bounds how many entries a refusal names; the rest are counted.
const ontologyDeleteNamed = 5

// ontologyDeleteCheck is what the guard read and approved, re-asserted inside the
// delete's transaction (ontologyDeleteRecheck): every chunk of the subtree with its
// status, and the chunks the plan found no statement on.
type ontologyDeleteCheck struct {
	statuses map[string]string
	quiet    []string
}

// ontologyDeleteGuard is asked by every path that deletes chunks of a document in the
// tenant scope. top is the chunk whose subtree goes ("" = the whole document). It
// returns a refusal when the subtree holds an in-force entry of the tenant ontology
// that has dependents. When the delete may go ahead on a chunk of the ontology, check
// is what it approved, for the caller to re-assert inside its transaction; nil outside
// the ontology.
func (d *Document) ontologyDeleteGuard(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, op, docID, top string) (refused *tools.Result, check *ontologyDeleteCheck) {
	if key.Scope != "tenant" {
		return nil, nil
	}
	fail := func(err error) (*tools.Result, *ontologyDeleteCheck) {
		r := errFrom(op+": could not tell whether the ontology entries it would remove are in use, so nothing was deleted: "+err.Error(), err)
		return &r, nil
	}
	rctx := noWrites(ctx)
	ontID, err := d.resolveOntologyID(rctx, key)
	if err != nil {
		return fail(err)
	}
	if ontID == "" || ontID != docID {
		return nil, nil
	}
	read, err := d.ontologyForKey(rctx, key, mscope, memrank.OntologyPath, nil)
	if err != nil {
		return fail(err)
	}
	rows, err := d.planChunkRows(rctx, key, ontID)
	if err != nil {
		return fail(err)
	}
	all, err := d.ontologyChunkStatuses(rctx, key, ontID)
	if err != nil {
		return fail(err)
	}
	// Only the subtree going: a chunk elsewhere in the document that moves under the
	// target between here and the transaction must read as arrived.
	check = &ontologyDeleteCheck{statuses: all}
	if top != "" {
		check.statuses = map[string]string{}
		for _, id := range rows.subtree(top) {
			check.statuses[id] = all[id]
		}
	}
	entries := planEntries(read)
	// A lock refuses the delete whether or not anything rests on the entry (RFC GQ P2.3).
	if locked := lockedIn(deletedChunks(rows, top), entries); len(locked) > 0 {
		r := lockedRefusal(op, top, "deleted", locked)
		return &r, nil
	}
	tops := topEntries(rows, entries, top)
	if len(tops) == 0 {
		return nil, check // a proposal, a grammar, an empty section: no entry goes
	}
	var inUse []connector.OntologyPlannedChange
	for start := 0; start < len(tops); start += ontologyPlanMaxChanges {
		var changes []connector.OntologyChange
		for _, id := range tops[start:min(start+ontologyPlanMaxChanges, len(tops))] {
			changes = append(changes, connector.OntologyChange{Op: connector.OntologyPlanDelete, Entry: id})
		}
		plan, err := d.planOntologyAt(ctx, key, mscope, changes)
		if err != nil {
			return fail(err)
		}
		if !plan.Complete {
			r := errTransient(op+": "+entryHasDependentsCode+": the ontology entries this would remove could not be checked "+
				"against every user and agent scope of the tenant in time ("+strconv.Itoa(plan.ScopesSkipped)+
				" scope(s) not counted), so nothing was deleted", "Try again; if it keeps happening, retire the entry instead (status \"retired\").")
			return &r, nil
		}
		for _, ch := range plan.Changes {
			if ch.Risk == connector.OntologyRiskDestroys {
				inUse = append(inUse, ch)
			}
		}
	}
	if len(inUse) == 0 {
		for _, t := range tops {
			check.quiet = append(check.quiet, rows.subtree(t)...)
		}
		return nil, check
	}
	r := entryInUseRefusal(op, top == "", inUse)
	return &r, nil
}

// ontologyDemotionRefusal refuses update_chunk setting an inert status (proposed,
// rejected) on a chunk of the tenant ontology that is not one already — an entry, a
// retired entry or a section. The reader drops an inert chunk with its subtree, so a
// demoted entry would read as a non-entry and delete_chunk would pass it: retirement is
// the one way out of force (RFC GQ D3, D4). The root keeps its own status words; a
// proposal still moves between proposed and rejected. For every caller; fails closed.
func (d *Document) ontologyDemotionRefusal(ctx context.Context, key sqlmem.ScopeKey, row chunkRow) *tools.Result {
	if key.Scope != "tenant" || row.ParentID == "" {
		return nil
	}
	ontID, err := d.resolveOntologyID(ctx, key)
	if err != nil {
		r := errFrom("update_chunk: could not tell whether the chunk is in the tenant ontology, so its status was not changed: "+err.Error(), err)
		return &r
	}
	if ontID == "" || ontID != row.DocumentID {
		return nil
	}
	r := errValidation("update_chunk: an entry or section of the tenant ontology cannot be made a proposal: that status "+
		"takes it and everything under it out of the ontology while what uses them stays behind; retire it instead",
		"Set status \"retired\": what uses the entry is kept and readable, and nothing new may use it.")
	return &r
}

// ontologyMoveRefusal refuses move_chunk taking an in-force entry of the tenant
// ontology where the reader no longer finds it — under a proposal, a rejection or a
// grammar, past a depth the reader keeps. Such an entry reads as a non-entry, so a
// delete would pass it (RFC GQ D3). Judged by reading the ontology with the move
// applied in memory (the plan's overlay), so it is the reader's own rule, not a copy.
// For every caller; fails closed.
func (d *Document) ontologyMoveRefusal(ctx context.Context, key sqlmem.ScopeKey, row chunkRow, newParent string) *tools.Result {
	if key.Scope != "tenant" {
		return nil
	}
	fail := func(err error) *tools.Result {
		r := errFrom("move_chunk: could not tell whether the move takes an ontology entry out of force, so nothing was moved: "+err.Error(), err)
		return &r
	}
	rctx := noWrites(ctx)
	ontID, err := d.resolveOntologyID(rctx, key)
	if err != nil {
		return fail(err)
	}
	if ontID == "" || ontID != row.DocumentID {
		return nil
	}
	before, err := d.ontologyForKey(rctx, key, store.MemoryScopeTenant, memrank.OntologyPath, nil)
	if err != nil {
		return fail(err)
	}
	ov := newOntologyOverlay()
	ov.parents[row.ID] = newParent
	after, err := d.ontologyForKey(rctx, key, store.MemoryScopeTenant, memrank.OntologyPath, ov)
	if err != nil {
		return fail(err)
	}
	rows, err := d.planChunkRows(rctx, key, ontID)
	if err != nil {
		return fail(err)
	}
	was, now := planEntries(before), planEntries(after)
	// A lock refuses any move of the entry or of a chunk above it (RFC GQ P2.3), even
	// one the reader would follow.
	if locked := lockedIn(rows.subtree(row.ID), was); len(locked) > 0 {
		r := lockedRefusal("move_chunk", row.ID, "moved", locked)
		return &r
	}
	var lost []string
	for _, id := range rows.subtree(row.ID) {
		if e, ok := was[id]; ok {
			if _, still := now[id]; !still {
				lost = append(lost, e.Kind+" "+strconv.Quote(e.Name))
			}
		}
	}
	if len(lost) == 0 {
		return nil
	}
	r := errValidation("move_chunk: the move would take "+strings.Join(lost, ", ")+" out of the tenant ontology (under "+
		"a proposal, a rejection or a grammar, or past the depth the reader keeps), so nothing was moved; retire an entry instead",
		"Move the entry under its section or another in-force entry, or set status \"retired\" on it.")
	return &r
}

// errOntologyChunkClosed is retireChunk's refusal to close a chunk of the tenant
// ontology.
var errOntologyChunkClosed = errors.New("a chunk of the tenant ontology is never closed or superseded: the retention " +
	"prune deletes closed chunks; retire an entry instead (update_chunk with status \"retired\")")

// ontologyCloseRefusal refuses closing (superseding) chunkID when it is a chunk of the
// tenant ontology document: a closed chunk is what PruneRetiredChunks deletes, past
// the D3 guard. Asked by retireChunk, the one writer of a closure, so Document
// supersede_chunk and Memory supersede both meet it. Fails closed.
func (d *Document) ontologyCloseRefusal(ctx context.Context, key sqlmem.ScopeKey, chunkID string) error {
	if key.Scope != "tenant" {
		return nil
	}
	ontID, err := d.resolveOntologyID(ctx, key)
	if err != nil {
		return fmt.Errorf("could not tell whether the chunk is in the tenant ontology, so it was not closed: %w", err)
	}
	if ontID == "" {
		return nil
	}
	docID, err := d.chunkDocumentIDOrErr(ctx, key, chunkID)
	if err != nil {
		return fmt.Errorf("could not tell whether the chunk is in the tenant ontology, so it was not closed: %w", err)
	}
	if docID == ontID {
		return errOntologyChunkClosed
	}
	return nil
}

// chunkDocumentIDOrErr is the document chunkID belongs to: "" with no error only when
// the scope has no such chunk. documentIDForChunk answers "" on a failed read too, which
// a guard must not take for "not in the ontology". A raw read: d.query re-provisions a
// missing table and answers "no row", which would read as a confirmed miss.
func (d *Document) chunkDocumentIDOrErr(ctx context.Context, key sqlmem.ScopeKey, chunkID string) (string, error) {
	res, err := d.sqlRead(ctx, key, `SELECT document_id FROM chunks WHERE id = ? LIMIT 1`, []any{chunkID})
	if err != nil {
		return "", err
	}
	if len(res.Rows) == 0 || len(res.Rows[0]) == 0 {
		return "", nil
	}
	return asStr(res.Rows[0][0]), nil
}

// ontologyChunkStatuses is every chunk of the ontology document with its status.
func (d *Document) ontologyChunkStatuses(ctx context.Context, key sqlmem.ScopeKey, ontID string) (map[string]string, error) {
	res, err := d.query(ctx, key, `SELECT id, coalesce(status, '') FROM chunks WHERE document_id = ?`, ontID)
	if err != nil {
		return nil, err
	}
	if res.Truncated {
		return nil, fmt.Errorf("the ontology document has more chunks than one read returns")
	}
	out := make(map[string]string, len(res.Rows))
	for _, r := range res.Rows {
		if len(r) >= 2 {
			out[asStr(r[0])] = asStr(r[1])
		}
	}
	return out, nil
}

// ontologyDeleteRecheck re-asserts, inside the delete's transaction, what the guard
// approved: ids (the chunks the transaction is about to remove) are all chunks the
// guard read, each with the status it read — so no entry arrived under the target by a
// move or a new chunk, and none changed status — and none of the chunks the plan found
// quiet has gained a statement. why is "" when the delete may go ahead.
func (d *Document) ontologyDeleteRecheck(ctx context.Context, txnID string, check *ontologyDeleteCheck, ids []string) (why string, err error) {
	if check == nil {
		return "", nil
	}
	for _, batch := range batchIDs(ids, graphIDBatch) {
		if len(batch) == 0 {
			continue
		}
		ph, args := inPlaceholders(batch)
		res, err := d.queryTxn(ctx, txnID, `SELECT id, coalesce(status, '') FROM chunks WHERE id IN (`+ph+`)`, args...)
		if err != nil {
			return "", err
		}
		for _, r := range res.Rows {
			id, status := asStr(r[0]), asStr(r[1])
			was, seen := check.statuses[id]
			if !seen {
				return "chunk " + id + " arrived under it", nil
			}
			if was != status {
				return "chunk " + id + " changed status", nil
			}
		}
	}
	gained, err := d.ontologyEntryGainedStatements(ctx, txnID, check.quiet)
	if err != nil || !gained {
		return "", err
	}
	return "a statement was written to it", nil
}

// deletedChunks is every chunk a delete of top's subtree removes, breadth first; with
// top "" the whole document.
func deletedChunks(rows planRows, top string) []string {
	if top != "" {
		return rows.subtree(top)
	}
	var out []string
	for _, root := range rows.kids[""] {
		out = append(out, rows.subtree(root)...)
	}
	return out
}

// topEntries are the in-force entries of top's subtree that no other entry of it is
// above: planning a delete of each covers its own subtree, so nothing is counted twice.
func topEntries(rows planRows, entries map[string]planEntry, top string) []string {
	var out []string
	seen := map[string]bool{}
	frontier := rows.kids[""]
	if top != "" {
		frontier = []string{top}
	}
	for len(frontier) > 0 {
		var next []string
		for _, id := range frontier {
			if seen[id] {
				continue
			}
			seen[id] = true
			if _, ok := entries[id]; ok {
				out = append(out, id)
				continue
			}
			next = append(next, rows.kids[id]...)
		}
		frontier = next
	}
	return out
}

// entryInUseRefusal names the entries in use and what rests on each, and says to
// retire instead.
func entryInUseRefusal(op string, wholeDocument bool, inUse []connector.OntologyPlannedChange) tools.Result {
	var named []string
	for i, ch := range inUse {
		if i == ontologyDeleteNamed {
			named = append(named, fmt.Sprintf("and %d more", len(inUse)-ontologyDeleteNamed))
			break
		}
		named = append(named, ch.Entry.Kind+" "+strconv.Quote(ch.Entry.Name)+" ("+dependentCounts(ch)+")")
	}
	what, retire := "the ontology entry it would remove is in use", "Retire it instead."
	if len(inUse) > 1 {
		what, retire = strconv.Itoa(len(inUse))+" ontology entries it would remove are in use", "Retire them instead."
	}
	next := "update_chunk with status \"retired\" on an entry keeps everything that rests on it, readable, and offers " +
		"the entry for no new write. An entry nothing rests on may be deleted."
	if wholeDocument {
		what = "the tenant ontology has entries in use"
		next = "The ontology is edited in place, never deleted while anything rests on it: retire the entries in use " +
			"(update_chunk with status \"retired\") and delete only those nothing rests on."
	}
	return errBusiness(op+": "+entryHasDependentsCode+": "+what+", so nothing was deleted: "+strings.Join(named, "; ")+
		". "+retire, next)
}

// dependentCounts is "statements 3, typed_documents 12" for the counts a delete would
// destroy or orphan, by name.
func dependentCounts(ch connector.OntologyPlannedChange) string {
	var parts []string
	for name, c := range ch.Counts {
		if c.N > 0 && (c.Effect == connector.OntologyEffectDestroyed || c.Effect == connector.OntologyEffectOrphaned) {
			parts = append(parts, name+" "+strconv.Itoa(c.N))
		}
	}
	sort.Strings(parts)
	return strings.Join(parts, ", ")
}

// ontologyEntryGainedStatements re-asserts, inside delete_chunk's transaction, the
// part of the plan's answer a concurrent write is most likely to change: the subtree
// being removed has no statement at either end. The plan found none (an entry with a
// statement is refused before the transaction), so any found now arrived meanwhile.
func (d *Document) ontologyEntryGainedStatements(ctx context.Context, txnID string, ids []string) (bool, error) {
	for _, batch := range batchIDs(ids, graphIDBatch) {
		if len(batch) == 0 {
			continue
		}
		ph, args := inPlaceholders(batch)
		res, err := d.queryTxn(ctx, txnID, `SELECT 1 FROM chunk_edges WHERE from_id IN (`+ph+`) OR to_id IN (`+ph+`) LIMIT 1`,
			append(append([]any{}, args...), args...)...)
		if err != nil {
			return false, err
		}
		if len(res.Rows) > 0 {
			return true, nil
		}
	}
	return false, nil
}

// errOntologyChangedTxn rolls a delete's transaction back when ontologyDeleteRecheck
// finds a change; the caller answers with errOntologyChangedDuringDelete, never this.
var errOntologyChangedTxn = errors.New("the ontology changed while it was being deleted from")

// errOntologyChangedDuringDelete is a delete's refusal when the re-check finds that
// what the guard approved changed before the transaction.
func errOntologyChangedDuringDelete(op, why string) tools.Result {
	return errBusiness(op+": "+entryHasDependentsCode+": the ontology changed while this delete was checked ("+why+
		"), so nothing was deleted", "Send the delete again; it is checked afresh. To keep what uses an entry, retire it instead (status \"retired\").")
}

// ontologyReimportRefusal refuses import_md reimport onto the tenant ontology, for
// every caller. An edition gives each changed part a new chunk and closes the old one;
// for the ontology that replaces entries under new ids and leaves everything keyed by
// the old ids behind — the loss D3 forbids, by another route. The ontology changes in
// place. Fails closed on a lookup that errors.
func (d *Document) ontologyReimportRefusal(ctx context.Context, key sqlmem.ScopeKey, in docInput) *tools.Result {
	if key.Scope != "tenant" {
		return nil
	}
	fail := func(err error) *tools.Result {
		r := errFrom("import_md: could not tell whether the reimport targets the tenant ontology, so nothing was imported: "+err.Error(), err)
		return &r
	}
	ontID, err := d.resolveOntologyID(ctx, key)
	if err != nil {
		return fail(err)
	}
	if ontID == "" {
		return nil
	}
	target := strings.TrimSpace(in.DocumentID)
	if wk := strings.TrimSpace(in.WorkKey); target == "" && wk != "" {
		if target, _, _, err = d.documentByWorkKey(ctx, key, wk); err != nil {
			return fail(err)
		}
	}
	if target != ontID {
		return nil
	}
	r := errBusiness("import_md: a reimport onto the tenant ontology is refused: an edition gives each changed entry a new "+
		"chunk and closes the old one, leaving what rests on the old ids behind. Nothing was imported.",
		"Change an entry in place (update_chunk), retire it (status \"retired\"), or add entries with import_md "+
			"under the ontology without reimport.")
	return &r
}
