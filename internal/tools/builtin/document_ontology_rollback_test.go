package builtin

import (
	"context"
	"errors"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/connector"
	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
)

// RFC GQ P5.7 — rolling the ontology back to a release.

// rollbackFixtureOn is a populated tenant ontology, confirmed, with its confirm release
// taken: a document typed Memo, a supervises statement, and documents placed under
// topics/reporting and under the concept outsourcing — so the changes the tests make
// since the release rest on something.
func rollbackFixtureOn(t *testing.T, d *Document, base context.Context) (changeFixture, OntologyRelease) {
	t.Helper()
	f := releaseFixtureOn(t, d, base)
	seedFingerprintDependents(f.retiredFixture)
	f.placeUnder("topics/risk-management/outsourcing")
	return f, takeRelease(t, d, f.ctx, OntologyReleaseConfirm)
}

// rollbackPlan plans rolling back to release seq.
func (f changeFixture) rollbackPlan(seq int64) connector.PlanOntologyResult {
	f.t.Helper()
	plan, err := f.d.PlanOntology(f.ctx, []connector.OntologyChange{{Op: connector.OntologyPlanRelease, Seq: seq}})
	if err != nil {
		f.t.Fatalf("plan the rollback to %d: %v", seq, err)
	}
	return plan
}

// addClass declares a class directly in the confirmed ontology, under the Classes
// section, and returns its chunk id.
func (f changeFixture) addClass(name string) string {
	f.t.Helper()
	out := f.must(map[string]any{"op": "create_chunk", "scope": "tenant", "document_id": f.read().DocumentID,
		"parent_id": f.chunk("Classes"), "title": name})
	return out["id"].(string)
}

// editSinceRelease makes, after a release, one change of each kind a rollback undoes in
// place: a relation renamed, a class's meaning edited, a class retired, a concept moved
// under another, and a class added (nothing rests on it). It returns each entry's id by
// what was done to it.
func (f changeFixture) editSinceRelease() map[string]string {
	f.t.Helper()
	ids := map[string]string{"renamed": f.chunk("supervises"), "edited": f.chunk("event"), "retired": f.chunk("Memo"),
		"moved": f.chunk("outsourcing")}
	if r := f.change(f.ctx, "update_chunk", ids["renamed"], map[string]any{"title": "oversees", "plan_fingerprint": f.planFingerprint(ids["renamed"])}); r.IsError {
		f.t.Fatalf("rename: %s", r.Text)
	}
	if r := f.change(f.ctx, "update_chunk", ids["edited"], map[string]any{"body": "Something that happened.\n- `name` text\n"}); r.IsError {
		f.t.Fatalf("edit: %s", r.Text)
	}
	f.retire("Memo")
	if r := f.change(f.ctx, "move_chunk", ids["moved"], map[string]any{"new_parent_id": f.chunk("Reporting"),
		"plan_fingerprint": f.planFingerprint(ids["moved"])}); r.IsError {
		f.t.Fatalf("move: %s", r.Text)
	}
	ids["added"] = f.addClass("Ledger")
	return ids
}

// TestRollback_PlanToAReleaseListsEveryDifference: the plan of a rollback names every
// entry the release diff reports, each change carrying the release — the renamed
// relation renamed back, the edited class edited back, the retired class unretired, the
// moved concept moved back, the class added since deleted, and the class deleted since
// added back — and writes nothing.
func TestRollback_PlanToAReleaseListsEveryDifference(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, base context.Context) {
		f, rel := rollbackFixtureOn(t, d, base)
		ids := f.editSinceRelease()
		ids["deleted"] = f.chunk("Provision")
		f.must(map[string]any{"op": "delete_chunk", "scope": "tenant", "id": ids["deleted"]})
		before := f.fingerprintNow()

		plan := f.rollbackPlan(rel.Seq)
		ops := map[string][]string{}
		for _, ch := range plan.Changes {
			if ch.Release != rel.Seq {
				t.Errorf("change %s of %s carries release %d, want %d", ch.Op, ch.Entry.Name, ch.Release, rel.Seq)
			}
			ops[ch.Entry.ChunkID] = append(ops[ch.Entry.ChunkID], ch.Op)
		}
		for _, c := range f.diff(rel.Seq, 0).Changes {
			if len(ops[c.ChunkID]) == 0 {
				t.Errorf("the diff's %s of %s (%s) is not in the plan: %s", c.Change, c.ChunkID, c.Kind, diffJSON(plan.Changes))
			}
		}
		want := map[string]string{"renamed": connector.OntologyPlanRename, "edited": connector.OntologyPlanEdit,
			"retired": connector.OntologyPlanUnretire, "moved": connector.OntologyPlanMove, "added": connector.OntologyPlanDelete,
			"deleted": connector.OntologyPlanAdd}
		for what, op := range want {
			if got := ops[ids[what]]; len(got) != 1 || got[0] != op {
				t.Errorf("the %s entry is planned as %v, want [%s]", what, got, op)
			}
		}
		if len(ops) != len(want) {
			t.Errorf("the plan names %d entries, want %d: %s", len(ops), len(want), diffJSON(plan.Changes))
		}
		if plan.Fingerprint != before || f.fingerprintNow() != before {
			t.Error("planning a rollback changed the ontology, or reported another fingerprint")
		}
	})
}

