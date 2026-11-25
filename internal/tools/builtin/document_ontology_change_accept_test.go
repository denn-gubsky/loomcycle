package builtin

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
)

// newAcceptFixture is the change fixture with dependents: a document typed Memo, a
// document placed under topics/reporting and a supervises statement.
func newAcceptFixture(t *testing.T) changeFixture {
	t.Helper()
	f := newChangeFixture(t, retiredOntologyMD)
	seedFingerprintDependents(f.retiredFixture)
	return f
}

// file files a change proposal as the agent and returns its chunk id.
func (f changeFixture) file(v map[string]any) string {
	f.t.Helper()
	return f.mustPropose(v)["chunk_id"].(string)
}

// accept accepts a proposal on the operator plane.
func (f changeFixture) accept(id, fingerprint string) (ChangeAcceptReport, error) {
	f.t.Helper()
	handled, rep, err := f.d.AcceptChangeProposal(f.ctx, "tenant", id, fingerprint)
	if !handled && err == nil {
		f.t.Fatalf("accept %s: not handled as a change proposal", id)
	}
	return rep, err
}

// fingerprintNow is the fingerprint a plan reports now.
func (f changeFixture) fingerprintNow() string {
	f.t.Helper()
	return ontologyFingerprintNow(f.t, f.d, f.ctx)
}

// exists reports whether chunk id is still stored.
func (f changeFixture) exists(id string) bool {
	f.t.Helper()
	_, ok, err := f.d.getChunkRow(f.ctx, mustTenantKey(f.t, f.d, f.ctx), id)
	if err != nil {
		f.t.Fatal(err)
	}
	return ok
}

func (f changeFixture) body(id string) string {
	f.t.Helper()
	b, _ := f.must(map[string]any{"op": "get_chunk", "scope": "tenant", "id": id})["body"].(string)
	return b
}

// setMark rewrites a proposal's change_proposal mark, as the operator's editor can.
func (f changeFixture) setMark(id string, mark any) {
	f.t.Helper()
	if r := f.change(f.ctx, "update_chunk", id, map[string]any{"fields": map[string]any{"change_proposal": mark}}); r.IsError {
		f.t.Fatalf("set mark: %s", r.Text)
	}
}

func (f changeFixture) markOf(id string) map[string]any {
	f.t.Helper()
	fields, _ := f.must(map[string]any{"op": "get_chunk", "scope": "tenant", "id": id})["fields"].(map[string]any)
	m, _ := fields["change_proposal"].(map[string]any)
	out := map[string]any{}
	for k, v := range m {
		out[k] = v
	}
	return out
}

// TestAcceptChange_KeepsTheTargetsChunkID (RFC GQ check P4): accepting an edit that
// renames a relation kind, gives it a meaning and sets its directives changes the
// entry in place — same chunk id, the new name, the new directive in force — with the
// plan's fingerprint, and reports what it applied.
func TestAcceptChange_KeepsTheTargetsChunkID(t *testing.T) {
	f := newAcceptFixture(t)
	supervises := f.chunk("supervises")
	id := f.file(map[string]any{"change": "edit", "entry": supervises, "name": "oversees",
		"meaning": "One person oversees another.", "directives": map[string]any{"dated": true}})
	rep, err := f.accept(id, f.fingerprintNow())
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	if got := f.chunk("oversees"); got != supervises {
		t.Errorf("the renamed entry is chunk %s, want the target's own %s", got, supervises)
	}
	var found bool
	for _, r := range f.read().Relations {
		if r.Name == "oversees" {
			found = true
			if r.ChunkID != supervises || !r.Dated || !strings.Contains(r.Meaning, "oversees another") {
				t.Errorf("oversees reads as %+v", r)
			}
		}
		if r.Name == "supervises" && r.ChunkID != "" {
			t.Errorf("the old name is still a tenant kind: %+v", r)
		}
	}
	if !found {
		t.Error("the renamed kind is not in the read")
	}
	if !reflect.DeepEqual(rep.Applied, []string{"rename", "edit"}) || rep.Target != supervises || rep.Op != memrank.ChangeOpEdit {
		t.Errorf("report = %+v", rep)
	}
	if rep.Risk == "none" || rep.Counts["statements_of_kind"] == 0 {
		t.Errorf("report carries no plan of the renamed kind's statements: %+v", rep)
	}
}

