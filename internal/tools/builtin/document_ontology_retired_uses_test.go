package builtin

import (
	"errors"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/connector"
	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// The new uses of a retired entry the RFC GQ P2.1 review found unguarded: accepting a
// proposal filed before the retirement, a statement to a retired class or instance,
// and the revision of a statement that already used the entry (not a new use).

func (f *retiredFixture) edgeStatus(id string) string {
	f.t.Helper()
	out := f.must(map[string]any{"op": "get_edge", "scope": "tenant", "id": id})
	s, _ := out["statement"].(map[string]any)["status"].(string)
	return s
}

func (f *retiredFixture) root(title string) string {
	f.t.Helper()
	return f.must(map[string]any{"op": "create_document", "scope": "tenant", "title": title})["root_chunk_id"].(string)
}

// TestAcceptStatement_ProposalOfARetiredEntryIsRefused: a proposal filed before its kind
// or its concept was retired is not put in force — by accept_statement, by a bulk
// ruling, or by an authored link_chunks over it. Rejecting it stays allowed.
func TestAcceptStatement_ProposalOfARetiredEntryIsRefused(t *testing.T) {
	f := newRetiredFixture(t)
	a, b := f.root("A"), f.root("B")
	reporting := conceptOfTag(f.read(), "topics/reporting").ChunkID
	propose := func(from, to, kind string) string {
		return f.must(map[string]any{"op": "propose_statement", "scope": "tenant", "from_id": from, "to_id": to, "kind": kind})["id"].(string)
	}
	byKind := propose(a, b, "supervises")
	byConcept := propose(a, reporting, "filed_under")
	bulk := propose(b, a, "supervises")
	f.retire("supervises")
	f.retire("Reporting")

	for _, id := range []string{byKind, byConcept} {
		if _, r := f.do(f.ctx, map[string]any{"op": "accept_statement", "scope": "tenant", "id": id}); !r.IsError ||
			!strings.Contains(r.Text, "entry_retired") {
			t.Errorf("accept_statement %s: %q, want entry_retired", id, r.Text)
		}
	}
	if _, r := f.do(f.ctx, map[string]any{"op": "link_chunks", "scope": "tenant", "from_id": a, "to_id": b, "kind": "supervises"}); !r.IsError ||
		!strings.Contains(r.Text, "entry_retired") {
		t.Errorf("link_chunks over the proposal: %q, want entry_retired", r.Text)
	}
	op := reviewOperatorCtx(f.ctx, nil)
	res, err := f.d.RuleStatements(op, "tenant", connector.RuleStatementsRequest{Action: "accept", IDs: []string{bulk},
		DryRun: boolPtr(false), ExpectCount: intPtr(1)}, RuleAsOperator)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Items) != 1 || res.Items[0].Error != rulingEntryRetired {
		t.Errorf("bulk accept = %+v, want the proposal refused with %s", res, rulingEntryRetired)
	}
	for _, id := range []string{byKind, byConcept, bulk} {
		if s := f.edgeStatus(id); s != edgeStatusProposed {
			t.Errorf("%s went %s, want it still proposed", id, s)
		}
	}
	if _, r := f.do(f.ctx, map[string]any{"op": "reject_statement", "scope": "tenant", "id": byConcept, "reason": "retired"}); r.IsError {
		t.Errorf("rejecting a proposal of a retired entry: %s", r.Text)
	}
}

// TestProposeStatement_RevisionOfAStatementOfARetiredKindIsAllowed: a revision replaces
// the statement it revises, so correcting one that already used a retired kind is not a
// new use; a revision that changes the kind to a retired one is.
func TestProposeStatement_RevisionOfAStatementOfARetiredKindIsAllowed(t *testing.T) {
	f := newRetiredFixture(t)
	a, b := f.root("A"), f.root("B")
	id := f.must(map[string]any{"op": "link_chunks", "scope": "tenant", "from_id": a, "to_id": b, "kind": "supervises"})["id"].(string)
	filed := f.must(map[string]any{"op": "link_chunks", "scope": "tenant", "from_id": a,
		"to_id": conceptOfTag(f.read(), "topics/reporting").ChunkID, "kind": "filed_under"})["id"].(string)
	f.retire("supervises")

	rev, r := f.do(f.ctx, map[string]any{"op": "propose_statement", "scope": "tenant", "id": id, "confidence": 0.5})
	if r.IsError {
		t.Fatalf("a revision of a statement of a retired kind: %s", r.Text)
	}
	if _, r := f.do(f.ctx, map[string]any{"op": "accept_statement", "scope": "tenant", "id": rev["id"]}); r.IsError {
		t.Errorf("accepting the revision: %s", r.Text)
	}
	if _, r := f.do(f.ctx, map[string]any{"op": "propose_statement", "scope": "tenant", "id": filed, "kind": "supervises"}); !r.IsError ||
		!strings.Contains(r.Text, "entry_retired") {
		t.Errorf("a revision changing the kind to a retired one: %q, want entry_retired", r.Text)
	}
}

