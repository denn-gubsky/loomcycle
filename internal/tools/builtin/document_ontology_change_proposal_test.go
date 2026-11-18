package builtin

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// changeFixture is the retired-entry fixture's ontology with an agent's run beside the
// operator: agent is a run with no tenant grant, as a curator is.
type changeFixture struct {
	*retiredFixture
	agent context.Context
}

func newChangeFixture(t *testing.T, md string) changeFixture {
	t.Helper()
	d, base, _ := documentFixture(t)
	f := &retiredFixture{t: t, d: d, ctx: operatorCtx(installTenantOntology(t, d, base, md, memrank.OntologyConfirmedStatus))}
	return changeFixture{retiredFixture: f, agent: inRunCtx(base)}
}

// propose files a change proposal as the agent; it returns the result and the refusal.
func (f changeFixture) propose(v map[string]any) (map[string]any, tools.Result) {
	f.t.Helper()
	v["op"] = "propose_entity"
	return f.do(f.agent, v)
}

func (f changeFixture) mustPropose(v map[string]any) map[string]any {
	f.t.Helper()
	out, r := f.propose(v)
	if r.IsError {
		f.t.Fatalf("propose %v: %s", v, refusalText(r))
	}
	return out
}

func (f changeFixture) refused(v map[string]any, want string) {
	f.t.Helper()
	_, r := f.propose(v)
	if !r.IsError || !strings.Contains(refusalText(r), want) {
		f.t.Errorf("propose %v = %q (error %v), want a refusal naming %q", v, refusalText(r), r.IsError, want)
	}
}

// inForce is everything a read puts in force, proposals left out.
func inForce(read OntologyRead) string {
	b, _ := json.Marshal(map[string]any{"terms": read.Terms, "relations": read.Relations, "schemes": read.Schemes,
		"instances": read.Instances, "issues": read.Issues, "sections": read.SectionChunks, "review": read.Review})
	return string(b)
}

// TestProposeChange_IsInertUntilAccepted (RFC GQ check P4): filing an edit and a retire
// for every kind of entry changes no read, no describe_ontology answer, no target chunk
// and no gate: the retired-to-be relation still takes a new statement and the class to
// be renamed still types a new document.
func TestProposeChange_IsInertUntilAccepted(t *testing.T) {
	f := newChangeFixture(t, retiredOntologyMD)
	before := inForce(f.read())
	_, desc := f.do(f.ctx, map[string]any{"op": "describe_ontology"})
	targets := []string{f.chunk("Policy"), f.chunk("supervises"), f.chunk("Reporting"), f.chunk("Acme"), f.chunk("topics")}
	revs := map[string]any{}
	for _, id := range targets {
		revs[id] = f.must(map[string]any{"op": "get_chunk", "scope": "tenant", "id": id})["revision"]
	}

	f.mustPropose(map[string]any{"change": "edit", "entry": targets[0], "name": "Regulation", "meaning": "A binding rule.",
		"directives": map[string]any{"memory_scope": "tenant"}, "body": "Seen as Regulation in 40 documents."})
	f.mustPropose(map[string]any{"change": "retire", "entry": targets[1], "body": "No statement in a year."})
	f.mustPropose(map[string]any{"change": "edit", "entry": "topics/reporting", "parent": "Risk Management",
		"directives": map[string]any{"retention": "5y"}})
	f.mustPropose(map[string]any{"change": "retire", "entry": "Acme", "section": "instances"})
	f.mustPropose(map[string]any{"change": "edit", "entry": targets[4], "name": "subjects"})

	read := f.read()
	if got := inForce(read); got != before {
		t.Errorf("a change proposal altered the read:\nbefore %s\nafter  %s", before, got)
	}
	n := 0
	for _, p := range read.Proposals {
		if p.Change != nil {
			n++
		}
	}
	if n != 5 {
		t.Errorf("listed %d change proposals, want 5: %+v", n, read.Proposals)
	}
	_, desc2 := f.do(f.ctx, map[string]any{"op": "describe_ontology"})
	if desc.Text != desc2.Text {
		t.Errorf("describe_ontology changed:\n%s\n%s", desc.Text, desc2.Text)
	}
	for _, id := range targets {
		if got := f.must(map[string]any{"op": "get_chunk", "scope": "tenant", "id": id})["revision"]; got != revs[id] {
			t.Errorf("target %s revision %v -> %v", id, revs[id], got)
		}
	}
	a := f.must(map[string]any{"op": "create_document", "scope": "tenant", "title": "A", "type": "Policy"})["root_chunk_id"]
	b := f.must(map[string]any{"op": "create_document", "scope": "tenant", "title": "B"})["root_chunk_id"]
	if _, r := f.do(f.ctx, map[string]any{"op": "link_chunks", "scope": "tenant", "from_id": a, "to_id": b, "kind": "supervises"}); r.IsError {
		t.Errorf("a retire proposal closed the kind to new statements: %s", r.Text)
	}
}