// TestAcceptChange_TamperedMarkIsRefused422: a mark edited after filing into something
// filing would refuse, a mark that no longer decodes, and a proposal filed elsewhere
// than filing puts one are refused as invalid; nothing changes.
func TestAcceptChange_TamperedMarkIsRefused422(t *testing.T) {
	f := newAcceptFixture(t)
	provision := f.chunk("Provision")
	for name, tamper := range map[string]func(m map[string]any) any{
		"locked directive": func(m map[string]any) any {
			m["sets_directives"], m["directives"] = true, []map[string]any{{"name": "locked"}}
			return m
		},
		"unknown directive": func(m map[string]any) any {
			m["sets_directives"], m["directives"] = true, []map[string]any{{"name": "section", "value": "relations"}}
			return m
		},
		"kind": func(m map[string]any) any { m["kind"] = "relation"; return m },
		"section header target": func(m map[string]any) any {
			m["target"] = f.read().SectionChunks[memrank.SectionClasses]
			return m
		},
		"directives without the flag": func(m map[string]any) any {
			m["directives"] = []map[string]any{{"name": "norm"}}
			return m
		},
		"garbage": func(map[string]any) any { return "garbage" },
	} {
		id := f.file(map[string]any{"change": "edit", "entry": provision, "meaning": "A clause of an act."})
		f.setMark(id, tamper(f.markOf(id)))
		before := f.body(provision)
		if _, err := f.accept(id, f.fingerprintNow()); !errors.Is(err, ErrChangeProposalInvalid) {
			t.Errorf("%s: accept = %v, want ErrChangeProposalInvalid", name, err)
		}
		if f.body(provision) != before || !f.exists(id) {
			t.Errorf("%s: a refused accept changed the target or removed the proposal", name)
		}
		f.change(f.ctx, "delete_chunk", id, nil)
	}
	// A forged change proposal under its target: the reader would take it as a proposed
	// child of the entry, and filing never puts one there.
	mark := map[string]any{"op": "edit", "target": provision, "target_name": "Provision", "section": "classes",
		"kind": "class", "meaning": "Forged."}
	forged := f.must(map[string]any{"op": "create_chunk", "scope": "tenant", "document_id": f.read().DocumentID,
		"parent_id": provision, "title": "edit: Provision", "status": "proposed",
		"fields": map[string]any{"change_proposal": mark}})["id"].(string)
	if _, err := f.accept(forged, f.fingerprintNow()); !errors.Is(err, ErrChangeProposalInvalid) {
		t.Errorf("forged under the target: accept = %v, want ErrChangeProposalInvalid", err)
	}
	if strings.Contains(f.body(provision), "Forged") {
		t.Error("the forged proposal was applied")
	}
}

// TestAcceptChange_StaleFingerprintIsRefused (P2.4): a change something rests on is
// refused without a fingerprint and with one from before the ontology changed, each
// refusal carrying the current one; the target and the proposal stay.
func TestAcceptChange_StaleFingerprintIsRefused(t *testing.T) {
	f := newAcceptFixture(t)
	supervises := f.chunk("supervises")
	id := f.file(map[string]any{"change": "edit", "entry": supervises, "name": "oversees"})
	stale := f.fingerprintNow()
	f.file(map[string]any{"change": "retire", "entry": "Provision"}) // moves the fingerprint
	now := f.fingerprintNow()
	for given, code := range map[string]string{"": planRequiredCode, stale: planStaleCode} {
		_, err := f.accept(id, given)
		var pr *ChangePlanRefusal
		if !errors.As(err, &pr) || pr.Code != code || pr.Fingerprint != now {
			t.Errorf("accept with %q = %v, want %s carrying %s", given, err, code, now)
		}
	}
	if f.chunk("supervises") != supervises || !f.exists(id) {
		t.Error("a refused accept changed something")
	}
	if _, err := f.accept(id, now); err != nil {
		t.Errorf("accept with the current fingerprint: %v", err)
	}
}

