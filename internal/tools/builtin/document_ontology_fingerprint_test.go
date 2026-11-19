package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/connector"
	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// newFingerprintFixture is the retired-entry fixture with something resting on three
// of its entries: a tenant document typed Memo, a document filed under the concept
// Reporting, and a statement of the kind supervises. Provision, event, filed_under and
// Acme have nothing resting on them.
func newFingerprintFixture(t *testing.T) *retiredFixture {
	t.Helper()
	f := newRetiredFixture(t)
	seedFingerprintDependents(f)
	return f
}

func seedFingerprintDependents(f *retiredFixture) {
	f.t.Helper()
	f.must(map[string]any{"op": "create_document", "scope": "tenant", "title": "A memo", "type": "Memo"})
	f.placeUnder("topics/reporting")
	a := f.must(map[string]any{"op": "create_document", "scope": "tenant", "title": "Alice"})
	b := f.must(map[string]any{"op": "create_document", "scope": "tenant", "title": "Bob"})
	f.must(map[string]any{"op": "link_chunks", "scope": "tenant", "from_id": a["root_chunk_id"], "to_id": b["root_chunk_id"], "kind": "supervises"})
}

// planFingerprint is the fingerprint the plan reports now, for any change.
func (f *retiredFixture) planFingerprint(entry string) string {
	f.t.Helper()
	plan, err := f.d.PlanOntology(f.ctx, []connector.OntologyChange{{Op: connector.OntologyPlanRetire, Entry: entry}})
	if err != nil || plan.Fingerprint == "" {
		f.t.Fatalf("plan: %v %+v", err, plan)
	}
	return plan.Fingerprint
}

// ontologyFingerprintNow is the tenant ontology's fingerprint on ctx, as a plan reports
// it, for a test that edits the ontology outside a retiredFixture.
func ontologyFingerprintNow(t *testing.T, d *Document, ctx context.Context) string {
	t.Helper()
	key, _, err := d.ontologyTenantKey(ctx)
	if err != nil {
		t.Fatal(err)
	}
	id, err := d.resolveOntologyID(ctx, key)
	if err != nil || id == "" {
		t.Fatalf("ontology id: %q %v", id, err)
	}
	fp, err := d.ontologyPlanFingerprint(ctx, key, id)
	if err != nil {
		t.Fatal(err)
	}
	return fp
}

// change is ctx's call of op on id with the given fields; update_chunk is sent at the
// chunk's current revision.
func (f *retiredFixture) change(ctx context.Context, op, id string, fields map[string]any) tools.Result {
	f.t.Helper()
	v := map[string]any{"op": op, "scope": "tenant", "id": id}
	if op == "update_chunk" {
		g := f.must(map[string]any{"op": "get_chunk", "scope": "tenant", "id": id})
		v["revision"] = int(g["revision"].(float64))
	}
	for k, x := range fields {
		v[k] = x
	}
	_, r := f.do(ctx, v)
	return r
}

// planRefusalOf parses a plan_required / plan_stale refusal; ok is false for any other
// result.
func planRefusalOf(r tools.Result) (code, fingerprint string, ok bool) {
	if !r.IsError {
		return "", "", false
	}
	var body struct {
		Code        string `json:"code"`
		Error       string `json:"error"`
		Fingerprint string `json:"fingerprint"`
	}
	if json.Unmarshal([]byte(r.Text), &body) != nil || body.Error == "" {
		return "", "", false
	}
	return body.Code, body.Fingerprint, true
}

func wantPlanRefusal(t *testing.T, what string, r tools.Result, code, fingerprint string) {
	t.Helper()
	got, fp, ok := planRefusalOf(r)
	if !ok || got != code || fp != fingerprint {
		t.Errorf("%s = %q, want a %s refusal carrying the current fingerprint %s", what, r.Text, code, fingerprint)
	}
	if r.Error == nil || r.Error.Category != tools.CategoryBusiness {
		t.Errorf("%s: category %+v, want business", what, r.Error)
	}
}