// TestRelationGate_NewStatementToARetiredClassOrInstanceIsRefused: a new statement to
// a retired class's chunk, or to the subject a retired instance was adopted as, is a
// new use of it; one that already holds is written to as before.
func TestRelationGate_NewStatementToARetiredClassOrInstanceIsRefused(t *testing.T) {
	f := newRetiredFixture(t)
	if _, err := f.d.AdoptInstances(f.ctx, false); err != nil {
		t.Fatal(err)
	}
	acme, _ := f.d.chunkIDByNaturalKey(f.ctx, mustTenantKey(t, f.d, f.ctx), "organization:acme")
	if acme == "" {
		t.Fatal("Acme was not adopted")
	}
	a, b := f.root("A"), f.root("B")
	memo := f.chunk("Memo")
	kept := f.must(map[string]any{"op": "link_chunks", "scope": "tenant", "from_id": b, "to_id": acme, "kind": "supervises"})["id"]
	f.retire("Memo")
	f.retire("Acme")
	for _, in := range []map[string]any{
		{"from_id": a, "to_id": memo, "kind": "instance_of"},
		{"from_id": a, "to_id": acme, "kind": "supervises"},
		{"from_id": acme, "to_id": a, "kind": "supervises"},
	} {
		in["op"], in["scope"] = "link_chunks", "tenant"
		if _, r := f.do(f.ctx, in); !r.IsError || !strings.Contains(r.Text, "entry_retired") {
			t.Errorf("%v: %q, want entry_retired", in, r.Text)
		}
	}
	again := f.must(map[string]any{"op": "link_chunks", "scope": "tenant", "from_id": b, "to_id": acme, "kind": "supervises"})
	if again["id"] != kept {
		t.Errorf("re-writing the existing statement gave %v, want %v", again["id"], kept)
	}
}

// TestOntologyConfirm_NoNewInstanceOfToARetiredClass: re-adopting an instance whose
// class was retired writes no new instance_of to it, and reports no error for it.
func TestOntologyConfirm_NoNewInstanceOfToARetiredClass(t *testing.T) {
	md := retiredOntologyMD + "\n### Clause 1\n@a Provision\n"
	d, base, _ := documentFixture(t)
	f := &retiredFixture{t: t, d: d, ctx: operatorCtx(installTenantOntology(t, d, base, md, memrank.OntologyConfirmedStatus))}
	first, err := f.d.AdoptInstances(f.ctx, false)
	if err != nil || len(first.Errors) != 0 {
		t.Fatalf("first adoption = %+v, %v", first, err)
	}
	var clauseKey string
	for _, st := range first.Instances {
		if st.Name == "Clause 1" {
			clauseKey = st.Key
		}
	}
	clause, _ := f.d.chunkIDByNaturalKey(f.ctx, mustTenantKey(t, f.d, f.ctx), clauseKey)
	if clause == "" {
		t.Fatalf("Clause 1 was not adopted: %+v", first)
	}
	provision := f.chunk("Provision")
	f.must(map[string]any{"op": "unlink_chunks", "scope": "tenant", "from_id": clause, "to_id": provision, "kind": "instance_of"})
	f.retire("Provision")
	rep, err := f.d.AdoptInstances(f.ctx, false)
	if err != nil || len(rep.Errors) != 0 {
		t.Errorf("re-adoption = %+v, %v; want no errors", rep, err)
	}
	docID := f.must(map[string]any{"op": "get_chunk", "scope": "tenant", "id": clause})["document_id"].(string)
	edges := edgesOf(t, f.d, f.ctx, `{"op":"get_edges","scope":"tenant","document_id":"`+docID+`","kind":"instance_of"}`)
	for _, e := range edges {
		if e["to_id"] == provision && e["expired_at"] == nil {
			t.Errorf("a new instance_of to the retired class was written: %v", e)
		}
	}
}