// TestAcceptRetire_EntryBecomesRetiredAndDependentsStay: accepting a retire retires the
// entry and its subtree in place; the document typed with it keeps its type.
func TestAcceptRetire_EntryBecomesRetiredAndDependentsStay(t *testing.T) {
	f := newAcceptFixture(t)
	doc := f.must(map[string]any{"op": "create_document", "scope": "tenant", "title": "A rule", "type": "Policy"})
	policy, internal := f.chunk("Policy"), f.chunk("Internal Policy")
	id := f.file(map[string]any{"change": "retire", "entry": policy})
	rep, err := f.accept(id, f.fingerprintNow())
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	for _, c := range []string{policy, internal} {
		if st := f.status(c); st != memrank.OntologyStatusRetired {
			t.Errorf("chunk %s status %q, want retired", c, st)
		}
	}
	g := f.must(map[string]any{"op": "get_chunk", "scope": "tenant", "id": doc["root_chunk_id"]})
	if g["type"] != "Policy" {
		t.Errorf("the typed document lost its type: %v", g["type"])
	}
	if !reflect.DeepEqual(rep.Applied, []string{"retire"}) || rep.Counts["typed_documents"] == 0 {
		t.Errorf("report = %+v", rep)
	}
}

// TestAcceptChange_LockedTargetIsRefused: an entry locked after the proposal was filed
// refuses the accept; nothing changes and the proposal stays.
func TestAcceptChange_LockedTargetIsRefused(t *testing.T) {
	f := newAcceptFixture(t)
	provision := f.chunk("Provision")
	edit := f.file(map[string]any{"change": "edit", "entry": provision, "meaning": "A clause."})
	retire := f.file(map[string]any{"change": "retire", "entry": f.chunk("Document")})
	if r := f.change(f.ctx, "update_chunk", provision, map[string]any{"body": "@locked"}); r.IsError {
		t.Fatal(r.Text)
	}
	if r := f.change(f.ctx, "update_chunk", f.chunk("Internal Policy"), map[string]any{"body": "@locked"}); r.IsError {
		t.Fatal(r.Text)
	}
	for name, id := range map[string]string{"edit of a locked entry": edit, "retire above a locked entry": retire} {
		if _, err := f.accept(id, f.fingerprintNow()); !errors.Is(err, ErrChangeEntryLocked) {
			t.Errorf("%s: accept = %v, want ErrChangeEntryLocked", name, err)
		}
		if !f.exists(id) {
			t.Errorf("%s: the proposal was removed", name)
		}
	}
	if f.body(provision) != "@locked" || f.status(f.chunk("Document")) != "" {
		t.Error("a refused accept changed an entry")
	}
}

// TestAcceptChange_ProposalChunkIsRemoved (RFC GQ Q3): the accepted proposal's chunk is
// deleted, not left with a status; no proposal names the target any more.
func TestAcceptChange_ProposalChunkIsRemoved(t *testing.T) {
	f := newAcceptFixture(t)
	id := f.file(map[string]any{"change": "edit", "entry": "Provision", "meaning": "A numbered clause."})
	if _, err := f.accept(id, ""); err != nil {
		t.Fatalf("accept: %v", err)
	}
	if f.exists(id) {
		t.Error("the accepted proposal's chunk is still stored")
	}
	for _, p := range f.read().Proposals {
		if p.ChunkID == id {
			t.Errorf("the accepted proposal is still listed: %+v", p)
		}
	}
	if !strings.Contains(f.body(f.chunk("Provision")), "A numbered clause.") {
		t.Errorf("the meaning was not applied: %q", f.body(f.chunk("Provision")))
	}
}

// TestAcceptChange_ConcurrentEditOfTargetIsRefused: an edit of the target between the
// accept's checks and its write is a revision conflict; the concurrent edit stands,
// nothing of the proposal is applied, and it stays to be accepted again. A move-only
// edit is held to the same revision.
func TestAcceptChange_ConcurrentEditOfTargetIsRefused(t *testing.T) {
	f := newAcceptFixture(t)
	provision := f.chunk("Provision")
	for name, v := range map[string]map[string]any{
		"meaning":   {"change": "edit", "entry": provision, "meaning": "From the proposal."},
		"move only": {"change": "edit", "entry": provision, "parent": f.chunk("Document")},
	} {
		id := f.file(v)
		f.d.beforeAcceptWrite = func() {
			f.d.beforeAcceptWrite = nil
			if r := f.change(f.ctx, "update_chunk", provision, map[string]any{"body": "Edited meanwhile (" + name + ")."}); r.IsError {
				t.Fatal(r.Text)
			}
		}
		if _, err := f.accept(id, f.fingerprintNow()); !errors.Is(err, ErrChangeTargetChanged) {
			t.Errorf("%s: accept = %v, want ErrChangeTargetChanged", name, err)
		}
		if got := f.body(provision); got != "Edited meanwhile ("+name+")." {
			t.Errorf("%s: target body %q", name, got)
		}
		if !f.exists(id) {
			t.Errorf("%s: the proposal was removed", name)
		}
		if p, _, _ := f.d.getChunkRow(f.ctx, mustTenantKey(t, f.d, f.ctx), provision); p.ParentID == f.chunk("Document") {
			t.Errorf("%s: the target moved", name)
		}
		f.change(f.ctx, "delete_chunk", id, nil)
	}
}

