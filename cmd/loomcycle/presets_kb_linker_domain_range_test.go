package main

import (
	"context"
	"fmt"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// L6's domain and range check, against the real Document tool: the statements are
// filed through the runtime's own gate while their ends fit the kind, then an end
// is retyped, which no write gate looks at again. Only the linker's pass does.

// kbDomainOntologyMD: governs runs from a Policy (SecurityPolicy is one; a Memo is
// not) and is asserted without review when a rule files it; touches_on declares
// neither end.
const kbDomainOntologyMD = "# Tenant Ontology\n\n" +
	"## Classes\n@section classes\n\n### Document\n\n#### Policy\n\n##### SecurityPolicy\n\n#### Memo\n\n" +
	"## Relations\n@section relations\n\n### governs\n@domain Policy\n@auto_assert\n\n### touches_on\n@auto_assert\n"

// kbDomainCase is one Policy document with a clause, a target document, and the
// operator context a person's own link is written under.
type kbDomainCase struct {
	d      *builtin.Document
	ctx    context.Context
	op     context.Context
	policy map[string]any
	clause string
	target string
}

func newKBDomainCase(t *testing.T) *kbDomainCase {
	t.Helper()
	d, ctx := realDocument(t)
	ctx = withRealOntology(t, d, ctx, kbDomainOntologyMD)
	op := tools.WithCallerTier(context.Background(), tools.TierOperatorPlane)
	op = tools.WithRunIdentity(op, tools.RunIdentityValue{AgentID: "a", UserID: "u1", TenantID: "tnt"})
	op = tools.WithRunPolicy(op, tools.RunPolicy{
		Tools:  []string{"*"},
		Memory: tools.MemoryPolicyValue{AllowedScopes: []string{"tenant", "user"}},
		SqlMem: tools.SqlMemPolicyValue{AllowedScopes: []string{"tenant", "user"}},
	})
	c := &kbDomainCase{d: d, ctx: ctx, op: op}
	c.policy = realDoc(t, d, ctx, `{"op":"create_document","scope":"user","title":"Incident policy","type":"Policy"}`)
	c.clause = realDoc(t, d, ctx, fmt.Sprintf(`{"op":"create_chunk","scope":"user","document_id":%q,"title":"Clause 1","body":%q}`,
		c.policy["document_id"], kbBodyClaim))["id"].(string)
	other := realDoc(t, d, ctx, `{"op":"create_document","scope":"user","title":"Operations"}`)
	c.target = realDoc(t, d, ctx, fmt.Sprintf(`{"op":"create_chunk","scope":"user","document_id":%q,"title":"Article 1","body":%q}`,
		other["document_id"], kbBodyLaw))["id"].(string)
	return c
}

// ruleStatement files kind from the policy document to the target as a rule would,
// and requires the runtime to have asserted it.
func (c *kbDomainCase) ruleStatement(t *testing.T, kind string) string {
	t.Helper()
	id := realDoc(t, c.d, c.ctx, fmt.Sprintf(
		`{"op":"propose_statement","scope":"user","from_id":%q,"to_id":%q,"kind":%q,"method":"rule:governance","evidence":[{"chunk_id":%q,"quote":"report a material incident"}]}`,
		c.policy["root_chunk_id"], c.target, kind, c.clause))["id"].(string)
	if s := c.edge(t, id); s["status"] != "asserted" {
		t.Fatalf("fixture: the rule's statement is %v, want asserted", s["status"])
	}
	return id
}

// retype sets the policy document's class, as an operator re-classifying it would.
func (c *kbDomainCase) retype(t *testing.T, class string) {
	t.Helper()
	root := realDoc(t, c.d, c.op, fmt.Sprintf(`{"op":"get_chunk","scope":"user","id":%q}`, c.policy["root_chunk_id"]))
	realDoc(t, c.d, c.op, fmt.Sprintf(`{"op":"update_chunk","scope":"user","id":%q,"revision":%v,"type":%q}`,
		c.policy["root_chunk_id"], root["revision"], class))
}

func (c *kbDomainCase) edge(t *testing.T, id string) map[string]any {
	t.Helper()
	s, _ := realDoc(t, c.d, c.ctx, fmt.Sprintf(`{"op":"get_edge","scope":"user","id":%q}`, id))["statement"].(map[string]any)
	return s
}

func (c *kbDomainCase) maintain(t *testing.T) map[string]any {
	t.Helper()
	rep := runRealLinker(t, c.d, c.ctx, fmt.Sprintf(`{"mode":"maintain","document_id":%q,"write":"propose"}`, c.policy["document_id"]))
	if refusals, _ := rep["refusals"].(map[string]any); len(refusals) != 0 {
		t.Errorf("refusals = %v, want none from the real ops", refusals)
	}
	return rep
}

func wantDomainRange(t *testing.T, rep map[string]any, want map[string]float64) {
	t.Helper()
	for _, name := range []string{"checked", "mismatched", "flagged", "demoted", "unchecked"} {
		if n := layerCount(t, rep, "L6", "checks.domain_range."+name); n != want[name] {
			t.Errorf("checks.domain_range.%s = %v, want %v", name, n, want[name])
		}
	}
}

// TestKBLinker_MaintainDemotesAStatementWhoseEndLeftTheDomain: a rule's asserted
// governs statement, whose source document is re-classified from Policy to Memo,
// goes back to review. Its minimal pair is TestKBLinker_SubclassEndPasses.
func TestKBLinker_MaintainDemotesAStatementWhoseEndLeftTheDomain(t *testing.T) {
	c := newKBDomainCase(t)
	id := c.ruleStatement(t, "governs")
	c.retype(t, "Memo")

	rep := c.maintain(t)
	s := c.edge(t, id)
	if s["status"] != "proposed" || s["expired_at"] != nil {
		t.Errorf("statement = %v, want demoted to proposed and still live", s)
	}
	if s["verified_at"] != nil {
		t.Errorf("statement = %v, want it not verified", s)
	}
	wantDomainRange(t, rep, map[string]float64{"checked": 1, "mismatched": 1, "demoted": 1})
}

// TestKBLinker_SubclassEndPasses: the same statement, with the source re-classified
// to a subclass of the domain, is inside it and is verified untouched.
func TestKBLinker_SubclassEndPasses(t *testing.T) {
	c := newKBDomainCase(t)
	id := c.ruleStatement(t, "governs")
	c.retype(t, "SecurityPolicy")

	rep := c.maintain(t)
	s := c.edge(t, id)
	if s["status"] != "asserted" || s["verified_at"] == nil {
		t.Errorf("statement = %v, want still asserted, and verified", s)
	}
	if attrs, _ := s["attributes"].(map[string]any); attrs["needs_review"] == true {
		t.Errorf("statement = %v, want it not flagged", s)
	}
	wantDomainRange(t, rep, map[string]float64{"checked": 1})
}

// TestKBLinker_PersonsStatementIsFlaggedNotDemoted: the same mismatch on a link a
// person made is theirs to settle. It is flagged and stays asserted.
func TestKBLinker_PersonsStatementIsFlaggedNotDemoted(t *testing.T) {
	c := newKBDomainCase(t)
	id := realDoc(t, c.d, c.op, fmt.Sprintf(
		`{"op":"link_chunks","scope":"user","from_id":%q,"to_id":%q,"kind":"governs","evidence":[{"chunk_id":%q,"quote":"report a material incident"}]}`,
		c.policy["root_chunk_id"], c.target, c.clause))["id"].(string)
	if s := c.edge(t, id); s["status"] != "asserted" || s["method"] != "manual" {
		t.Fatalf("fixture: the person's link is %v by %v, want asserted by manual", s["status"], s["method"])
	}
	c.retype(t, "Memo")

	rep := c.maintain(t)
	s := c.edge(t, id)
	if s["status"] != "asserted" || s["expired_at"] != nil {
		t.Errorf("statement = %v, want still asserted", s)
	}
	if attrs, _ := s["attributes"].(map[string]any); attrs["needs_review"] != true {
		t.Errorf("statement = %v, want flagged for review", s)
	}
	wantDomainRange(t, rep, map[string]float64{"checked": 1, "mismatched": 1, "flagged": 1})
}

// TestKBLinker_KindWithNoDomainIsUnchecked: touches_on declares neither end, so a
// Memo's statement of that kind is counted unchecked and verified; no class is read.
func TestKBLinker_KindWithNoDomainIsUnchecked(t *testing.T) {
	c := newKBDomainCase(t)
	id := c.ruleStatement(t, "touches_on")
	c.retype(t, "Memo")

	rep := c.maintain(t)
	s := c.edge(t, id)
	if s["status"] != "asserted" || s["verified_at"] == nil {
		t.Errorf("statement = %v, want still asserted, and verified", s)
	}
	wantDomainRange(t, rep, map[string]float64{"unchecked": 1})
}

// TestKBLinker_EndOfAnUndeclaredClassIsUncheckedNotDemoted: a class the tree does
// not hold cannot be placed inside or outside the domain — it may have been
// renamed — so the statement is left alone.
func TestKBLinker_EndOfAnUndeclaredClassIsUncheckedNotDemoted(t *testing.T) {
	c := newKBDomainCase(t)
	id := c.ruleStatement(t, "governs")
	c.retype(t, "Circular")

	rep := c.maintain(t)
	s := c.edge(t, id)
	if s["status"] != "asserted" || s["verified_at"] == nil {
		t.Errorf("statement = %v, want still asserted, and verified", s)
	}
	wantDomainRange(t, rep, map[string]float64{"unchecked": 1})
}