// TestOntologyEdit_RenameWithoutPlanIsRefused (RFC GQ D2): a rename, a retire, a move
// under another parent and a characteristic edit of an entry something rests on are
// refused without a plan's fingerprint, for the operator and an admin alike, with the
// fingerprint a plan reports now; nothing changes.
func TestOntologyEdit_RenameWithoutPlanIsRefused(t *testing.T) {
	f := newFingerprintFixture(t)
	memo, reporting, supervises := f.chunk("Memo"), f.chunk("Reporting"), f.chunk("supervises")
	fp := f.planFingerprint(memo)
	for name, ctx := range map[string]context.Context{"operator": f.ctx, "admin": adminCtx(f.ctx)} {
		wantPlanRefusal(t, name+" rename Memo", f.change(ctx, "update_chunk", memo, map[string]any{"title": "Memorandum"}), planRequiredCode, fp)
		wantPlanRefusal(t, name+" retire Reporting", f.change(ctx, "update_chunk", reporting, map[string]any{"status": memrank.OntologyStatusRetired}), planRequiredCode, fp)
		wantPlanRefusal(t, name+" move Memo", f.change(ctx, "move_chunk", memo, map[string]any{"new_parent_id": f.chunk("Provision")}), planRequiredCode, fp)
		wantPlanRefusal(t, name+" make supervises functional", f.change(ctx, "update_chunk", supervises, map[string]any{"body": "@functional"}), planRequiredCode, fp)
	}
	if _, ok := termByName(f.read(), "Memo"); !ok || termParent(t, f.read(), "Memo") != "Document" {
		t.Error("Memo was renamed or moved")
	}
	if st := f.status(reporting); st != "" {
		t.Errorf("Reporting status = %q after the refusal", st)
	}
	if b := f.body(supervises); strings.Contains(b, "@functional") {
		t.Errorf("supervises body = %q after the refusal", b)
	}
	if got := f.planFingerprint(memo); got != fp {
		t.Errorf("the refusals moved the fingerprint %s → %s", fp, got)
	}
}

// TestOntologyEdit_StaleFingerprintIsRefused (RFC GQ check P2): a fingerprint from a
// plan made before another change to the ontology is refused with plan_stale, carrying
// the current one; sent again with that, the change applies.
func TestOntologyEdit_StaleFingerprintIsRefused(t *testing.T) {
	f := newFingerprintFixture(t)
	reporting := f.chunk("Reporting")
	old := f.planFingerprint(reporting)
	if r := f.change(f.ctx, "update_chunk", f.chunk("Provision"), map[string]any{"body": "A clause of an act."}); r.IsError {
		t.Fatalf("meaning edit of Provision: %s", r.Text)
	}
	now := f.planFingerprint(reporting)
	if now == old {
		t.Fatal("an edit of the ontology left the fingerprint as it was")
	}
	retire := map[string]any{"status": memrank.OntologyStatusRetired, "plan_fingerprint": old}
	wantPlanRefusal(t, "retire with a stale fingerprint", f.change(f.ctx, "update_chunk", reporting, retire), planStaleCode, now)
	if st := f.status(reporting); st != "" {
		t.Errorf("Reporting status = %q after the stale refusal", st)
	}
	retire["plan_fingerprint"] = now
	if r := f.change(f.ctx, "update_chunk", reporting, retire); r.IsError {
		t.Fatalf("retire with the current fingerprint: %s", r.Text)
	}
	if st := f.status(reporting); st != memrank.OntologyStatusRetired {
		t.Errorf("Reporting status = %q, want retired", st)
	}
}

// TestOntologyEdit_FreshFingerprintApplies: each refused change of the first test
// applies when it carries the fingerprint a plan reports now.
func TestOntologyEdit_FreshFingerprintApplies(t *testing.T) {
	f := newFingerprintFixture(t)
	memo := f.chunk("Memo")
	steps := []struct {
		what, op, id string
		fields       map[string]any
	}{
		{"rename Memo", "update_chunk", memo, map[string]any{"title": "Memorandum"}},
		{"move Memo", "move_chunk", memo, map[string]any{"new_parent_id": f.chunk("Provision")}},
		{"make supervises functional", "update_chunk", f.chunk("supervises"), map[string]any{"body": "@functional"}},
		{"retire Reporting", "update_chunk", f.chunk("Reporting"), map[string]any{"status": memrank.OntologyStatusRetired}},
	}
	for _, s := range steps {
		s.fields["plan_fingerprint"] = f.planFingerprint(memo)
		if r := f.change(f.ctx, s.op, s.id, s.fields); r.IsError {
			t.Fatalf("%s with a fresh fingerprint: %s", s.what, r.Text)
		}
	}
	if termParent(t, f.read(), "Memorandum") != "Provision" {
		t.Errorf("Memorandum's parent = %q, want Provision", termParent(t, f.read(), "Memorandum"))
	}
	if !relationByName(f.read(), "supervises").Functional {
		t.Error("supervises is not functional")
	}
}

func relationByName(r OntologyRead, name string) memrank.OntologyRelation {
	for _, rel := range r.Relations {
		if rel.Name == name {
			return rel
		}
	}
	return memrank.OntologyRelation{}
}

