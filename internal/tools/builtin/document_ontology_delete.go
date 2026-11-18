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
// one connection, so it cannot run inside it). delete_chunk closes the cheap half of
// the window: inside its transaction it re-asserts that the subtree it is about to
// remove still has no statement (ontologyEntryGainedStatements), the dependent a
// concurrent write is most likely to add.

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

// ontologyDeleteGuard is asked by every path that deletes chunks of a document in the
// tenant scope. top is the chunk whose subtree goes ("" = the whole document). It
// returns a refusal when the subtree holds an in-force entry of the tenant ontology
// that has dependents. checked is the chunks of the entries' subtrees when the subtree
// held entries and none was in use — exactly what the plan found no statement on: the
// caller re-asserts, inside its transaction, that none gained one meanwhile.
func (d *Document) ontologyDeleteGuard(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, op, docID, top string) (refused *tools.Result, checked []string) {
	if key.Scope != "tenant" {
		return nil, nil
	}
	fail := func(err error) (*tools.Result, []string) {
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
	tops := topEntries(rows, planEntries(read), top)
	if len(tops) == 0 {
		return nil, nil // a proposal, a grammar, an empty section: no entry goes
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
			checked = append(checked, rows.subtree(t)...)
		}
		return nil, checked
	}
	r := entryInUseRefusal(op, top == "", inUse)
	return &r, nil
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

// errEntryGainedStatementsTxn rolls delete_chunk's transaction back when the re-check
// finds a statement; the caller answers with errEntryGainedStatements, never this.
var errEntryGainedStatementsTxn = errors.New("an ontology entry gained a statement while it was being deleted")

// errEntryGainedStatements is delete_chunk's refusal when the re-check above finds a
// statement the plan did not.
func errEntryGainedStatements() tools.Result {
	return errBusiness("delete_chunk: "+entryHasDependentsCode+": a statement was written to the ontology entry while it "+
		"was being deleted, so nothing was deleted", "Retire it instead: update_chunk with status \"retired\".")
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