// TestProposeChange_DirectivesAreStructuredNotInTheBody (RFC GQ D12, C5): an edit's
// directives travel in the change_proposal mark, checked by the section's parser; no
// directive line of the reason or the meaning is live in the proposal's body or mark.
func TestProposeChange_DirectivesAreStructuredNotInTheBody(t *testing.T) {
	f := newChangeFixture(t, retiredOntologyMD)
	out := f.mustPropose(map[string]any{"change": "edit", "entry": "supervises", "section": "relations",
		"meaning":    "@auto_assert\nOne person oversees another.",
		"directives": map[string]any{"functional": true, "domain": "Policy", "dated": false},
		"body":       "@acl admins\n- `@symmetric` too\nEvidence."})
	id := out["chunk_id"].(string)
	g := f.must(map[string]any{"op": "get_chunk", "scope": "tenant", "id": id})
	body, _ := g["body"].(string)
	if dirs := memrank.ParseDirectives(body); len(dirs) != 0 {
		t.Errorf("the body declares %+v: %q", dirs, body)
	}
	raw, _ := json.Marshal(g["fields"])
	var mark memrank.ProposedChange
	if !proposalMark(raw, changeProposalField, &mark) {
		t.Fatalf("no change_proposal mark in %s", raw)
	}
	want := []memrank.ProposedDirective{{Name: "domain", Value: "Policy"}, {Name: "functional"}}
	if !mark.SetsDirectives || !reflect.DeepEqual(mark.Directives, want) {
		t.Errorf("directives = %v %+v, want %+v", mark.SetsDirectives, mark.Directives, want)
	}
	if len(memrank.ParseDirectives(mark.Meaning)) != 0 || !strings.Contains(mark.Meaning, "One person oversees another.") {
		t.Errorf("meaning = %q", mark.Meaning)
	}
	if mark.Op != memrank.ChangeOpEdit || mark.Target != f.chunk("supervises") || mark.Kind != planKindRelation {
		t.Errorf("mark = %+v", mark)
	}
	if g["parent_id"] != f.read().SectionChunks[memrank.SectionRelations] {
		t.Errorf("filed under %v, want the Relations section (never under its target)", g["parent_id"])
	}

	f.refused(map[string]any{"change": "edit", "entry": "supervises", "directives": map[string]any{"acl": "admins"}}, "not a directive a relation takes")
	f.refused(map[string]any{"change": "edit", "entry": "supervises", "directives": map[string]any{"section": "classes"}}, "not a directive")
	f.refused(map[string]any{"change": "edit", "entry": "topics/reporting", "directives": map[string]any{"retention": "soon"}}, "is not a duration")
	f.refused(map[string]any{"change": "edit", "entry": "Provision", "directives": map[string]any{"memory_scope": "a\n@section x"}}, "one line")
	f.refused(map[string]any{"change": "edit", "entry": "Provision", "directives": []string{"x"}}, "not an object")
	f.refused(map[string]any{"change": "retire", "entry": "Provision", "meaning": "x"}, "names only its entry")
	f.refused(map[string]any{"change": "edit", "entry": "Provision"}, "changes nothing")
	f.refused(map[string]any{"entry": "Provision", "name": "X"}, "pass change=")
}