// TestAcceptChange_ClassRenameTheJobCannotCarryIsRefused (RFC GQ Q4, P5.3): a class
// something rests on in a user scope, which the rename job does not reach yet, is not
// renamed, fingerprint or not; a class nothing rests on is.
func TestAcceptChange_ClassRenameTheJobCannotCarryIsRefused(t *testing.T) {
	f := newAcceptFixture(t)
	f.must(map[string]any{"op": "create_document", "scope": "user", "title": "My memo", "type": "Memo"})
	memo := f.chunk("Memo")
	id := f.file(map[string]any{"change": "edit", "entry": memo, "name": "Memorandum"})
	if _, err := f.accept(id, f.fingerprintNow()); !errors.Is(err, ErrChangeRenameJobPending) {
		t.Errorf("accept = %v, want ErrChangeRenameJobPending", err)
	}
	if f.chunk("Memo") != memo || !f.exists(id) {
		t.Error("a refused rename changed something")
	}
	free := f.file(map[string]any{"change": "edit", "entry": "Provision", "name": "Clause"})
	if _, err := f.accept(free, f.fingerprintNow()); err != nil {
		t.Errorf("rename of a class nothing rests on: %v", err)
	}
}

// TestAcceptChange_ClassRenameIsCarriedByTheRenameJob (RFC GQ P5.3): accepting the
// rename of a class whose rows are all in the tenant scope renames it, and the rename job
// moves its stored type with it.
func TestAcceptChange_ClassRenameIsCarriedByTheRenameJob(t *testing.T) {
	f := newAcceptFixture(t)
	memo := f.chunk("Memo")
	id := f.file(map[string]any{"change": "edit", "entry": memo, "name": "Memorandum"})
	rep, err := f.accept(id, f.fingerprintNow())
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	if !containsString(rep.Applied, "rename") {
		t.Errorf("applied = %v, want rename", rep.Applied)
	}
	count := func(typ string) string {
		res, err := f.d.query(f.ctx, mustTenantKey(t, f.d, f.ctx), `SELECT count(*) FROM documents WHERE type = ?`, typ)
		if err != nil {
			t.Fatal(err)
		}
		return asStr(res.Rows[0][0])
	}
	if old, now := count("Memo"), count("Memorandum"); old != "0" || now != "1" {
		t.Errorf("documents typed Memo %s, Memorandum %s; want 0 and 1", old, now)
	}
}

// TestAcceptChange_MeaningCannotAddOrRemoveFields:a class's field bullets are its own
// whatever the proposal's meaning says — a field-shaped line in the meaning is quoted
// text, and a meaning without the entry's bullets does not drop them.
func TestAcceptChange_MeaningCannotAddOrRemoveFields(t *testing.T) {
	f := newAcceptFixture(t)
	id := f.file(map[string]any{"change": "edit", "entry": "Document", "section": "classes",
		"meaning": "A written record.\n- `secret` text", "directives": map[string]any{"memory_scope": "tenant"}})
	if _, err := f.accept(id, f.fingerprintNow()); err != nil {
		t.Fatalf("accept: %v", err)
	}
	for _, term := range f.read().Terms {
		if term.Name == "Document" && !reflect.DeepEqual(term.Fields, []string{"title"}) {
			t.Errorf("Document fields = %v, want [title]", term.Fields)
		}
	}
	body := f.body(f.chunk("Document"))
	for _, want := range []string{"@memory_scope tenant", "- `title` text", "> - `secret` text", "A written record."} {
		if !strings.Contains(body, want) {
			t.Errorf("body %q lacks %q", body, want)
		}
	}
}

