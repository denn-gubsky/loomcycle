package builtin

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// A caller's own confidence never hides a statement — only a verdict does — and
// accepting a proposal makes it visible even when a judge had refuted it.
func TestAcceptStatement_AcceptedStatementIsVisible(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, ctx context.Context) {
		docID, a, b, c := stmtDoc(t, d, ctx, "accept")
		op := operatorCtx(ctx)
		low, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"propose_statement","scope":"user","from_id":%q,"to_id":%q,"kind":"cites","confidence":0.1}`, a, b))
		if r.IsError {
			t.Fatal(r.Text)
		}
		refuted, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"propose_statement","scope":"user","from_id":%q,"to_id":%q,"kind":"cites","confidence":0.9,
			"evidence":[{"chunk_id":%q,"quote":"Article 12"}]}`, a, c, a))
		if r.IsError {
			t.Fatal(r.Text)
		}
		if _, r := docExec(t, d, inRunCtx(ctx), fmt.Sprintf(`{"op":"judge_fact","scope":"user","id":%q,"verdict":"unsupported","reason":"wrong article"}`, refuted["id"])); r.IsError {
			t.Fatal(r.Text)
		}
		s := integrityEdge(t, d, ctx, "user", refuted["id"].(string))
		if s["confidence"] != 0.9 || s["verdict_confidence"] != confidenceUnsupported {
			t.Errorf("the verdict overwrote the caller's confidence: %v", s)
		}
		for _, id := range []any{low["id"], refuted["id"]} {
			if _, r := docExec(t, d, op, fmt.Sprintf(`{"op":"accept_statement","scope":"user","id":%q,"reason":"checked"}`, id)); r.IsError {
				t.Fatal(r.Text)
			}
		}
		listed := map[any]bool{}
		for _, e := range edgesOf(t, d, ctx, fmt.Sprintf(`{"op":"get_edges","scope":"user","document_id":%q}`, docID)) {
			listed[e["id"]] = true
		}
		if !listed[low["id"]] || !listed[refuted["id"]] {
			t.Errorf("an accepted statement is withheld: listed=%v low=%v refuted=%v", listed, low["id"], refuted["id"])
		}
		s = integrityEdge(t, d, ctx, "user", refuted["id"].(string))
		if _, ok := s["verdict_confidence"]; ok || s["judged_by"] != "operator" || s["confidence"] != 0.9 {
			t.Errorf("accept over a refuting verdict = %v, want it cleared and the operator recorded", s)
		}
	})
}

// Accepting a proposal a judge found supported keeps the judge's verdict: the
// accept has nothing to add to it.
func TestAcceptStatement_KeepsASupportingVerdict(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	_, a, b, _ := stmtDoc(t, d, ctx, "keep")
	p, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"propose_statement","scope":"user","from_id":%q,"to_id":%q,"kind":"cites",
		"evidence":[{"chunk_id":%q,"quote":"Article 12"}]}`, a, b, a))
	if r.IsError {
		t.Fatal(r.Text)
	}
	if _, r := docExec(t, d, inRunCtx(ctx), fmt.Sprintf(`{"op":"judge_fact","scope":"user","id":%q,"verdict":"supported","reason":"the clause names it"}`, p["id"])); r.IsError {
		t.Fatal(r.Text)
	}
	if _, r := docExec(t, d, operatorCtx(ctx), fmt.Sprintf(`{"op":"accept_statement","scope":"user","id":%q}`, p["id"])); r.IsError {
		t.Fatal(r.Text)
	}
	s := integrityEdge(t, d, ctx, "user", p["id"].(string))
	if s["status"] != edgeStatusAsserted || s["judge_reason"] != "the clause names it" || s["verdict_confidence"] != confidenceSupported {
		t.Errorf("accept destroyed the judge's verdict: %v", s)
	}
}

// A ruling is on a proposal (accept or reject) or on a rejection (accept): never
// on a statement that is already asserted, and a [[name]] link is pointed at its
// body.
func TestReviewStatement_RulesOnlyOnAProposalOrARejection(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, ctx context.Context) {
		_, a, b, c := stmtDoc(t, d, ctx, "rule")
		op := operatorCtx(ctx)
		l, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"link_chunks","scope":"user","from_id":%q,"to_id":%q,"kind":"cites"}`, a, b))
		if r.IsError {
			t.Fatal(r.Text)
		}
		id := l["id"].(string)
		for _, body := range []string{
			`{"op":"reject_statement","scope":"user","id":"` + id + `","reason":"x"}`,
			`{"op":"accept_statement","scope":"user","id":"` + id + `"}`,
		} {
			if _, r := docExec(t, d, op, body); !r.IsError || !strings.Contains(r.Text, "not_a_proposal") {
				t.Errorf("a ruling on an asserted statement: want not_a_proposal, got %s", r.Text)
			}
		}
		if s := integrityEdge(t, d, ctx, "user", id); s["status"] != edgeStatusAsserted || s["judged_by"] != nil {
			t.Errorf("a refused ruling changed the statement: %v", s)
		}
		// A rejection can be accepted (rejected in error), not rejected again.
		p, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"propose_statement","scope":"user","from_id":%q,"to_id":%q,"kind":"cites"}`, a, c))
		if r.IsError {
			t.Fatal(r.Text)
		}
		pid := p["id"].(string)
		if _, r := docExec(t, d, op, `{"op":"reject_statement","scope":"user","id":"`+pid+`","reason":"first"}`); r.IsError {
			t.Fatal(r.Text)
		}
		if _, r := docExec(t, d, op, `{"op":"reject_statement","scope":"user","id":"`+pid+`","reason":"second"}`); !r.IsError {
			t.Errorf("a rejection was rejected again")
		}
		if s := integrityEdge(t, d, ctx, "user", pid); s["judge_reason"] != "first" {
			t.Errorf("the second ruling replaced the first reason: %v", s)
		}
		if out, r := docExec(t, d, op, `{"op":"accept_statement","scope":"user","id":"`+pid+`"}`); r.IsError || out["status"] != edgeStatusAsserted {
			t.Fatalf("accepting a rejection: %v %s", out, r.Text)
		}
		// A wikilink is asserted by its body; the refusal says where to remove it.
		w, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"create_chunk","scope":"user","document_id":%q,"title":"Linker","body":"See [[Article 12]]."}`, firstDocOf(t, d, ctx, a)))
		if r.IsError {
			t.Fatal(r.Text)
		}
		key, _, _ := d.resolveScope(ctx, "user")
		res, err := d.query(ctx, key, `SELECT id FROM chunk_edges WHERE from_id = ? AND auto = 1`, w["id"])
		if err != nil || len(res.Rows) != 1 {
			t.Fatalf("wikilink edge: %v %v", res, err)
		}
		_, r = docExec(t, d, op, `{"op":"reject_statement","scope":"user","id":"`+asStr(res.Rows[0][0])+`","reason":"no"}`)
		if !r.IsError || !strings.Contains(r.Text, "body") {
			t.Errorf("rejecting a wikilink: want a refusal pointing at the body, got %s", r.Text)
		}
	})
}
