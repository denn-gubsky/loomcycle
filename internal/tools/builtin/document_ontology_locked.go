package builtin

// document_ontology_locked.go — @locked on an entry of the tenant ontology (RFC GQ D5,
// plan phase P2.3; the format is RFC FO 3.1a and 3.7 B).
//
// A LOCKED ENTRY cannot be renamed, moved, retired or deleted, with or without
// dependents, and neither can any entry above it: each of those changes the entry
// itself or its place (a concept's tag carries its ancestors' names; a retire or a
// delete takes the subtree with it). The text is not frozen: the meaning and the other
// directives of a locked entry stay editable (RFC FO 3.7 B6).
//
// FOR EVERY CALLER, the operator plane included, like the delete refusal beside it
// (document_ontology_delete.go): the checks sit inside the handlers, not in
// guardOntologyWrite, which the operator plane skips. The one way past a lock is the
// one RFC FO names: an operator's own update_chunk of the entry's body, removing the
// line. An agent never reaches that (guardOntologyWrite refuses its update_chunk), and
// no proposal can carry the line: propose_entity quotes every directive of the body it
// files, a change proposal refuses @locked in its directives, and an agent's raw
// proposal or an accept of one carrying it is refused here.
//
// READ FROM THE PARSED ENTRY (planEntry.locked), so the lock is exactly what the reader
// says the entry declares. A section header and the root are not entries: @locked on
// one is reported as an issue and locks nothing.

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"

	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// entryLockedCode is the refusal's code, at the start of its text as the other
// ontology refusals carry theirs (entry_has_dependents, entry_retired).
const entryLockedCode = "entry_locked"

// lockedIn lists the locked entries among ids, in the order given. ids may hold
// proposals, grammars and section headers; those are not entries and lock nothing.
func lockedIn(ids []string, entries map[string]planEntry) []planEntry {
	var out []planEntry
	for _, id := range ids {
		if e, ok := entries[id]; ok && e.locked {
			out = append(out, e)
		}
	}
	return out
}

// lockedWho names the locked entries of a refusal: the target itself, or the entries
// under it that hold it.
func lockedWho(target string, locked []planEntry) string {
	var named []string
	for i, e := range locked {
		if i == ontologyDeleteNamed {
			named = append(named, "and "+strconv.Itoa(len(locked)-ontologyDeleteNamed)+" more")
			break
		}
		named = append(named, e.Kind+" "+strconv.Quote(e.Name))
	}
	if len(locked) == 1 && locked[0].ChunkID == target {
		return named[0] + " carries @locked"
	}
	return "it is above a locked entry (" + strings.Join(named, ", ") + ")"
}

// lockedRefusal refuses op on target because of the locked entries of its subtree.
// verb is what was refused: renamed, moved, retired or deleted.
func lockedRefusal(op, target, verb string, locked []planEntry) tools.Result {
	return errBusiness(op+": "+entryLockedCode+": "+lockedWho(target, locked)+": a locked entry, and every entry "+
		"above it, cannot be renamed, moved, retired or deleted. Nothing was "+verb+".",
		"An operator removes the @locked line from the locked entry's body (update_chunk) first. The lock does not "+
			"stop editing the entry's meaning or its other directives.")
}

// lockedPlanNote is the plan's report of a change a lock would refuse (RFC GQ P1).
func lockedPlanNote(op string, e planEntry, locked []planEntry) string {
	return entryLockedCode + ": " + lockedWho(e.ChunkID, locked) + ", so this " + op + " would be refused until " +
		"an operator removes the lock"
}

// lockedHeaderIssues reports @locked on the ontology's root or on a section header:
// neither is an entry, so the line has no effect (RFC FO 3.1a). Asked of the sections
// that count; an out-of-force section reports its own issue.
func lockedHeaderIssues(kids map[string][]ontologyChunk, rootID string, sectionOf map[string]string, bodyOf func(string) string) []string {
	var out []string
	if rootID != "" && memrank.DeclaresLocked(bodyOf(rootID)) {
		out = append(out, "@locked on the ontology's root has no effect; lock an entry instead")
	}
	for _, c := range kids[rootID] {
		if kind := sectionOf[c.id]; kind != "" && kind != sectionOutOfForce && memrank.DeclaresLocked(bodyOf(c.id)) {
			out = append(out, "@locked on the "+kind+" section header has no effect; lock an entry of it instead")
		}
	}
	return out
}