// TestAcceptChange_SecondProposalForTheSameChangeIsRejected: a second open proposal of
// the same target and op (filed on another replica, past filing's per-replica lock) is
// rejected, saying why, when one is accepted; a proposal of another op stays open.
func TestAcceptChange_SecondProposalForTheSameChangeIsRejected(t *testing.T) {
	f := newAcceptFixture(t)
	provision := f.chunk("Provision")
	first := f.file(map[string]any{"change": "edit", "entry": provision, "meaning": "First."})
	mark := f.markOf(first)
	mark["meaning"] = "Second."
	second := f.must(map[string]any{"op": "create_chunk", "scope": "tenant", "document_id": f.read().DocumentID,
		"parent_id": f.read().SectionChunks[memrank.SectionClasses], "title": "edit: Provision", "status": "proposed",
		"body": "Filed elsewhere.", "fields": map[string]any{"change_proposal": mark}})["id"].(string)
	retire := f.file(map[string]any{"change": "retire", "entry": provision})
	rep, err := f.accept(first, "")
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	if !reflect.DeepEqual(rep.Superseded, []string{second}) {
		t.Errorf("superseded = %v, want [%s]", rep.Superseded, second)
	}
	if st := f.status(second); st != memrank.OntologyStatusRejected || !strings.Contains(f.body(second), "Rejected: proposal "+first) {
		t.Errorf("second proposal: status %q body %q", st, f.body(second))
	}
	if f.status(retire) != memrank.OntologyStatusProposed {
		t.Error("a proposal of another op was rejected")
	}
	if _, err := f.accept(second, ""); err == nil {
		t.Error("the rejected second proposal was accepted")
	}
}

// TestAcceptChangeProposal_OtherProposalsAreNotHandled: an add proposal and a scheme
// proposal carry no change mark; the accept leaves them to their own paths.
func TestAcceptChangeProposal_OtherProposalsAreNotHandled(t *testing.T) {
	f := newAcceptFixture(t)
	add := f.mustPropose(map[string]any{"name": "Circular", "parent": "Document", "body": "Seen in 3 documents."})["chunk_id"].(string)
	handled, _, err := f.d.AcceptChangeProposal(f.ctx, "tenant", add, "")
	if handled || err != nil {
		t.Errorf("add proposal: handled %v err %v", handled, err)
	}
	if f.status(add) != memrank.OntologyStatusProposed {
		t.Error("the add proposal was touched")
	}
}

// TestAcceptChange_OnPostgres: an edit and a retire accepted in place, and a stale
// fingerprint refused, on the postgres tier.
func TestAcceptChange_OnPostgres(t *testing.T) {
	d, base := pgDocumentOrSkip(t)
	f := changeFixture{retiredFixture: &retiredFixture{t: t, d: d,
		ctx: operatorCtx(installTenantOntology(t, d, base, retiredOntologyMD, memrank.OntologyConfirmedStatus))}, agent: inRunCtx(base)}
	seedFingerprintDependents(f.retiredFixture)
	supervises, reporting := f.chunk("supervises"), f.chunk("Reporting")
	edit := f.file(map[string]any{"change": "edit", "entry": supervises, "name": "oversees"})
	retire := f.file(map[string]any{"change": "retire", "entry": reporting})
	var pr *ChangePlanRefusal
	if _, err := f.accept(edit, strings.Repeat("0", 64)); !errors.As(err, &pr) || pr.Code != planStaleCode {
		t.Errorf("stale accept on postgres = %v", err)
	}
	if _, err := f.accept(edit, f.fingerprintNow()); err != nil {
		t.Fatalf("accept edit on postgres: %v", err)
	}
	if _, err := f.accept(retire, f.fingerprintNow()); err != nil {
		t.Fatalf("accept retire on postgres: %v", err)
	}
	if f.chunk("oversees") != supervises || f.status(reporting) != memrank.OntologyStatusRetired || f.exists(edit) || f.exists(retire) {
		t.Error("the accepts were not applied in place on postgres")
	}
	// An edit with a move: the move follows only when the fingerprint after the update
	// is exactly what the update left, read from this tier's aggregates.
	memo, provision := f.chunk("Memo"), f.chunk("Provision")
	move := f.file(map[string]any{"change": "edit", "entry": memo, "meaning": "A short note.", "parent": provision})
	rep, err := f.accept(move, f.fingerprintNow())
	if err != nil {
		t.Fatalf("accept edit with a move on postgres: %v", err)
	}
	if row, _, _ := f.d.getChunkRow(f.ctx, mustTenantKey(t, f.d, f.ctx), memo); row.ParentID != provision || len(rep.Applied) != 2 {
		t.Errorf("edit with a move on postgres: parent %s applied %v", row.ParentID, rep.Applied)
	}
}