// TestOntologyGuard_AgentProposalUnderOrNamedAsARetiredEntryIsRefused: an agent's raw
// create_chunk/upsert_chunk proposal is held to propose_entity's rules — nothing filed
// under a retired entry, and no proposal of a retired entry's name.
func TestOntologyGuard_AgentProposalUnderOrNamedAsARetiredEntryIsRefused(t *testing.T) {
	f := newRetiredFixture(t)
	docID := f.read().DocumentID
	classes := f.read().SectionChunks[memrank.SectionClasses]
	memo, outsourcing := f.chunk("Memo"), f.chunk("outsourcing")
	f.retire("Memo")
	f.retire("Risk Management")
	agent := tools.WithCallerTier(inRunCtx(f.ctx), tools.TierMember)
	for _, in := range []map[string]any{
		{"op": "create_chunk", "parent_id": memo, "title": "Board Memo"},
		{"op": "create_chunk", "parent_id": outsourcing, "title": "cloud"},
		{"op": "create_chunk", "parent_id": classes, "title": "Memo"},
		{"op": "upsert_chunk", "natural_key": "proposal:board-memo", "parent_id": memo, "title": "Board Memo", "body": "x"},
	} {
		in["scope"], in["document_id"], in["status"] = "tenant", docID, memrank.OntologyStatusProposed
		if _, r := f.do(agent, in); !r.IsError || !strings.Contains(r.Text, "retired") {
			t.Errorf("%v: %q, want a retired refusal", in, r.Text)
		}
	}
	if _, r := f.do(agent, map[string]any{"op": "create_chunk", "scope": "tenant", "document_id": docID,
		"parent_id": classes, "title": "Note", "status": memrank.OntologyStatusProposed}); r.IsError {
		t.Errorf("a proposal under a live section: %s", r.Text)
	}
}

// TestProposeStatement_RevisionOfAStatementNotInForceIsCheckedInFull: only a statement
// in force is replaced by its revision, so only its revision shares its uses; a
// revision of a proposed or a (policy-)rejected statement of a retired kind is a new
// use, refused as a new statement would be.
func TestProposeStatement_RevisionOfAStatementNotInForceIsCheckedInFull(t *testing.T) {
	f := newRetiredFixture(t)
	a, b, c := f.root("A"), f.root("B"), f.root("C")
	asserted := f.must(map[string]any{"op": "link_chunks", "scope": "tenant", "from_id": a, "to_id": b, "kind": "supervises"})["id"].(string)
	proposed := f.must(map[string]any{"op": "propose_statement", "scope": "tenant", "from_id": b, "to_id": c, "kind": "supervises"})["id"].(string)
	rejected := f.must(map[string]any{"op": "propose_statement", "scope": "tenant", "from_id": c, "to_id": a, "kind": "supervises"})["id"].(string)
	// A review policy's rejection is no tombstone, so its statement may be revised.
	if err := f.d.exec(f.ctx, mustTenantKey(t, f.d, f.ctx), `UPDATE chunk_edges SET status = ?, ruled_by = ? WHERE id = ?`,
		edgeStatusRejected, "policy:1@1", rejected); err != nil {
		t.Fatal(err)
	}
	f.retire("supervises")
	for _, id := range []string{proposed, rejected} {
		if _, r := f.do(f.ctx, map[string]any{"op": "propose_statement", "scope": "tenant", "id": id, "confidence": 0.5}); !r.IsError ||
			!strings.Contains(r.Text, "entry_retired") {
			t.Errorf("a revision of %s: %q, want entry_retired", f.edgeStatus(id), r.Text)
		}
	}
	if _, r := f.do(f.ctx, map[string]any{"op": "propose_statement", "scope": "tenant", "id": asserted, "confidence": 0.5}); r.IsError {
		t.Errorf("a revision of the asserted statement: %s", r.Text)
	}
}

