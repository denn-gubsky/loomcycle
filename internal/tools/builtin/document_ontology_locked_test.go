package builtin

import (
	"context"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/connector"
	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// lockedMD is the retired-entry fixture's ontology with a lock on one entry of each
// section: the class Internal Policy (under Policy, under Document), the sub-kind
// co_supervises (under supervises), the concept Reporting, and the instance Globex.
func lockedMD() string {
	md := retiredOntologyMD
	for _, at := range []string{"##### Internal Policy\n", "#### co_supervises\n", "#### Reporting\n", "### Globex\n"} {
		md = strings.Replace(md, at, at+"@locked\n", 1)
	}
	return md
}

func newLockedFixture(t *testing.T) *retiredFixture {
	t.Helper()
	d, base, _ := documentFixture(t)
	return &retiredFixture{t: t, d: d, ctx: operatorCtx(installTenantOntology(t, d, base, lockedMD(), memrank.OntologyConfirmedStatus))}
}

// update is the caller's update_chunk of id at its current revision with the given
// fields; it returns the refusal text, "" when applied.
func (f *retiredFixture) update(ctx context.Context, id string, fields map[string]any) string {
	f.t.Helper()
	g := f.must(map[string]any{"op": "get_chunk", "scope": "tenant", "id": id})
	v := map[string]any{"op": "update_chunk", "scope": "tenant", "id": id, "revision": int(g["revision"].(float64))}
	for k, x := range fields {
		v[k] = x
	}
	if _, r := f.do(ctx, v); r.IsError {
		return refusalText(r)
	}
	return ""
}

func (f *retiredFixture) move(ctx context.Context, id, parent string) string {
	f.t.Helper()
	if _, r := f.do(ctx, map[string]any{"op": "move_chunk", "scope": "tenant", "id": id, "new_parent_id": parent}); r.IsError {
		return refusalText(r)
	}
	return ""
}

func (f *retiredFixture) body(id string) string {
	f.t.Helper()
	b, _ := f.must(map[string]any{"op": "get_chunk", "scope": "tenant", "id": id})["body"].(string)
	return b
}

func wantLocked(t *testing.T, what, text, locked string) {
	t.Helper()
	if !strings.Contains(text, entryLockedCode) || !strings.Contains(text, locked) {
		t.Errorf("%s = %q, want an %s refusal naming %s", what, text, entryLockedCode, locked)
	}
}

// TestOntology_LockedEntryCannotBeRetired (RFC GQ check P2): the operator's retire of a
// locked entry of each section is refused with entry_locked, and its status stays.
func TestOntology_LockedEntryCannotBeRetired(t *testing.T) {
	f := newLockedFixture(t)
	for _, c := range []struct{ title, named string }{
		{"Internal Policy", `class "Internal Policy"`}, {"co_supervises", `relation "co_supervises"`},
		{"Reporting", `concept "Reporting"`}, {"Globex", `instance "Globex"`},
	} {
		id := f.chunk(c.title)
		for name, ctx := range map[string]context.Context{"operator": f.ctx, "admin": adminCtx(f.ctx)} {
			wantLocked(t, name+" retire "+c.title, f.setStatus(ctx, id, memrank.OntologyStatusRetired), c.named+" carries @locked")
		}
		if st := f.status(id); st != "" {
			t.Errorf("%s status = %q after the refused retire", c.title, st)
		}
	}
}

// TestOntology_LockedEntryWithoutDependentsCannotBeDeleted: a lock refuses a delete even
// where nothing rests on the entry — the delete would otherwise go through.
func TestOntology_LockedEntryWithoutDependentsCannotBeDeleted(t *testing.T) {
	f := newLockedFixture(t)
	id := f.chunk("co_supervises")
	plan, err := f.d.PlanOntology(f.ctx, []connector.OntologyChange{{Op: connector.OntologyPlanDelete, Entry: id}})
	if err != nil || len(plan.Changes) != 1 || plan.Changes[0].Risk != connector.OntologyRiskNone {
		t.Fatalf("the fixture's co_supervises should have no dependents: %v %+v", err, plan.Changes)
	}
	for name, ctx := range map[string]context.Context{"operator": f.ctx, "admin": adminCtx(f.ctx)} {
		wantLocked(t, name+" delete", f.deleteChunk(ctx, id), `relation "co_supervises" carries @locked`)
	}
	if !f.exists(id) {
		t.Fatal("the locked entry was deleted")
	}
	// The same entry without its lock is deleted.
	if text := f.update(f.ctx, id, map[string]any{"body": "Shares supervision."}); text != "" {
		t.Fatalf("remove the lock: %s", text)
	}
	if text := f.deleteChunk(f.ctx, id); text != "" {
		t.Errorf("delete after the lock was removed: %s", text)
	}
}

// TestOntology_AncestorOfALockedEntryCannotBeMoved: a move of an entry above a locked
// one — or of the section header holding it — is refused, naming the locked entry.
func TestOntology_AncestorOfALockedEntryCannotBeMoved(t *testing.T) {
	f := newLockedFixture(t)
	wantLocked(t, "move Policy under Provision", f.move(f.ctx, f.chunk("Policy"), f.chunk("Provision")), `class "Internal Policy"`)
	wantLocked(t, "move supervises under filed_under", f.move(f.ctx, f.chunk("supervises"), f.chunk("filed_under")), `relation "co_supervises"`)
	wantLocked(t, "move the Relations header", f.move(f.ctx, f.chunk("Relations"), f.chunk("Instances")), `relation "co_supervises"`)
	if p := f.read(); termParent(t, p, "Policy") != "Document" {
		t.Errorf("Policy moved: parent %q", termParent(t, p, "Policy"))
	}
	// An entry with no lock under it still moves.
	if text := f.move(f.ctx, f.chunk("Memo"), f.chunk("Policy")); text != "" {
		t.Errorf("move of an unlocked entry: %s", text)
	}
}

func termParent(t *testing.T, read OntologyRead, name string) string {
	t.Helper()
	term, ok := termByName(read, name)
	if !ok {
		t.Fatalf("no class %s", name)
	}
	return term.Parent
}

// TestOntology_AncestorOfALockedEntryCannotBeRenamedRetiredOrDeleted: the rest of the
// ancestor rule; each refusal names the locked entry below.
func TestOntology_AncestorOfALockedEntryCannotBeRenamedRetiredOrDeleted(t *testing.T) {
	f := newLockedFixture(t)
	for _, title := range []string{"Document", "Policy"} {
		id := f.chunk(title)
		wantLocked(t, "rename "+title, f.update(f.ctx, id, map[string]any{"title": title + "s"}), `class "Internal Policy"`)
		wantLocked(t, "retire "+title, f.setStatus(f.ctx, id, memrank.OntologyStatusRetired), `class "Internal Policy"`)
		wantLocked(t, "delete "+title, f.deleteChunk(f.ctx, id), `class "Internal Policy"`)
	}
	wantLocked(t, "retire the scheme", f.setStatus(f.ctx, f.chunk("topics"), memrank.OntologyStatusRetired), `concept "Reporting"`)
	wantLocked(t, "delete the ontology", func() string {
		_, r := f.do(f.ctx, map[string]any{"op": "delete_document", "scope": "tenant", "id": f.read().DocumentID})
		return refusalText(r)
	}(), `class "Internal Policy"`)
}

// TestOntology_LockedEntryCannotBeRenamedOrMoved: the entry's own name and place are
// held — by its title, and by its body's heading when it has no title — while its text
// stays editable (RFC FO 3.7 B6).
func TestOntology_LockedEntryCannotBeRenamedOrMoved(t *testing.T) {
	f := newLockedFixture(t)
	id := f.chunk("Reporting")
	wantLocked(t, "rename", f.update(f.ctx, id, map[string]any{"title": "Disclosure"}), `concept "Reporting" carries @locked`)
	wantLocked(t, "rename by heading", f.update(f.ctx, id, map[string]any{"title": "", "body": "# Disclosure\n@locked"}), `concept "Reporting"`)
	wantLocked(t, "move", f.move(f.ctx, id, f.chunk("Risk Management")), `concept "Reporting" carries @locked`)
	if text := f.update(f.ctx, id, map[string]any{"body": "@locked\n@notation R\nWhat is reported to the regulator."}); text != "" {
		t.Errorf("an edit of the locked entry's text was refused: %s", text)
	}
	if text := f.update(f.ctx, id, map[string]any{"title": "Reporting"}); text != "" {
		t.Errorf("an update that keeps the name was refused: %s", text)
	}
	if c := conceptOfTag(f.read(), "topics/reporting"); !c.Locked || c.Notation != "R" {
		t.Errorf("after the edit the concept reads %+v, want it locked with notation R", c)
	}
}

// TestOntology_OperatorRemovesLockThenRetires: only the operator's own edit of the
// body takes the lock off, and the retire then goes through.
func TestOntology_OperatorRemovesLockThenRetires(t *testing.T) {
	f := newLockedFixture(t)
	id := f.chunk("Globex")
	wantLocked(t, "retire", f.setStatus(f.ctx, id, memrank.OntologyStatusRetired), `instance "Globex"`)
	if text := f.update(f.ctx, id, map[string]any{"body": "@a organization"}); text != "" {
		t.Fatalf("remove the lock: %s", text)
	}
	if text := f.setStatus(f.ctx, id, memrank.OntologyStatusRetired); text != "" {
		t.Errorf("retire after the lock was removed: %s", text)
	}
	if st := f.status(id); st != memrank.OntologyStatusRetired {
		t.Errorf("status = %q, want retired", st)
	}
}

// TestOntologyGuard_AgentCannotRemoveLock: an agent's update_chunk taking the line out
// is refused before the handler runs, and the entry stays locked.
func TestOntologyGuard_AgentCannotRemoveLock(t *testing.T) {
	f := newLockedFixture(t)
	id := f.chunk("Internal Policy")
	agent := tools.WithCallerTier(inRunCtx(f.ctx), tools.TierRun)
	if text := f.update(agent, id, map[string]any{"body": "No lock."}); !strings.Contains(text, "may not modify the ontology") {
		t.Fatalf("an agent's update_chunk removing @locked = %q, want the ontology guard's refusal", text)
	}
	if term, _ := termByName(f.read(), "Internal Policy"); !term.Locked || !strings.Contains(f.body(id), "@locked") {
		t.Errorf("after the agent's refused edit: locked=%v body=%q", term.Locked, f.body(id))
	}
}

// TestProposeChange_LockedDirectiveIsRefusedEitherWay: no proposal sets or clears the
// lock. propose_entity quotes the line, so it locks nothing on accept; an agent's raw
// proposal carrying it is refused; a change proposal may neither set it nor touch a
// locked entry (which is the only way to clear it).
func TestProposeChange_LockedDirectiveIsRefusedEitherWay(t *testing.T) {
	d, base, _ := documentFixture(t)
	f := changeFixture{retiredFixture: &retiredFixture{t: t, d: d,
		ctx: operatorCtx(installTenantOntology(t, d, base, lockedMD(), memrank.OntologyConfirmedStatus))}, agent: inRunCtx(base)}
	read := f.read()
	granted := tools.WithCallerTier(inRunCtx(f.ctx), tools.TierRun) // a run holding the tenant grants
	for _, op := range []string{"create_chunk", "upsert_chunk"} {
		_, r := f.do(granted, map[string]any{"op": op, "scope": "tenant", "document_id": read.DocumentID,
			"parent_id": read.SectionChunks[memrank.SectionClasses], "title": "Ledger " + op, "natural_key": "ledger-" + op,
			"status": memrank.OntologyStatusProposed, "body": "- `@locked`"})
		if !r.IsError || !strings.Contains(refusalText(r), entryLockedCode) {
			t.Errorf("an agent's raw %s proposal carrying @locked = %q, want refused", op, refusalText(r))
		}
	}
	// Without the line, the same raw proposal is filed: the refusal is the lock's.
	if _, r := f.do(granted, map[string]any{"op": "create_chunk", "scope": "tenant", "document_id": read.DocumentID,
		"parent_id": read.SectionChunks[memrank.SectionClasses], "title": "Journal",
		"status": memrank.OntologyStatusProposed, "body": "A journal."}); r.IsError {
		t.Errorf("an agent's raw proposal without @locked was refused: %s", refusalText(r))
	}
	out := f.mustPropose(map[string]any{"name": "Ledger", "body": "@locked"})
	if memrank.DeclaresLocked(f.body(out["chunk_id"].(string))) {
		t.Errorf("propose_entity filed a live @locked: %q", f.body(out["chunk_id"].(string)))
	}
	f.refused(map[string]any{"change": "edit", "entry": "Memo", "directives": map[string]any{"locked": true}}, "@locked cannot be set or cleared")
	f.refused(map[string]any{"change": "edit", "entry": "Reporting", "meaning": "Plain."}, entryLockedCode)
}

// TestOntology_LockedOnASectionHeaderIsAnIssue: on a section header or the root the
// line is reported and locks nothing — the entries below still retire and delete.
func TestOntology_LockedOnASectionHeaderIsAnIssue(t *testing.T) {
	md := strings.Replace(retiredOntologyMD, "@section classes\n", "@section classes\n@locked\n", 1)
	md = strings.Replace(md, "# Tenant Ontology\n", "# Tenant Ontology\n\n@locked\n", 1)
	d, base, _ := documentFixture(t)
	f := &retiredFixture{t: t, d: d, ctx: operatorCtx(installTenantOntology(t, d, base, md, memrank.OntologyConfirmedStatus))}
	issues := strings.Join(f.read().Issues, "\n")
	for _, want := range []string{"@locked on the ontology's root has no effect", "@locked on the classes section header has no effect"} {
		if !strings.Contains(issues, want) {
			t.Errorf("issues %q do not report %q", issues, want)
		}
	}
	for _, term := range f.read().Terms {
		if term.Locked {
			t.Errorf("class %s reads as locked", term.Name)
		}
	}
	if text := f.setStatus(f.ctx, f.chunk("Memo"), memrank.OntologyStatusRetired); text != "" {
		t.Errorf("retire under a header carrying @locked: %s", text)
	}
	if text := f.deleteChunk(f.ctx, f.chunk("Provision")); text != "" {
		t.Errorf("delete under a header carrying @locked: %s", text)
	}
}

// TestOntologyPlan_ReportsALockOnTheEntryAndItsAncestors: a planned retire, rename,
// move or delete of a locked entry or an entry above one carries an entry_locked note
// naming the locked entry; an edit (the text is not frozen) and an unlocked entry do not.
func TestOntologyPlan_ReportsALockOnTheEntryAndItsAncestors(t *testing.T) {
	f := newLockedFixture(t)
	internal, policy, memo := f.chunk("Internal Policy"), f.chunk("Policy"), f.chunk("Memo")
	plan, err := f.d.PlanOntology(f.ctx, []connector.OntologyChange{
		{Op: connector.OntologyPlanRetire, Entry: internal},
		{Op: connector.OntologyPlanRename, Entry: policy, Name: "Rule"},
		{Op: connector.OntologyPlanMove, Entry: policy, Parent: f.chunk("Provision")},
		{Op: connector.OntologyPlanDelete, Entry: policy},
		{Op: connector.OntologyPlanEdit, Entry: internal, Body: "@locked\nAn internal rule."},
		{Op: connector.OntologyPlanRetire, Entry: memo},
	})
	if err != nil {
		t.Fatal(err)
	}
	note := func(i int) string { return strings.Join(plan.Changes[i].Notes, "\n") }
	wantLocked(t, "planned retire", note(0), `class "Internal Policy" carries @locked`)
	for i := 1; i <= 3; i++ {
		wantLocked(t, "planned "+plan.Changes[i].Op+" of Policy", note(i), `above a locked entry (class "Internal Policy")`)
	}
	for _, i := range []int{4, 5} {
		if strings.Contains(note(i), entryLockedCode) {
			t.Errorf("planned %s of %s carries a lock note: %q", plan.Changes[i].Op, plan.Changes[i].Entry.Name, note(i))
		}
	}
}

// TestOntologyLocked_RefusedOnPostgres: the retire, move and delete refusals and the
// parsed flag on the postgres tier.
func TestOntologyLocked_RefusedOnPostgres(t *testing.T) {
	d, base := pgDocumentOrSkip(t)
	f := &retiredFixture{t: t, d: d, ctx: operatorCtx(installTenantOntology(t, d, base, lockedMD(), memrank.OntologyConfirmedStatus))}
	if c := conceptOfTag(f.read(), "topics/reporting"); !c.Locked {
		t.Fatalf("Reporting does not read as locked on postgres: %+v", c)
	}
	id := f.chunk("Reporting")
	wantLocked(t, "retire", f.setStatus(f.ctx, id, memrank.OntologyStatusRetired), `concept "Reporting"`)
	wantLocked(t, "move", f.move(f.ctx, id, f.chunk("Risk Management")), `concept "Reporting"`)
	wantLocked(t, "delete", f.deleteChunk(f.ctx, id), `concept "Reporting"`)
	wantLocked(t, "rename", f.update(f.ctx, id, map[string]any{"title": "Disclosure"}), `concept "Reporting"`)
	if !f.exists(id) || f.status(id) != "" {
		t.Errorf("after the refusals: exists=%v status=%q", f.exists(id), f.status(id))
	}
	// The #2762 review's SQL: the top-level check and the upsert refusal.
	wantLocked(t, "re-read the Schemes header", f.update(f.ctx, f.chunk("Schemes"), map[string]any{"body": "@section bogus"}), `concept "Reporting"`)
	read := f.read()
	f.must(map[string]any{"op": "upsert_chunk", "scope": "tenant", "document_id": read.DocumentID,
		"parent_id": read.SectionChunks[memrank.SectionClasses], "natural_key": "class:ledger", "title": "Ledger"})
	if _, r := f.do(f.ctx, map[string]any{"op": "upsert_chunk", "scope": "tenant", "document_id": read.DocumentID,
		"natural_key": "class:ledger", "title": "Journal"}); !r.IsError {
		t.Error("an upsert over an ontology entry went through on postgres")
	}
}