// ontologyLockedUpdateRefusal refuses an update_chunk of the tenant ontology that would
// change a locked entry, or an entry above one, without naming the change:
//
//   - a rename: the name the reader reads changes (the title, or the body's first
//     heading when the title is empty);
//   - a re-read: on a child of the root, a new body that changes how the section
//     pre-pass reads the chunk (@section added, dropped, switched or made unknown)
//     takes its entries out of force, or reads them as another section's.
//
// Called before the revision moves; nil outside the ontology and for an update that
// changes neither. Fails closed, on a partial read too.
func (d *Document) ontologyLockedUpdateRefusal(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, row chunkRow, in docInput, present map[string]json.RawMessage) *tools.Result {
	_, hasTitle := present["title"]
	_, hasBody := present["body"]
	if key.Scope != "tenant" || (!hasTitle && !hasBody) {
		return nil
	}
	title := row.Title
	if hasTitle {
		title = in.Title
	}
	name := memrank.TrimOntologyName(title)
	sameName := name != "" && name == memrank.TrimOntologyName(row.Title)
	fail := func(err error) *tools.Result {
		r := errFrom("update_chunk: could not tell whether the update changes a locked ontology entry, so nothing was changed: "+err.Error(), err)
		return &r
	}
	rctx := noWrites(ctx)
	if sameName {
		// The title holds the name unchanged: only a body edit of a document's
		// top-level chunk can still matter. One cheap read keeps a plain body edit
		// anywhere else free of the ontology lookups.
		if !hasBody {
			return nil
		}
		top, err := d.isTopLevelChunk(rctx, key, row)
		if err != nil {
			return fail(err)
		}
		if !top {
			return nil
		}
	}
	ontID, err := d.resolveOntologyID(rctx, key)
	if err != nil {
		return fail(err)
	}
	if ontID == "" || ontID != row.DocumentID {
		return nil
	}
	read, err := d.ontologyForKey(rctx, key, mscope, memrank.OntologyPath, nil)
	if err != nil {
		return fail(err)
	}
	if read.partial {
		return lockReadPartial("update_chunk")
	}
	entries := planEntries(read)
	cur, err := d.readBody(rctx, mscope, key, row.ID)
	if err != nil {
		return fail(err)
	}
	verb := ""
	if hasBody && row.ParentID != "" && row.ParentID == read.RootChunkID && sectionReading(cur.Body) != sectionReading(in.Body) {
		verb = "changed: the new body changes how the section reader reads this chunk"
	}
	if e, ok := entries[row.ID]; ok && verb == "" && !sameName {
		if name == "" {
			body := cur.Body
			if hasBody {
				body = in.Body
			}
			name = memrank.FirstHeadingName(body)
		}
		if name != e.Name {
			verb = "renamed"
		}
	}
	if verb == "" {
		return nil
	}
	rows, err := d.planChunkRows(rctx, key, ontID)
	if err != nil {
		return fail(err)
	}
	if locked := lockedIn(rows.subtree(row.ID), entries); len(locked) > 0 {
		r := lockedRefusal("update_chunk", row.ID, verb, locked)
		return &r
	}
	return nil
}

// sectionReading is how the section pre-pass reads a root child's body: the kind it
// knows, and the issue an unknown @section raises.
func sectionReading(body string) string {
	return memrank.SectionKind(body) + "|" + memrank.SectionIssue(body)
}

// isTopLevelChunk reports whether row sits directly under its document's root chunk.
func (d *Document) isTopLevelChunk(ctx context.Context, key sqlmem.ScopeKey, row chunkRow) (bool, error) {
	if row.ParentID == "" {
		return false, nil
	}
	res, err := d.sqlRead(ctx, key, `SELECT 1 FROM documents WHERE id = ? AND root_chunk_id = ? LIMIT 1`, []any{row.DocumentID, row.ParentID})
	if err != nil {
		return false, err
	}
	return len(res.Rows) > 0, nil
}

// lockReadPartial is a lock guard's refusal when a chunk body of the ontology could
// not be read: the reader then reads that entry's directives as empty, so a locked
// entry would pass as unlocked. The guard fails closed.
func lockReadPartial(op string) *tools.Result {
	r := errTransient(op+": "+entryLockedCode+": a chunk body of the tenant ontology could not be read, so whether "+
		"a locked entry is affected is unknown; nothing was changed", "Try again.")
	return &r
}

// ontologyUpsertRefusal refuses upsert_chunk over an existing chunk of the tenant
// ontology that is not a proposal, for every caller. An upsert rewrites the chunk in
// place past every check update_chunk makes (rename, retire, demotion, locks); the
// ontology's entries change through update_chunk. A proposal may still be re-filed by
// its key, as the agent's guard allows. Fails closed.
func (d *Document) ontologyUpsertRefusal(ctx context.Context, key sqlmem.ScopeKey, existing string) *tools.Result {
	if key.Scope != "tenant" {
		return nil
	}
	fail := func(err error) *tools.Result {
		r := errFrom("upsert_chunk: could not tell whether the key names a chunk of the tenant ontology, so nothing was written: "+err.Error(), err)
		return &r
	}
	ontID, err := d.resolveOntologyID(ctx, key)
	if err != nil {
		return fail(err)
	}
	if ontID == "" {
		return nil
	}
	row, found, err := d.getChunkRow(ctx, key, existing)
	if err != nil {
		return fail(err)
	}
	if !found || row.DocumentID != ontID || memrank.IsInertEntityStatus(row.Status) {
		return nil
	}
	r := errBusiness("upsert_chunk: the natural_key names chunk "+row.ID+" of the tenant ontology, which is not a proposal; "+
		"an upsert would rewrite it past the checks every change to the ontology meets. Nothing was written.",
		"Change the entry with update_chunk at its revision, or retire it with status \"retired\".")
	return &r
}

// lockedProposalRefusal refuses an ontology proposal whose body declares @locked: an
// agent's raw create_chunk or upsert_chunk files the body as written, and accepting
// the proposal would put the lock in force without an operator having written it.
func lockedProposalRefusal(op, body string) *tools.Result {
	if !memrank.DeclaresLocked(body) {
		return nil
	}
	r := errPermission(op+": "+entryLockedCode+": a proposal cannot carry @locked; only an operator's own edit of "+
		"an entry adds or removes the lock (RFC FO 3.1a)", "Leave the @locked line out of the proposal.")
	return &r
}