// TestRollback_ApplyReturnsTheContentVersionOfTheRelease: applying a rollback with its
// plan's fingerprint leaves the ontology declaring what the release declared — its
// content version is the release's — and takes a rollback release of it.
func TestRollback_ApplyReturnsTheContentVersionOfTheRelease(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, base context.Context) {
		f, rel := rollbackFixtureOn(t, d, base)
		ids := f.editSinceRelease()
		if currentContentVersion(t, d, f.ctx) == rel.ContentVersion {
			t.Fatal("the edits did not move the content version; the test would prove nothing")
		}

		rep, err := d.ApplyOntologyRollback(f.ctx, rel.Seq, f.rollbackPlan(rel.Seq).Fingerprint)
		if err != nil {
			t.Fatalf("apply: %v", err)
		}
		if rep.ContentVersion != rel.ContentVersion || !rep.Matches || rep.ReleaseContentVersion != rel.ContentVersion {
			t.Errorf("report = %s, want the content version %s, matching", diffJSON(rep), rel.ContentVersion)
		}
		if now := currentContentVersion(t, d, f.ctx); now != rel.ContentVersion {
			t.Errorf("the content version in force is %s, want the release's %s", now, rel.ContentVersion)
		}
		if got := f.diff(rel.Seq, 0); len(got.Changes) != 0 {
			t.Errorf("after the rollback the diff to the release = %s, want none", diffJSON(got.Changes))
		}
		if f.exists(ids["added"]) {
			t.Error("the class added since the release, which nothing rests on, was not deleted")
		}
		newest := listedReleases(t, d, f.ctx)[0]
		if rep.RollbackRelease != newest.Seq || newest.Reason != OntologyReleaseRollback || newest.ContentVersion != rel.ContentVersion {
			t.Errorf("newest release = %+v (report says %d), want a rollback release of %s", newest, rep.RollbackRelease, rel.ContentVersion)
		}
	})
}

// TestRollback_EntryAddedSinceWithDependentsIsRetiredNotDeleted: of two classes added
// since the release, the one a document is typed with is planned and applied as a
// retirement — it stays, retired, and the document keeps its type — while the one
// nothing rests on is deleted (D3).
func TestRollback_EntryAddedSinceWithDependentsIsRetiredNotDeleted(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, base context.Context) {
		f, rel := rollbackFixtureOn(t, d, base)
		ledger, journal := f.addClass("Ledger"), f.addClass("Journal")
		doc := f.must(map[string]any{"op": "create_document", "scope": "tenant", "title": "Q3 ledger", "type": "Ledger"})

		plan := f.rollbackPlan(rel.Seq)
		op := map[string]string{}
		for _, ch := range plan.Changes {
			op[ch.Entry.ChunkID] = ch.Op
		}
		if op[ledger] != connector.OntologyPlanRetire || op[journal] != connector.OntologyPlanDelete {
			t.Fatalf("planned Ledger %q and Journal %q, want retire and delete: %s", op[ledger], op[journal], diffJSON(plan.Changes))
		}
		if _, err := d.ApplyOntologyRollback(f.ctx, rel.Seq, plan.Fingerprint); err != nil {
			t.Fatalf("apply: %v", err)
		}
		if !f.exists(ledger) || f.status(ledger) != memrank.OntologyStatusRetired {
			t.Errorf("Ledger exists %v with status %q, want kept and retired", f.exists(ledger), f.status(ledger))
		}
		if f.exists(journal) {
			t.Error("Journal, which nothing rests on, was not deleted")
		}
		if g := f.must(map[string]any{"op": "get_chunk", "scope": "tenant", "id": doc["root_chunk_id"]}); g["type"] != "Ledger" {
			t.Errorf("the typed document lost its type: %v", g["type"])
		}
	})
}