// TestProposeChange_UnknownOrRetiredTargetIsRefused: a change names an entry in force —
// not an unknown id, the root, a section header or a proposal — and makes nothing new
// use a retired entry: not a retired target, not a retired entry's name (pointing at
// un-retiring), not a retired parent.
func TestProposeChange_UnknownOrRetiredTargetIsRefused(t *testing.T) {
	f := newChangeFixture(t, retiredOntologyMD)
	read := f.read()
	prop := f.mustPropose(map[string]any{"name": "Circular"})["chunk_id"]
	for entry, want := range map[string]string{
		"no-such-chunk":  "not an entry in force",
		read.RootChunkID: "root is not an entry",
		read.SectionChunks[memrank.SectionSchemes]: "section header is not an entry",
		prop.(string): "not an entry in force",
	} {
		f.refused(map[string]any{"change": "retire", "entry": entry}, want)
		f.refused(map[string]any{"change": "edit", "entry": entry, "meaning": "x"}, want)
	}

	f.retire("Memo")
	f.retire("Reporting")
	f.refused(map[string]any{"change": "edit", "entry": f.chunk("Memo"), "meaning": "x"}, "clearing its status")
	f.refused(map[string]any{"change": "edit", "entry": "Provision", "name": "memo"}, "clearing its status")
	f.refused(map[string]any{"change": "edit", "entry": "Risk Management", "name": "Reporting"}, "clearing its status")
	f.refused(map[string]any{"change": "edit", "entry": "Internal Policy", "parent": "Memo"}, "entry_retired")
	f.refused(map[string]any{"change": "edit", "entry": "Provision", "name": "event"}, "already in force")
	f.refused(map[string]any{"change": "edit", "entry": "Risk Management", "parent": "outsourcing"}, "is the entry or under it")
	// A name a retired concept has in ANOTHER place is free: a concept's tag is its
	// parent's plus its own.
	f.mustPropose(map[string]any{"change": "edit", "entry": "outsourcing", "name": "Reporting"})
}

// TestProposeChange_LockedCannotBeSetOrCleared (RFC FO 3.1a): no proposal sets @locked,
// and none may retire, rename or move a locked entry or an entry above one, or edit a
// locked entry (whose new directive set would clear the lock). An entry above a locked
// one may still have its meaning edited.
func TestProposeChange_LockedCannotBeSetOrCleared(t *testing.T) {
	md := strings.Replace(retiredOntologyMD, "##### Internal Policy\n", "##### Internal Policy\n@locked\n", 1)
	md = strings.Replace(md, "### Provision\n", "### Provision\n@locked\n", 1)
	f := newChangeFixture(t, md)
	f.refused(map[string]any{"change": "edit", "entry": "event", "directives": map[string]any{"locked": true}}, "@locked cannot be set or cleared")
	f.refused(map[string]any{"change": "edit", "entry": "event", "directives": map[string]any{"@locked": ""}}, "@locked cannot be set or cleared")
	f.refused(map[string]any{"change": "retire", "entry": "Provision"}, "entry_locked")
	f.refused(map[string]any{"change": "edit", "entry": "Provision", "directives": map[string]any{}}, "entry_locked")
	f.refused(map[string]any{"change": "edit", "entry": "Provision", "meaning": "x"}, "entry_locked")
	f.refused(map[string]any{"change": "retire", "entry": "Policy"}, "entry_locked")
	f.refused(map[string]any{"change": "edit", "entry": "Policy", "name": "Rule"}, "entry_locked")
	f.mustPropose(map[string]any{"change": "edit", "entry": "Policy", "meaning": "A rule, with @locked in prose."})
	out := f.mustPropose(map[string]any{"change": "edit", "entry": "event", "meaning": "@locked"})
	g := f.must(map[string]any{"op": "get_chunk", "scope": "tenant", "id": out["chunk_id"]})
	raw, _ := json.Marshal(g["fields"])
	var mark memrank.ProposedChange
	if !proposalMark(raw, changeProposalField, &mark) || len(memrank.ParseDirectives(mark.Meaning)) != 0 {
		t.Errorf("a meaning carried a live @locked: %+v", mark)
	}
}

