package builtin

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/connector"
	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// planOf plans changes on the operator plane; an error fails the test.
func (f changeFixture) planOf(changes ...connector.OntologyChange) connector.PlanOntologyResult {
	f.t.Helper()
	res, err := f.d.PlanOntology(f.ctx, changes)
	if err != nil {
		f.t.Fatalf("PlanOntology(%+v): %v", changes, err)
	}
	return res
}

func proposalChange(id string) connector.OntologyChange {
	return connector.OntologyChange{Op: connector.OntologyPlanProposal, ID: id}
}

// withoutProposal is the plan with every change's proposal id cleared, for comparing a
// plan of proposals with the plan of the direct changes they stand for.
func withoutProposal(res connector.PlanOntologyResult) connector.PlanOntologyResult {
	out := res
	out.Changes = append([]connector.OntologyPlannedChange(nil), res.Changes...)
	for i := range out.Changes {
		out.Changes[i].Proposal = ""
	}
	return out
}

// TestOntologyPlan_ProposalPlansLikeTheDirectChange (RFC GQ P4.3): planning a retire
// proposal, a rename proposal and a move proposal by their ids gives exactly the plan
// of the direct change each stands for — the same risk, counts, notes, issues and
// fingerprint — with the proposal's id on the planned change.
func TestOntologyPlan_ProposalPlansLikeTheDirectChange(t *testing.T) {
	f := newAcceptFixture(t)
	reporting, supervises, memo, provision := f.chunk("Reporting"), f.chunk("supervises"), f.chunk("Memo"), f.chunk("Provision")
	for name, tc := range map[string]struct {
		proposal map[string]any
		direct   []connector.OntologyChange
	}{
		"retire": {map[string]any{"change": "retire", "entry": reporting},
			[]connector.OntologyChange{{Op: connector.OntologyPlanRetire, Entry: reporting}}},
		"rename": {map[string]any{"change": "edit", "entry": supervises, "name": "oversees"},
			[]connector.OntologyChange{{Op: connector.OntologyPlanRename, Entry: supervises, Name: "oversees"}}},
		"move": {map[string]any{"change": "edit", "entry": memo, "parent": provision},
			[]connector.OntologyChange{{Op: connector.OntologyPlanMove, Entry: memo, Parent: provision}}},
	} {
		id := f.file(tc.proposal)
		got := f.planOf(proposalChange(id))
		want := f.planOf(tc.direct...)
		if len(got.Changes) != len(tc.direct) {
			t.Fatalf("%s: the proposal plans as %d changes, want %d: %+v", name, len(got.Changes), len(tc.direct), got.Changes)
		}
		for _, ch := range got.Changes {
			if ch.Proposal != id {
				t.Errorf("%s: planned change %s carries proposal %q, want %q", name, ch.Op, ch.Proposal, id)
			}
		}
		if !reflect.DeepEqual(withoutProposal(got), want) {
			t.Errorf("%s: plan of the proposal\n %+v\nwant the plan of the direct change\n %+v", name, got, want)
		}
		if name == "retire" && got.Changes[0].Risk == connector.OntologyRiskNone {
			t.Errorf("retire: the placed document under the concept planned as risk none: %+v", got.Changes[0])
		}
		// Each proposal is planned against the ontology as filed; reject it so the next
		// one's filing is not refused as a second open proposal of the same entry.
		f.setStatus(f.ctx, id, memrank.OntologyStatusRejected)
	}
}

// TestOntologyPlan_SetOfProposalsIsPlannedTogether (RFC GQ P4.3): several proposals,
// mixed with a direct change, are planned as one request — one overlay, one fingerprint
// — exactly as the direct changes they stand for are, in the order given; an add
// proposal plans as risk none, naming the proposal.
func TestOntologyPlan_SetOfProposalsIsPlannedTogether(t *testing.T) {
	f := newAcceptFixture(t)
	reporting, supervises, provision := f.chunk("Reporting"), f.chunk("supervises"), f.chunk("Provision")
	retire := f.file(map[string]any{"change": "retire", "entry": reporting})
	rename := f.file(map[string]any{"change": "edit", "entry": supervises, "name": "oversees"})
	add := f.mustPropose(map[string]any{"name": "Circular", "parent": "Memo"})["chunk_id"].(string)

	got := f.planOf(proposalChange(retire), connector.OntologyChange{Op: connector.OntologyPlanRetire, Entry: provision},
		proposalChange(rename), proposalChange(add))
	want := f.planOf(connector.OntologyChange{Op: connector.OntologyPlanRetire, Entry: reporting},
		connector.OntologyChange{Op: connector.OntologyPlanRetire, Entry: provision},
		connector.OntologyChange{Op: connector.OntologyPlanRename, Entry: supervises, Name: "oversees"})
	if len(got.Changes) != 4 {
		t.Fatalf("plan has %d changes, want 4: %+v", len(got.Changes), got.Changes)
	}
	for i, wantID := range []string{retire, "", rename, add} {
		if got.Changes[i].Proposal != wantID {
			t.Errorf("changes[%d].proposal = %q, want %q", i, got.Changes[i].Proposal, wantID)
		}
	}
	a := got.Changes[3]
	if a.Op != connector.OntologyPlanAdd || a.Risk != connector.OntologyRiskNone || a.Entry.ChunkID != add ||
		a.Entry.Name != "Circular" || a.Entry.Kind != planKindClass || len(a.Counts) != 0 {
		t.Errorf("the add proposal planned as %+v", a)
	}
	got.Changes = got.Changes[:3]
	if !reflect.DeepEqual(withoutProposal(got), want) {
		t.Errorf("plan of the set\n %+v\nwant the plan of the direct changes together\n %+v", got, want)
	}
}