// TestProposeSubject_RetiredEntityTypeIsRefused: a subject of a retired class is not
// filed, and a proposal filed before the class was retired is not adopted.
func TestProposeSubject_RetiredEntityTypeIsRefused(t *testing.T) {
	f := newRetiredFixture(t)
	f.must(map[string]any{"op": "propose_subject", "subject": "Launch", "natural_key": "event:launch"})
	f.retire("event")
	if _, r := f.do(f.ctx, map[string]any{"op": "propose_subject", "subject": "Offsite", "natural_key": "event:offsite"}); !r.IsError ||
		!strings.Contains(r.Text, "entry_retired") {
		t.Errorf("propose_subject of a retired class: %q, want entry_retired", r.Text)
	}
	_, _, err := f.d.AdoptSubjectProposalInScope(f.ctx, "tenant",
		SubjectProposalFields{Subject: "Launch", EntityType: "event", NaturalKey: "event:launch"}, "")
	if !errors.Is(err, ErrEntryRetired) {
		t.Errorf("adopting a subject of a retired class: %v, want ErrEntryRetired", err)
	}
	if id, _ := f.d.chunkIDByNaturalKey(f.ctx, mustTenantKey(t, f.d, f.ctx), "event:launch"); id != "" {
		t.Error("a subject of the retired class was minted")
	}
}

// TestRemember_RetiredClassIsRefused: a remembered fact is a new row, so it may not take
// a retired class as its type; remembering the same fact again, already of that type,
// is not a new use.
func TestRemember_RetiredClassIsRefused(t *testing.T) {
	f := newRetiredFixture(t)
	f.must(map[string]any{"op": "remember", "scope": "user", "text": "The launch slipped a week.", "type": "event"})
	f.retire("event")
	if _, r := f.do(f.ctx, map[string]any{"op": "remember", "scope": "user", "text": "The offsite moved to May.", "type": "event"}); !r.IsError ||
		!strings.Contains(r.Text, "entry_retired") {
		t.Errorf("remember of a retired class: %q, want entry_retired", r.Text)
	}
	if _, r := f.do(f.ctx, map[string]any{"op": "remember", "scope": "user", "text": "The launch slipped a week.", "type": "event"}); r.IsError {
		t.Errorf("remembering an existing fact of the retired class again: %s", r.Text)
	}
}

// TestTags_RetiredConceptTagIsRefusedOutsideTheTenantScope: in a user scope a concept's
// members are found by its mirror tag, so a NEW tag equal to a retired concept's, or
// under it, is a new placement and is refused on every tag write; a tag the target
// already carries is kept, and a live concept's tag is written.
func TestTags_RetiredConceptTagIsRefusedOutsideTheTenantScope(t *testing.T) {
	f := newRetiredFixture(t)
	doc := f.must(map[string]any{"op": "create_document", "scope": "user", "title": "Notes", "tags": []any{"topics/risk-management"}})
	chunk := f.must(map[string]any{"op": "create_chunk", "scope": "user", "document_id": doc["document_id"], "title": "c",
		"tags": []any{"topics/risk-management/outsourcing"}})["id"].(string)
	f.retire("Risk Management")
	for _, in := range []map[string]any{
		{"op": "create_document", "title": "More", "tags": []any{"topics/risk-management"}},
		{"op": "create_chunk", "document_id": doc["document_id"], "title": "d", "tags": []any{"topics/risk-management/outsourcing/cloud"}},
		{"op": "upsert_chunk", "document_id": doc["document_id"], "natural_key": "note:e", "title": "e", "body": "e", "tags": []any{"topics/risk-management"}},
		{"op": "add_tags", "id": chunk, "tags": []any{"topics/risk-management"}},
		{"op": "add_tags", "document_id": doc["document_id"], "tags": []any{"topics/risk-management/outsourcing"}},
	} {
		in["scope"] = "user"
		if _, r := f.do(f.ctx, in); !r.IsError || !strings.Contains(r.Text, "entry_retired") {
			t.Errorf("%v: %q, want entry_retired", in, r.Text)
		}
	}
	rev := int(f.must(map[string]any{"op": "get_chunk", "scope": "user", "id": chunk})["revision"].(float64))
	if _, r := f.do(f.ctx, map[string]any{"op": "update_chunk", "scope": "user", "id": chunk, "revision": rev,
		"tags": []any{"topics/risk-management"}}); !r.IsError || !strings.Contains(r.Text, "entry_retired") {
		t.Errorf("update_chunk adding a retired concept's tag: %q, want entry_retired", r.Text)
	}
	if _, r := f.do(f.ctx, map[string]any{"op": "update_chunk", "scope": "user", "id": chunk, "revision": rev,
		"tags": []any{"topics/risk-management/outsourcing", "topics/reporting"}}); r.IsError {
		t.Errorf("update_chunk keeping its tag and adding a live one: %s", r.Text)
	}
}