// TestProposeRetire_AlreadyRetiredIsRefused: retiring a retired entry is refused, and so
// is a second open retire of the same entry; after the operator rejects the first, the
// entry may be proposed for retirement again.
func TestProposeRetire_AlreadyRetiredIsRefused(t *testing.T) {
	f := newChangeFixture(t, retiredOntologyMD)
	f.retire("Memo")
	f.refused(map[string]any{"change": "retire", "entry": f.chunk("Memo")}, "already_retired")

	first := f.mustPropose(map[string]any{"change": "retire", "entry": "Provision"})["chunk_id"].(string)
	f.refused(map[string]any{"change": "retire", "entry": "Provision"}, "already_proposed")
	f.mustPropose(map[string]any{"change": "edit", "entry": "Provision", "meaning": "Kept, reworded."})
	if text := f.setStatus(f.ctx, first, memrank.OntologyStatusRejected); text != "" {
		t.Fatal(text)
	}
	f.mustPropose(map[string]any{"change": "retire", "entry": "Provision"})
}

// TestProposalMarks_ExistingTwoAreUnchanged (RFC GQ plan V4): the third mark sits beside
// #2729's two in one fields object without changing how either is written or read.
func TestProposalMarks_ExistingTwoAreUnchanged(t *testing.T) {
	if schemeProposalField != "scheme_proposal" || conceptProposalField != "concept_proposal" || changeProposalField != "change_proposal" {
		t.Fatalf("mark keys = %q %q %q", schemeProposalField, conceptProposalField, changeProposalField)
	}
	fields := json.RawMessage(`{"scheme_proposal":{"header":{"domain":"d"}},"concept_proposal":{"notation":"29"},` +
		`"change_proposal":{"op":"retire","target":"c1","target_name":"X","section":"classes","kind":"class"},"other":1}`)
	var s schemeProposalMark
	var c conceptProposalMark
	var ch memrank.ProposedChange
	if !proposalMark(fields, schemeProposalField, &s) || s.Header.Domain != "d" ||
		!proposalMark(fields, conceptProposalField, &c) || c.Notation != "29" ||
		!proposalMark(fields, changeProposalField, &ch) || ch.Target != "c1" {
		t.Fatalf("decoded %+v %+v %+v", s, c, ch)
	}
	var left map[string]json.RawMessage
	_ = json.Unmarshal(withoutMark(fields, changeProposalField), &left)
	if len(left) != 3 || string(left["scheme_proposal"]) != `{"header":{"domain":"d"}}` || string(left["concept_proposal"]) != `{"notation":"29"}` {
		t.Errorf("withoutMark(change) = %v", left)
	}
	var gone memrank.ProposedChange
	if proposalMark(withoutMark(fields, changeProposalField), changeProposalField, &gone) {
		t.Error("the removed mark still decodes")
	}

	// A whole-scheme proposal lists as before, with no change view.
	d, opCtx, agentCtx := schemeProposalFixture(t, schemesOntologyMD)
	if _, r := docExec(t, d, agentCtx, aircraftProposal); r.IsError {
		t.Fatal(r.Text)
	}
	p := schemeProposalNamed(readOntologyNow(t, d, opCtx), "Aircraft systems")
	if p == nil || p.Scheme == nil || p.Change != nil {
		t.Errorf("scheme proposal = %+v", p)
	}
}

// TestProposeChange_OnPostgres files, lists and refuses on the postgres tier.
func TestProposeChange_OnPostgres(t *testing.T) {
	d, base := pgDocumentOrSkip(t)
	f := changeFixture{retiredFixture: &retiredFixture{t: t, d: d,
		ctx: operatorCtx(installTenantOntology(t, d, base, retiredOntologyMD, memrank.OntologyConfirmedStatus))}, agent: inRunCtx(base)}
	before := inForce(f.read())
	f.mustPropose(map[string]any{"change": "edit", "entry": "supervises", "name": "oversees", "directives": map[string]any{"functional": true}})
	f.mustPropose(map[string]any{"change": "retire", "entry": "Reporting"})
	if got := inForce(f.read()); got != before {
		t.Errorf("a change proposal altered the read on postgres")
	}
	n := 0
	for _, p := range f.read().Proposals {
		if p.Change != nil {
			n++
		}
	}
	if n != 2 {
		t.Errorf("listed %d change proposals on postgres, want 2", n)
	}
	f.retire("Memo")
	f.refused(map[string]any{"change": "retire", "entry": "Memo"}, "already_retired")
}