// TestOntologyPlan_NotAProposalIsRefused (RFC GQ P4.3): an id that is not an open
// proposal of the caller's tenant ontology — unknown, an entry in force, a rejected
// proposal, an accepted one, a chunk of another document, another tenant's open
// proposal — is refused entry_not_found with one message shape, so another tenant's
// proposal reads as unknown. A proposal whose mark no longer checks is refused
// invalid_request. A malformed proposal change is refused before anything is read.
func TestOntologyPlan_NotAProposalIsRefused(t *testing.T) {
	f := newAcceptFixture(t)
	reporting, supervises := f.chunk("Reporting"), f.chunk("supervises")

	rejected := f.file(map[string]any{"change": "retire", "entry": reporting})
	f.setStatus(f.ctx, rejected, memrank.OntologyStatusRejected)
	accepted := f.file(map[string]any{"change": "edit", "entry": supervises, "name": "oversees"})
	if _, err := f.accept(accepted, f.fingerprintNow()); err != nil {
		t.Fatalf("accept: %v", err)
	}
	other := f.must(map[string]any{"op": "create_document", "scope": "tenant", "title": "Not the ontology"})["root_chunk_id"].(string)

	// Another tenant's open proposal, on the same Document.
	base := tools.WithRunIdentity(tools.WithAgentName(context.Background(), "doc-agent"),
		tools.RunIdentityValue{AgentID: "a", UserID: "u1", TenantID: "other-tenant"})
	otherOp := installTenantOntology(t, f.d, base, retiredOntologyMD, memrank.OntologyConfirmedStatus)
	g := changeFixture{retiredFixture: &retiredFixture{t: t, d: f.d, ctx: operatorCtx(otherOp)}, agent: inRunCtx(base)}
	foreign := g.file(map[string]any{"change": "retire", "entry": g.chunk("Reporting")})
	if res := g.planOf(proposalChange(foreign)); len(res.Changes) != 1 {
		t.Fatalf("the other tenant cannot plan its own proposal: %+v", res)
	}

	for name, id := range map[string]string{
		"unknown": "no-such-chunk", "entry in force": f.chunk("Memo"), "rejected": rejected, "accepted": accepted,
		"chunk outside the ontology": other, "section": f.read().SectionChunks[memrank.SectionClasses],
		"another tenant's proposal": foreign,
	} {
		_, err := f.d.PlanOntology(f.ctx, []connector.OntologyChange{proposalChange(id)})
		if !errors.Is(err, connector.ErrOntologyPlanNotFound) || connector.OntologyPlanCode(err) != "entry_not_found" {
			t.Errorf("%s: err = %v, want entry_not_found", name, err)
			continue
		}
		if want := "changes[0]: \"" + id + "\" is not an open proposal of the tenant ontology"; err.Error() != want {
			t.Errorf("%s: message %q, want %q", name, err.Error(), want)
		}
	}

	// A mark edited after filing is not what the checks rebuild: refused, not planned.
	tampered := f.file(map[string]any{"change": "retire", "entry": f.chunk("Risk Management")})
	mark := f.markOf(tampered)
	mark["target_name"] = "Something Else"
	f.setMark(tampered, mark)
	_, err := f.d.PlanOntology(f.ctx, []connector.OntologyChange{proposalChange(tampered)})
	if !errors.Is(err, connector.ErrOntologyPlanInvalid) || !strings.Contains(err.Error(), "invalid_proposal") {
		t.Errorf("tampered mark: err = %v, want invalid_request naming invalid_proposal", err)
	}

	for name, ch := range map[string]connector.OntologyChange{
		"no id":          {Op: connector.OntologyPlanProposal},
		"id and entry":   {Op: connector.OntologyPlanProposal, ID: tampered, Entry: reporting},
		"id on a retire": {Op: connector.OntologyPlanRetire, Entry: reporting, ID: tampered},
	} {
		if _, err := f.d.PlanOntology(f.ctx, []connector.OntologyChange{ch}); !errors.Is(err, connector.ErrOntologyPlanInvalid) {
			t.Errorf("%s: err = %v, want ErrOntologyPlanInvalid", name, err)
		}
	}
}

// TestOntologyPlan_ProposalOnPostgres: the postgres twin — a proposal plans as the
// direct change on that tier, and a rejected one is refused.
func TestOntologyPlan_ProposalOnPostgres(t *testing.T) {
	d, base := pgDocumentOrSkip(t)
	f := changeFixture{retiredFixture: &retiredFixture{t: t, d: d,
		ctx: operatorCtx(installTenantOntology(t, d, base, retiredOntologyMD, memrank.OntologyConfirmedStatus))}, agent: inRunCtx(base)}
	seedFingerprintDependents(f.retiredFixture)
	reporting, supervises := f.chunk("Reporting"), f.chunk("supervises")
	retire := f.file(map[string]any{"change": "retire", "entry": reporting})
	rename := f.file(map[string]any{"change": "edit", "entry": supervises, "name": "oversees"})
	got := f.planOf(proposalChange(retire), proposalChange(rename))
	want := f.planOf(connector.OntologyChange{Op: connector.OntologyPlanRetire, Entry: reporting},
		connector.OntologyChange{Op: connector.OntologyPlanRename, Entry: supervises, Name: "oversees"})
	if !reflect.DeepEqual(withoutProposal(got), want) {
		t.Errorf("postgres: plan of the proposals\n %+v\nwant\n %+v", got, want)
	}
	f.setStatus(f.ctx, retire, memrank.OntologyStatusRejected)
	if _, err := d.PlanOntology(f.ctx, []connector.OntologyChange{proposalChange(retire)}); !errors.Is(err, connector.ErrOntologyPlanNotFound) {
		t.Errorf("postgres: a rejected proposal: err = %v, want ErrOntologyPlanNotFound", err)
	}
}
