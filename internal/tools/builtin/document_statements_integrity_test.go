package builtin

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// integrityEdge reads one statement through get_edge.
func integrityEdge(t *testing.T, d *Document, ctx context.Context, scope, id string) map[string]any {
	t.Helper()
	out, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"get_edge","scope":%q,"id":%q}`, scope, id))
	if r.IsError {
		t.Fatalf("get_edge %s: %s", id, r.Text)
	}
	return out["statement"].(map[string]any)
}

// integritySubject files a subject node in the user scope and returns its root.
func integritySubject(t *testing.T, d *Document, ctx context.Context, name string) string {
	t.Helper()
	out, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"create_document","scope":"user","title":%q,"path":"/facts/%s",
		"type":"person","subject":%q,"natural_key":"person:%s"}`, name, name, name, name))
	if r.IsError {
		t.Fatalf("subject %s: %s", name, r.Text)
	}
	return out["root_chunk_id"].(string)
}

// A fact's relation is a machine write: over a statement still awaiting review it
// adds its evidence and leaves the review to the operator. Only an authored
// link_chunks asserts a proposal.
func TestFactRelation_LeavesAPendingProposalProposed(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, ctx context.Context) {
		dave := integritySubject(t, d, ctx, "dave")
		shop := integritySubject(t, d, ctx, "shop")
		facts, r := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"Facts","path":"/facts/all"}`)
		if r.IsError {
			t.Fatal(r.Text)
		}
		p, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"propose_statement","scope":"user","from_id":%q,"to_id":%q,"kind":"works_at"}`, dave, shop))
		if r.IsError {
			t.Fatal(r.Text)
		}
		id := p["id"].(string)
		out, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"upsert_chunk","scope":"user","document_id":%q,"natural_key":"m/x","title":"Dave works at the shop",
			"body":"Dave works at the shop.","about_ids":[%q,%q],"relation":"works_at"}`, facts["document_id"], dave, shop))
		if r.IsError {
			t.Fatal(r.Text)
		}
		rel, _ := out["relation"].(map[string]any)
		if rel["statement_id"] != id || rel["status"] != edgeStatusProposed {
			t.Errorf("relation = %v, want the proposal %s still proposed", out["relation"], id)
		}
		s := integrityEdge(t, d, ctx, "user", id)
		if s["status"] != edgeStatusProposed {
			t.Errorf("a fact upsert asserted a pending proposal: status = %v", s["status"])
		}
		if n, _ := s["evidence_count"].(float64); n != 1 {
			t.Errorf("the fact's evidence was not added to the proposal: %v", s)
		}
		// The authored link of a caller holding `link` is what asserts it.
		l, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"link_chunks","scope":"user","from_id":%q,"to_id":%q,"kind":"works_at"}`, dave, shop))
		if r.IsError || l["id"] != id || l["status"] != edgeStatusAsserted {
			t.Fatalf("link_chunks over the proposal: %v %s", l, r.Text)
		}
	})
}

// The propose verb files for review; it does not add to a relation already live.
func TestProposeStatement_DoesNotAddEvidenceToAnAssertedStatement(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, ctx context.Context) {
		_, a, b, _ := stmtDoc(t, d, ctx, "live")
		l, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"link_chunks","scope":"user","from_id":%q,"to_id":%q,"kind":"cites"}`, a, b))
		if r.IsError {
			t.Fatal(r.Text)
		}
		_, r = docExec(t, d, ctx, fmt.Sprintf(`{"op":"propose_statement","scope":"user","from_id":%q,"to_id":%q,"kind":"cites",
			"evidence":[{"chunk_id":%q,"quote":"A"}]}`, a, b, b))
		if !r.IsError || !strings.Contains(r.Text, "statement_asserted") {
			t.Errorf("a proposal over an asserted statement: want statement_asserted, got %s", r.Text)
		}
		if s := integrityEdge(t, d, ctx, "user", l["id"].(string)); s["evidence_count"] != float64(0) {
			t.Errorf("a proposal changed live evidence: %v", s)
		}
	})
}
