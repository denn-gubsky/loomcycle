package builtin

import (
	"context"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/connector"
	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// The owner's RFC GQ rulings of 2026-11-20: any change to the directives of an entry
// something rests on is planned as stales and needs a plan's fingerprint, and a direct
// rename of a class something rests on is refused (rename_job_pending) until RFC GQ P5
// carries the new name through stored data.

// directiveEdits are edits of each kind of in-use entry of the fingerprint fixture that
// change a directive the old signature or effect table did not count, with the meaning
// kept: a class's @alt and @annotate, a concept's @alt, @notation and @exact_match, a
// scheme's @period and @domain, a relation's @walk.
func directiveEdits(f *retiredFixture) []struct{ what, id, body string } {
	return []struct{ what, id, body string }{
		{"@alt on the class Memo", f.chunk("Memo"), "@alt note"},
		{"@annotate on the class Memo", f.chunk("Memo"), "@annotate purpose; owner"},
		{"@alt on the concept Reporting", f.chunk("Reporting"), "@alt disclosure"},
		{"@notation on the concept Reporting", f.chunk("Reporting"), "@notation R1"},
		{"@exact_match on the concept Reporting", f.chunk("Reporting"), "@exact_match http://example.org/reporting"},
		{"@period on the scheme topics", f.chunk("topics"), "@render\n@period week"},
		{"@domain on the scheme topics", f.chunk("topics"), "@render\n@domain compliance"},
		{"@walk on the relation supervises", f.chunk("supervises"), "@walk"},
	}
}

// TestOntologyFingerprint_AnyDirectiveChangeNeedsAPlan: a change to any directive of an
// entry something rests on plans as stales, is refused without a plan's fingerprint and
// applies with it.
func TestOntologyFingerprint_AnyDirectiveChangeNeedsAPlan(t *testing.T) {
	f := newFingerprintFixture(t)
	for _, e := range directiveEdits(f) {
		plan, err := f.d.PlanOntology(f.ctx, []connector.OntologyChange{{Op: connector.OntologyPlanEdit, Entry: e.id, Body: e.body}})
		if err != nil {
			t.Fatalf("plan %s: %v", e.what, err)
		}
		if got := plan.Changes[0].Risk; got != connector.OntologyRiskStales {
			t.Errorf("plan of %s: risk %q, want stales (%+v)", e.what, got, plan.Changes[0])
		}
		wantPlanRefusal(t, e.what, f.change(f.ctx, "update_chunk", e.id, map[string]any{"body": e.body}), planRequiredCode, plan.Fingerprint)
	}
	for _, e := range directiveEdits(f) {
		fp := f.planFingerprint(f.chunk("Memo"))
		if r := f.change(f.ctx, "update_chunk", e.id, map[string]any{"body": e.body, "plan_fingerprint": fp}); r.IsError {
			t.Errorf("%s with a fresh fingerprint: %s", e.what, r.Text)
		}
	}
	if c := conceptOfTag(f.read(), "topics/reporting"); c.ExactMatch == "" {
		t.Errorf("Reporting after the edits = %+v, want its exact_match set", c)
	}
}

// TestOntologyFingerprint_EveryDirectiveOfASectionIsPlanned: each directive the
// classes, relations and schemes sections read (a scheme header's and a concept's),
// set with a value its parser keeps, makes the edit of an entry in use plan as stales:
// the plan's signature of an entry leaves none of them out. A value the parser drops
// (an unknown qualifier, a pattern that does not compile) is an issue, not a directive
// change, and is not listed.
func TestOntologyFingerprint_EveryDirectiveOfASectionIsPlanned(t *testing.T) {
	f := newFingerprintFixture(t)
	cases := []struct {
		title, base string
		directives  []string
	}{
		{"Memo", "", []string{"@memory_scope tenant", "@identity", `@identifier_pattern \d+`, `@title_pattern \d+`, "@anchor_grammar g",
			"@citation_grammar g", "@exclude a", "@renamed_from old", "@definitions x", "@boilerplate x", "@norm", "@annotate a", "@alt a", "@locked"}},
		{"supervises", "", []string{"@inverse supervised_by", "@domain person", "@range person", "@symmetric", "@asymmetric", "@irreflexive",
			"@transitive", "@qualifiers aspect", "@qualifiers aspect\n@transitive_within aspect", "@qualifiers aspect\n@discriminator aspect",
			"@functional", "@dated", "@walk", "@auto_assert", "@renamed_from overseen", "@locked"}},
		{"topics", "@render\n", []string{"@ordered", "@domain d", "@projects Memo", "@origin adopted", "@source s", "@version 1", "@period week", "@locked"}},
		{"topics", "", []string{""}}, // @render dropped
		{"Reporting", "", []string{"@notation R", "@alt a", "@exact_match x", "@focus Acme", "@retention 1y", "@acl g", "@locked"}},
	}
	for _, c := range cases {
		id := f.chunk(c.title)
		for _, d := range c.directives {
			body := c.base + d
			if body == "" {
				body = "No directives."
			}
			plan, err := f.d.PlanOntology(f.ctx, []connector.OntologyChange{{Op: connector.OntologyPlanEdit, Entry: id, Body: body}})
			if err != nil {
				t.Errorf("plan %s %q: %v", c.title, body, err)
				continue
			}
			if got := plan.Changes[0].Risk; got != connector.OntologyRiskStales {
				t.Errorf("edit of %s to %q: risk %q, want stales (notes %v)", c.title, body, got, plan.Changes[0].Notes)
			}
		}
	}
}

// TestOntologyFingerprint_MeaningOnlyEditNeedsNoPlan: an edit of the meaning alone,
// with every directive kept, plans as none and applies without a fingerprint, on each
// kind of in-use entry.
func TestOntologyFingerprint_MeaningOnlyEditNeedsNoPlan(t *testing.T) {
	f := newFingerprintFixture(t)
	for _, e := range []struct{ what, id, body string }{
		{"the class Memo", f.chunk("Memo"), "A short internal note."},
		{"the concept Reporting", f.chunk("Reporting"), "What is reported to the regulator."},
		{"the scheme topics", f.chunk("topics"), "@render\nThe topics documents are filed under."},
		{"the relation supervises", f.chunk("supervises"), "Who answers to whom."},
	} {
		plan, err := f.d.PlanOntology(f.ctx, []connector.OntologyChange{{Op: connector.OntologyPlanEdit, Entry: e.id, Body: e.body}})
		if err != nil {
			t.Fatalf("plan %s: %v", e.what, err)
		}
		if got := plan.Changes[0].Risk; got != connector.OntologyRiskNone {
			t.Errorf("plan of a meaning edit of %s: risk %q, want none", e.what, got)
		}
		if r := f.change(f.ctx, "update_chunk", e.id, map[string]any{"body": e.body}); r.IsError {
			t.Errorf("meaning edit of %s: %s", e.what, r.Text)
		}
	}
}

// wantRenameJobPending checks a rename_job_pending refusal naming the class.
func wantRenameJobPending(t *testing.T, what string, r tools.Result, class string) {
	t.Helper()
	if !r.IsError || !strings.Contains(r.Text, renameJobPendingCode) || !strings.Contains(r.Text, `"`+class+`"`) {
		t.Errorf("%s = %q, want a %s refusal naming %q", what, r.Text, renameJobPendingCode, class)
		return
	}
	if r.Error == nil || r.Error.Category != tools.CategoryBusiness {
		t.Errorf("%s: category %+v, want business", what, r.Error)
	}
}

// planNotes is the notes of a one-change plan.
func (f *retiredFixture) planNotes(ch connector.OntologyChange) string {
	f.t.Helper()
	plan, err := f.d.PlanOntology(f.ctx, []connector.OntologyChange{ch})
	if err != nil {
		f.t.Fatalf("plan: %v", err)
	}
	return strings.Join(plan.Changes[0].Notes, "\n")
}

// TestOntologyRename_DirectClassRenameInUseIsRefused: renaming a class something rests
// on, by its title or by its body's heading, is refused with rename_job_pending for the
// operator and an admin, with or without a fingerprint; the plan says so in its notes.
// Here the rename job cannot carry it (RFC GQ P5.3): a row in a user scope carries the
// class, and the job rewrites the tenant scope only so far.
func TestOntologyRename_DirectClassRenameInUseIsRefused(t *testing.T) {
	f := newFingerprintFixture(t)
	f.must(map[string]any{"op": "create_document", "scope": "user", "title": "My memo", "type": "Memo"})
	memo := f.chunk("Memo")
	for name, ctx := range map[string]context.Context{"operator": f.ctx, "admin": adminCtx(f.ctx)} {
		wantRenameJobPending(t, name+" rename without a fingerprint", f.change(ctx, "update_chunk", memo, map[string]any{"title": "Memorandum"}), "Memo")
		fp := f.planFingerprint(memo)
		wantRenameJobPending(t, name+" rename with a fresh fingerprint", f.change(ctx, "update_chunk", memo, map[string]any{"title": "Memorandum", "plan_fingerprint": fp}), "Memo")
		wantRenameJobPending(t, name+" rename by the body's heading", f.change(ctx, "update_chunk", memo, map[string]any{"title": "", "body": "# Memorandum", "plan_fingerprint": fp}), "Memo")
	}
	if _, ok := termByName(f.read(), "Memo"); !ok {
		t.Error("Memo was renamed")
	}
	if notes := f.planNotes(connector.OntologyChange{Op: connector.OntologyPlanRename, Entry: memo, Name: "Memorandum"}); !strings.Contains(notes, renameJobPendingCode) {
		t.Errorf("plan of the rename: notes %q, want %s", notes, renameJobPendingCode)
	}
	if notes := f.planNotes(connector.OntologyChange{Op: connector.OntologyPlanEdit, Entry: memo, Body: "# Memorandum"}); strings.Contains(notes, renameJobPendingCode) {
		// Memo has a title, so a heading in its body names nothing.
		t.Errorf("plan of a body edit of the titled Memo: notes %q, want no %s", notes, renameJobPendingCode)
	}
	// The edit that keeps the name, and a directive change with a fingerprint, still apply.
	if r := f.change(f.ctx, "update_chunk", memo, map[string]any{"title": "Memo", "body": "@alt note", "plan_fingerprint": f.planFingerprint(memo)}); r.IsError {
		t.Errorf("an edit of Memo that keeps its name: %s", r.Text)
	}
	// Untitled, named by its body's heading: a plan of an edit whose heading renames it
	// reports the refusal too.
	if r := f.change(f.ctx, "update_chunk", memo, map[string]any{"title": "", "body": "# Memo\n@alt note"}); r.IsError {
		t.Fatalf("moving Memo's name into its heading: %s", r.Text)
	}
	if notes := f.planNotes(connector.OntologyChange{Op: connector.OntologyPlanEdit, Entry: memo, Body: "# Memorandum\n@alt note"}); !strings.Contains(notes, renameJobPendingCode) {
		t.Errorf("plan of a heading edit renaming the untitled Memo: notes %q, want %s", notes, renameJobPendingCode)
	}
	wantRenameJobPending(t, "rename of the untitled Memo by its heading", f.change(f.ctx, "update_chunk", memo, map[string]any{"body": "# Memorandum\n@alt note"}), "Memo")
}

// TestOntologyRename_ClassNothingRestsOnRenames: a class nothing rests on renames
// directly, and its plan carries no rename_job_pending note.
func TestOntologyRename_ClassNothingRestsOnRenames(t *testing.T) {
	f := newFingerprintFixture(t)
	provision := f.chunk("Provision")
	// Before the ruling the plan noted every class rename as refused until the rename
	// job exists, whether or not anything rested on it.
	if notes := f.planNotes(connector.OntologyChange{Op: connector.OntologyPlanRename, Entry: provision, Name: "Clause"}); strings.Contains(notes, "rename job") {
		t.Errorf("plan of renaming Provision: notes %q, want none about the rename job", notes)
	}
	if r := f.change(f.ctx, "update_chunk", provision, map[string]any{"title": "Clause"}); r.IsError {
		t.Fatalf("rename of a class nothing rests on: %s", r.Text)
	}
	if _, ok := termByName(f.read(), "Clause"); !ok {
		t.Error("Provision was not renamed to Clause")
	}
}

// TestOntologyRename_RelationAndConceptStillRenameWithAPlan: a relation or a concept
// something rests on renames with a plan's fingerprint and is never answered
// rename_job_pending. The relation's statements are keyed by its name, so without a
// fingerprint it is plan_required; the concept's members and statements are keyed by its
// chunk id, so the plan finds nothing a rename stales and it renames directly, as before.
func TestOntologyRename_RelationAndConceptStillRenameWithAPlan(t *testing.T) {
	f := newFingerprintFixture(t)
	supervises := f.chunk("supervises")
	wantPlanRefusal(t, "rename the relation supervises", f.change(f.ctx, "update_chunk", supervises, map[string]any{"title": "oversees"}),
		planRequiredCode, f.planFingerprint(supervises))
	for _, e := range []struct{ what, title, name string }{
		{"the relation supervises", "supervises", "oversees"},
		{"the concept Reporting", "Reporting", "Disclosure"},
	} {
		id := f.chunk(e.title)
		if notes := f.planNotes(connector.OntologyChange{Op: connector.OntologyPlanRename, Entry: id, Name: e.name}); strings.Contains(notes, renameJobPendingCode) {
			t.Errorf("plan of renaming %s: notes %q, want no %s", e.what, notes, renameJobPendingCode)
		}
		if r := f.change(f.ctx, "update_chunk", id, map[string]any{"title": e.name, "plan_fingerprint": f.planFingerprint(id)}); r.IsError {
			t.Errorf("rename %s with a fresh fingerprint: %s", e.what, r.Text)
		}
	}
	if relationByName(f.read(), "oversees").Name == "" {
		t.Error("supervises was not renamed")
	}
	if conceptOfTag(f.read(), "topics/disclosure").ChunkID == "" {
		t.Error("Reporting was not renamed")
	}
}

// TestOntologyPlan_DirectiveAndRenameRulingsOnPostgres: both rulings on the postgres
// tier.
func TestOntologyPlan_DirectiveAndRenameRulingsOnPostgres(t *testing.T) {
	d, base := pgDocumentOrSkip(t)
	f := &retiredFixture{t: t, d: d, ctx: operatorCtx(installTenantOntology(t, d, base, retiredOntologyMD, memrank.OntologyConfirmedStatus))}
	seedFingerprintDependents(f)
	// A user-scope row the rename job cannot reach yet (RFC GQ P5.3).
	f.must(map[string]any{"op": "create_document", "scope": "user", "title": "My memo", "type": "Memo"})
	memo, reporting := f.chunk("Memo"), f.chunk("Reporting")
	fp := f.planFingerprint(memo)
	wantPlanRefusal(t, "@alt on Reporting", f.change(f.ctx, "update_chunk", reporting, map[string]any{"body": "@alt disclosure"}), planRequiredCode, fp)
	if r := f.change(f.ctx, "update_chunk", reporting, map[string]any{"body": "What is reported."}); r.IsError {
		t.Errorf("meaning edit of Reporting: %s", r.Text)
	}
	wantRenameJobPending(t, "rename Memo with a fingerprint", f.change(f.ctx, "update_chunk", memo, map[string]any{"title": "Memorandum", "plan_fingerprint": f.planFingerprint(memo)}), "Memo")
	if r := f.change(f.ctx, "update_chunk", f.chunk("Provision"), map[string]any{"title": "Clause"}); r.IsError {
		t.Errorf("rename of a class nothing rests on: %s", r.Text)
	}
	if r := f.change(f.ctx, "update_chunk", reporting, map[string]any{"title": "Disclosure", "plan_fingerprint": f.planFingerprint(memo)}); r.IsError {
		t.Errorf("rename of a concept with a fingerprint: %s", r.Text)
	}
}

// TestOntologyFingerprint_DirectiveEditOfASecondSameNamedClassNeedsAPlan: of two
// in-force classes whose names differ only in case, the second is an entry of its own,
// and a directive edit of it, with something resting on it, needs a plan like any
// other: its signature is read by its own chunk, not by the chunk its name maps to.
func TestOntologyFingerprint_DirectiveEditOfASecondSameNamedClassNeedsAPlan(t *testing.T) {
	d, base, _ := documentFixture(t)
	md := "# Tenant Ontology\n\n## Classes\n@section classes\n\n### Document\n- `title` text\n\n#### Memo\n\n#### memo\n"
	f := &retiredFixture{t: t, d: d, ctx: operatorCtx(installTenantOntology(t, d, base, md, memrank.OntologyConfirmedStatus))}
	f.must(map[string]any{"op": "create_document", "scope": "tenant", "title": "A memo", "type": "memo"})
	second := f.chunk("memo")
	if _, ok := planEntries(f.read())[second]; !ok {
		t.Fatal("the second class is not an entry of its own")
	}
	plan, err := f.d.PlanOntology(f.ctx, []connector.OntologyChange{{Op: connector.OntologyPlanEdit, Entry: second, Body: "@alt note"}})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if got := plan.Changes[0].Risk; got != connector.OntologyRiskStales {
		t.Errorf("plan of @alt on the second class: risk %q, want stales (%+v)", got, plan.Changes[0])
	}
	wantPlanRefusal(t, "@alt on the second class", f.change(f.ctx, "update_chunk", second, map[string]any{"body": "@alt note"}), planRequiredCode, plan.Fingerprint)
}