// TestOntologyEdit_MeaningEditNeedsNoPlan (D2): an edit of the meaning alone, a reorder
// and a move under the same parent stay direct on an entry something rests on.
func TestOntologyEdit_MeaningEditNeedsNoPlan(t *testing.T) {
	f := newFingerprintFixture(t)
	memo := f.chunk("Memo")
	if r := f.change(f.ctx, "update_chunk", memo, map[string]any{"body": "A short internal note."}); r.IsError {
		t.Errorf("meaning edit of Memo: %s", r.Text)
	}
	if r := f.change(f.ctx, "update_chunk", f.chunk("supervises"), map[string]any{"body": "Who answers to whom."}); r.IsError {
		t.Errorf("meaning edit of supervises: %s", r.Text)
	}
	if r := f.change(f.ctx, "reorder_chunk", memo, map[string]any{"direction": "up"}); r.IsError {
		t.Errorf("reorder of Memo: %s", r.Text)
	}
	if r := f.change(f.ctx, "move_chunk", memo, map[string]any{"new_parent_id": f.chunk("Document"), "position": 0}); r.IsError {
		t.Errorf("move of Memo under the parent it has: %s", r.Text)
	}
	if r := f.change(f.ctx, "update_chunk", memo, map[string]any{"title": "Memo", "tags": []string{"kept"}}); r.IsError {
		t.Errorf("update of Memo that keeps its name: %s", r.Text)
	}
}

// TestOntologyEdit_RenameOfEntryWithoutDependentsNeedsNoPlan: the plan says none for an
// entry nothing rests on, so its rename, retire, move and characteristic edit stay
// direct.
func TestOntologyEdit_RenameOfEntryWithoutDependentsNeedsNoPlan(t *testing.T) {
	f := newFingerprintFixture(t)
	steps := []struct {
		what, op, id string
		fields       map[string]any
	}{
		{"rename Provision", "update_chunk", f.chunk("Provision"), map[string]any{"title": "Clause"}},
		{"retire event", "update_chunk", f.chunk("event"), map[string]any{"status": memrank.OntologyStatusRetired}},
		{"move Acme", "move_chunk", f.chunk("Acme"), map[string]any{"new_parent_id": f.chunk("Globex")}},
		{"make filed_under functional", "update_chunk", f.chunk("filed_under"), map[string]any{"body": "@range concept\n@functional"}},
	}
	for _, s := range steps {
		if r := f.change(f.ctx, s.op, s.id, s.fields); r.IsError {
			t.Errorf("%s: %s", s.what, r.Text)
		}
	}
}

// TestOntologyEdit_LockedEntryAnswersLockedBeforePlan (P2.3 before P2.4): a locked entry
// something rests on answers entry_locked to a rename, retire or move, not
// plan_required — a fingerprint could not make any of them apply.
func TestOntologyEdit_LockedEntryAnswersLockedBeforePlan(t *testing.T) {
	f := newLockedFixture(t)
	seedFingerprintDependents(f)
	id := f.chunk("Reporting")
	for what, r := range map[string]tools.Result{
		"rename": f.change(f.ctx, "update_chunk", id, map[string]any{"title": "Disclosure"}),
		"retire": f.change(f.ctx, "update_chunk", id, map[string]any{"status": memrank.OntologyStatusRetired}),
		"move":   f.change(f.ctx, "move_chunk", id, map[string]any{"new_parent_id": f.chunk("Risk Management")}),
	} {
		wantLocked(t, what, r.Text, `concept "Reporting"`)
	}
}

// TestOntologyEdit_FingerprintRefusedOnPostgres: the refusal and the apply on the
// postgres tier.
func TestOntologyEdit_FingerprintRefusedOnPostgres(t *testing.T) {
	d, base := pgDocumentOrSkip(t)
	f := &retiredFixture{t: t, d: d, ctx: operatorCtx(installTenantOntology(t, d, base, retiredOntologyMD, memrank.OntologyConfirmedStatus))}
	seedFingerprintDependents(f)
	memo := f.chunk("Memo")
	fp := f.planFingerprint(memo)
	wantPlanRefusal(t, "rename Memo", f.change(f.ctx, "update_chunk", memo, map[string]any{"title": "Memorandum"}), planRequiredCode, fp)
	wantPlanRefusal(t, "rename Memo, stale", f.change(f.ctx, "update_chunk", memo, map[string]any{"title": "Memorandum", "plan_fingerprint": strings.Repeat("0", 64)}), planStaleCode, fp)
	if r := f.change(f.ctx, "update_chunk", memo, map[string]any{"title": "Memorandum", "plan_fingerprint": fp}); r.IsError {
		t.Fatalf("rename with the fingerprint: %s", r.Text)
	}
	if r := f.change(f.ctx, "update_chunk", f.chunk("Provision"), map[string]any{"title": "Clause"}); r.IsError {
		t.Errorf("rename of an entry nothing rests on: %s", r.Text)
	}
}
