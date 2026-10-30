package builtin

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// statementMethodRun is ctx inside a run of a code-js agent holding methods.
func statementMethodRun(ctx context.Context, methods ...string) context.Context {
	ctx = tools.WithRunID(ctx, "run-linker")
	return tools.UpdateRunPolicy(ctx, func(rp *tools.RunPolicy) { rp.StatementMethods = methods })
}

func statementMethodOf(t *testing.T, d *Document, ctx context.Context, id string) string {
	t.Helper()
	out, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"get_edge","scope":"user","id":%q}`, id))
	if r.IsError {
		t.Fatalf("get_edge: %s", r.Text)
	}
	m, _ := out["statement"].(map[string]any)["method"].(string)
	return m
}

// A run holding the grant names its method per write, and the statement carries
// it; without `method` the stamp is manual, as it always was.
func TestProposeStatement_StampsTheGrantedMethod(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, ctx context.Context) {
		_, a, b, c := stmtDoc(t, d, ctx, "Licence")
		run := statementMethodRun(ctx, "rule", "extractor")
		out, r := docExec(t, d, run, fmt.Sprintf(`{"op":"propose_statement","scope":"user","from_id":%q,"to_id":%q,"kind":"cites","method":"rule:l2/citation.v1"}`, a, b))
		if r.IsError {
			t.Fatalf("propose with a granted rule: %s", r.Text)
		}
		if got := statementMethodOf(t, d, ctx, out["id"].(string)); got != "rule:l2/citation.v1" {
			t.Errorf("method = %q, want rule:l2/citation.v1", got)
		}
		out, r = docExec(t, d, run, fmt.Sprintf(`{"op":"propose_statement","scope":"user","from_id":%q,"to_id":%q,"kind":"based_on","method":"extractor:kb_verdict"}`, a, c))
		if r.IsError {
			t.Fatalf("propose with a granted extractor: %s", r.Text)
		}
		if got := statementMethodOf(t, d, ctx, out["id"].(string)); got != "extractor:kb_verdict" {
			t.Errorf("method = %q, want extractor:kb_verdict", got)
		}
		out, r = docExec(t, d, run, fmt.Sprintf(`{"op":"propose_statement","scope":"user","from_id":%q,"to_id":%q,"kind":"related"}`, b, c))
		if r.IsError {
			t.Fatalf("propose without a method: %s", r.Text)
		}
		if got := statementMethodOf(t, d, ctx, out["id"].(string)); got != "manual" {
			t.Errorf("method = %q, want manual", got)
		}
	})
}

// A method is refused, naming method_not_granted, when the caller's grant lacks
// its prefix, when the name is malformed, and from a caller holding no grant —
// a model's run, or the operator plane. Nothing is written.
func TestProposeStatement_RefusesAMethodNotGranted(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, ctx context.Context) {
		_, a, b, _ := stmtDoc(t, d, ctx, "Licence")
		ruleOnly := statementMethodRun(ctx, "rule")
		long := "rule:" + strings.Repeat("a", 65)
		for name, tc := range map[string]struct {
			ctx    context.Context
			method string
		}{
			"prefix not held":     {ruleOnly, "extractor:x"},
			"no grant in a run":   {tools.WithRunID(ctx, "run-model"), "rule:x"},
			"operator plane":      {operatorPlane(ctx), "rule:x"},
			"bare prefix":         {ruleOnly, "rule"},
			"empty name":          {ruleOnly, "rule:"},
			"upper case":          {ruleOnly, "rule:L2"},
			"name too long":       {ruleOnly, long},
			"server method":       {ruleOnly, "manual"},
			"unknown prefix":      {ruleOnly, "linker:x"},
			"space in name":       {ruleOnly, "rule:a b"},
			"prefix only granted": {ruleOnly, "extractor:"},
		} {
			_, r := docExec(t, d, tc.ctx, fmt.Sprintf(`{"op":"propose_statement","scope":"user","from_id":%q,"to_id":%q,"kind":"cites","method":%q}`, a, b, tc.method))
			if !r.IsError || !strings.Contains(r.Text, "method_not_granted") {
				t.Errorf("%s: method %q was not refused with method_not_granted: %s", name, tc.method, r.Text)
			}
		}
		key, _, _ := d.resolveScope(ctx, "user")
		res, err := d.query(ctx, key, `SELECT COUNT(*) FROM chunk_edges WHERE kind = 'cites'`)
		if err != nil || asInt(res.Rows[0][0]) != 0 {
			t.Errorf("a refused write left a statement behind: %v %v", res, err)
		}
	})
}

// `method` belongs to the statement-writing ops; on any other op it is refused
// rather than read, so link_chunks cannot be made to write a rule: assertion.
func TestDocumentMethod_RefusedOnAnOpThatStampsNothing(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	_, a, b, _ := stmtDoc(t, d, ctx, "Licence")
	run := statementMethodRun(ctx, "rule")
	_, r := docExec(t, d, run, fmt.Sprintf(`{"op":"link_chunks","scope":"user","from_id":%q,"to_id":%q,"kind":"cites","method":"rule:x"}`, a, b))
	if !r.IsError || !strings.Contains(r.Text, "method") {
		t.Fatalf("link_chunks with a method was not refused: %s", r.Text)
	}
}

// A rule: write of a kind the vocabulary marks @auto_assert, with every quote in
// a chunk, is written asserted; the same write without a rule stays proposed.
// Over a triple that already holds, an auto-asserting rule adds its evidence and
// answers asserted, while a kind that does not auto-assert is refused with
// statement_asserted — a proposal may not add to a relation that holds.
func TestProposeStatement_RuleAutoAssertsADeclaredKind(t *testing.T) {
	f := newRelFixture(t, relationsOntologyMD)
	run := statementMethodRun(f.ctx, "rule", "extractor")
	ev := fmt.Sprintf(`"evidence":[{"chunk_id":%q,"quote":"x"}]`, f.provA)

	// Fresh triple: asserted by a rule, proposed otherwise.
	out, r := docExec(t, f.d, run, fmt.Sprintf(`{"op":"propose_statement","scope":"user","from_id":%q,"to_id":%q,"kind":"supervises","method":"rule:l2",%s}`, f.provA, f.provB, ev))
	if r.IsError || out["status"] != "asserted" {
		t.Fatalf("a rule write of an auto-assert kind = %v %s", out, r.Text)
	}
	out, r = docExec(t, f.d, run, fmt.Sprintf(`{"op":"propose_statement","scope":"user","from_id":%q,"to_id":%q,"kind":"supervises","method":"extractor:m",%s}`, f.provB, f.provA, ev))
	if r.IsError || out["status"] != "proposed" {
		t.Fatalf("an extractor write of an auto-assert kind = %v %s", out, r.Text)
	}

	// Over an asserted triple of an auto-assert kind: evidence added, asserted.
	if _, refused, text := f.link(t, "supervises", f.untyped, f.provB, ""); refused {
		t.Fatalf("link: %s", text)
	}
	out, r = docExec(t, f.d, run, fmt.Sprintf(`{"op":"propose_statement","scope":"user","from_id":%q,"to_id":%q,"kind":"supervises","method":"rule:l2",%s}`, f.untyped, f.provB, ev))
	if r.IsError {
		t.Fatalf("a rule write over an asserted auto-assert triple was refused: %s", r.Text)
	}
	if out["status"] != "asserted" || out["created"] != false || out["evidence_written"] != float64(1) {
		t.Errorf("a rule write over an asserted auto-assert triple = %v", out)
	}

	// Over an asserted triple of a kind that does not auto-assert: refused.
	if _, refused, text := f.link(t, "cites", f.provA, f.provB, ""); refused {
		t.Fatalf("link cites: %s", text)
	}
	_, r = docExec(t, f.d, run, fmt.Sprintf(`{"op":"propose_statement","scope":"user","from_id":%q,"to_id":%q,"kind":"cites","method":"rule:l2",%s}`, f.provA, f.provB, ev))
	if !r.IsError || !strings.Contains(r.Text, "statement_asserted") {
		t.Errorf("a rule write of a non-auto kind over an asserted triple: %s", r.Text)
	}
}

// A competing proposal (id) stays a proposal even from a rule: auto-asserting it
// would put a second asserted statement beside the one it revises.
func TestProposeStatement_RuleRevisionStaysProposed(t *testing.T) {
	f := newRelFixture(t, relationsOntologyMD)
	run := statementMethodRun(f.ctx, "rule")
	base, refused, text := f.link(t, "supervises", f.provA, f.provB, "")
	if refused {
		t.Fatalf("link: %s", text)
	}
	out, r := docExec(t, f.d, run, fmt.Sprintf(`{"op":"propose_statement","scope":"user","id":%q,"method":"rule:l6","evidence":[{"chunk_id":%q,"quote":"x"}]}`, base["id"], f.provA))
	if r.IsError || out["status"] != "proposed" {
		t.Fatalf("a rule revision = %v %s", out, r.Text)
	}
}

// upsert_chunk's relation statement carries the stamped method in place of
// consolidator; its runtime edges (about) keep theirs.
func TestUpsertChunk_RelationCarriesTheStampedMethod(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, ctx context.Context) {
		dave := subjectNode(t, d, ctx, "Dave")
		shop := subjectNode(t, d, ctx, "Shop")
		facts, _ := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"Facts"}`)
		run := statementMethodRun(ctx, "extractor")
		out, r := docExec(t, d, run, fmt.Sprintf(`{"op":"upsert_chunk","scope":"user","document_id":%q,"natural_key":"dave|works_at|shop","title":"Dave works at the shop.",
			"body":"Dave works at the shop.","about_ids":[%q,%q],"relation":"works_at","method":"extractor:kb_facts"}`, facts["document_id"], dave, shop))
		if r.IsError {
			t.Fatalf("upsert: %s", r.Text)
		}
		rel, _ := out["relation"].(map[string]any)
		if rel == nil {
			t.Fatalf("relation = %v", out)
		}
		if got := statementMethodOf(t, d, ctx, rel["statement_id"].(string)); got != "extractor:kb_facts" {
			t.Errorf("relation method = %q, want extractor:kb_facts", got)
		}
		key, _, _ := d.resolveScope(ctx, "user")
		res, err := d.query(ctx, key, `SELECT method FROM chunk_edges WHERE from_id = ? AND kind = 'about'`, out["id"])
		if err != nil || len(res.Rows) == 0 || asStr(res.Rows[0][0]) != edgeMethodRuntime {
			t.Errorf("the fact's about edges = %v %v, want method runtime", res, err)
		}
		_, r = docExec(t, d, statementMethodRun(ctx), fmt.Sprintf(`{"op":"upsert_chunk","scope":"user","document_id":%q,"natural_key":"dave|x","title":"t","body":"b","method":"rule:x"}`, facts["document_id"]))
		if !r.IsError || !strings.Contains(r.Text, "method_not_granted") {
			t.Errorf("upsert_chunk with an ungranted method: %s", r.Text)
		}
	})
}

// HasStatementMethodGrant answers for the run's own policy: a code-js run
// holding any method, never a run without one nor the operator plane.
func TestHasStatementMethodGrant_ReportsTheRunsOwnGrant(t *testing.T) {
	ctx := context.Background()
	if HasStatementMethodGrant(ctx) || HasStatementMethodGrant(operatorPlane(ctx)) || HasStatementMethodGrant(statementMethodRun(ctx)) {
		t.Error("a caller without a grant reported one")
	}
	if !HasStatementMethodGrant(statementMethodRun(ctx, "extractor")) {
		t.Error("a run holding extractor reported no grant")
	}
}