// TestRollback_StaleFingerprintIsRefused: an apply without a fingerprint, or with one
// from before the ontology changed, is refused, each refusal carrying the current one,
// and nothing is written; the current fingerprint then applies.
func TestRollback_StaleFingerprintIsRefused(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, base context.Context) {
		f, rel := rollbackFixtureOn(t, d, base)
		ids := f.editSinceRelease()
		stale := f.rollbackPlan(rel.Seq).Fingerprint
		f.addClass("Journal") // moves the fingerprint
		now, version := f.fingerprintNow(), currentContentVersion(t, d, f.ctx)
		releases := len(listedReleases(t, d, f.ctx))

		for given, code := range map[string]string{"": planRequiredCode, stale: planStaleCode} {
			rep, err := d.ApplyOntologyRollback(f.ctx, rel.Seq, given)
			var pr *ChangePlanRefusal
			if !errors.As(err, &pr) || pr.Code != code || pr.Fingerprint != now || len(rep.Applied) != 0 {
				t.Errorf("apply with %q = %v (applied %v), want %s carrying %s", given, err, rep.Applied, code, now)
			}
		}
		if f.fingerprintNow() != now || currentContentVersion(t, d, f.ctx) != version || !f.exists(ids["added"]) ||
			len(listedReleases(t, d, f.ctx)) != releases {
			t.Error("a refused rollback changed the ontology or took a release")
		}
		if _, err := d.ApplyOntologyRollback(f.ctx, rel.Seq, now); err != nil {
			t.Errorf("apply with the current fingerprint: %v", err)
		}
	})
}

// TestRollback_EntryDeletedSinceIsAddedBackUnderANewID: a class deleted since the
// release comes back with its title, body and parent, under a new chunk id (owner
// ruling Q6: no writer keeps a removed chunk's id), so the report says the content
// version does not match and the diff to the release is that one entry, removed and
// added.
func TestRollback_EntryDeletedSinceIsAddedBackUnderANewID(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, base context.Context) {
		f, rel := rollbackFixtureOn(t, d, base)
		old := f.chunk("Provision")
		f.must(map[string]any{"op": "delete_chunk", "scope": "tenant", "id": old})

		rep, err := d.ApplyOntologyRollback(f.ctx, rel.Seq, f.rollbackPlan(rel.Seq).Fingerprint)
		if err != nil {
			t.Fatalf("apply: %v", err)
		}
		if len(rep.Applied) != 1 || rep.Applied[0].Op != "add" || rep.Applied[0].Entry != old || rep.Applied[0].NewID == "" || rep.Matches {
			t.Fatalf("report = %s, want one add of %s under a new id, not matching", diffJSON(rep), old)
		}
		back := rep.Applied[0].NewID
		if f.chunk("Provision") != back {
			t.Errorf("Provision is %s, want the chunk added back, %s", f.chunk("Provision"), back)
		}
		row, _, _ := d.getChunkRow(f.ctx, mustTenantKey(t, d, f.ctx), back)
		if row.ParentID != f.chunk("Classes") {
			t.Errorf("Provision came back under %s, want the Classes section", row.ParentID)
		}
		got := map[string]string{}
		for _, c := range f.diff(rel.Seq, 0).Changes {
			got[c.ChunkID] = c.Change
		}
		if len(got) != 2 || got[old] != connector.OntologyDiffRemoved || got[back] != connector.OntologyDiffAdded {
			t.Errorf("diff to the release = %v, want Provision removed (old id) and added (new id) only", got)
		}
	})
}
