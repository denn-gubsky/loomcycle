package builtin

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// Linking a relation that waits for review asserts it only for a caller who may
// rule there; for anyone else — a run, even one granted link — the link adds its
// evidence and the proposal keeps waiting. A permitted caller's flip is recorded
// as the ruling it is.
func TestLinkChunks_AnExistingProposalIsAssertedOnlyByAReviewer(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, base context.Context) {
		op := reviewOperatorCtx(base, nil)
		a, b := twoChunks(t, d, op, "tenant", "/kb/review")
		p, r := docExec(t, d, tenantRunCtx(base), fmt.Sprintf(`{"op":"propose_statement","scope":"tenant","from_id":%q,"to_id":%q,"kind":"cites"}`, a, b))
		if r.IsError || p["status"] != edgeStatusProposed {
			t.Fatalf("propose = %v %s", p, r.Text)
		}
		id := asStr(p["id"])

		out := linkFrom(t, d, tenantRunCtx(base, statementLinkGrant), "tenant", a, b, "cites")
		if s := statementOf(t, d, op, "tenant", id); out["status"] != edgeStatusProposed || s["status"] != edgeStatusProposed {
			t.Errorf("a granted run's link over a proposal = %v (stored %v), want it left proposed", out["status"], s["status"])
		}

		linkFrom(t, d, op, "tenant", a, b, "cites")
		s := statementOf(t, d, op, "tenant", id)
		if s["status"] != edgeStatusAsserted || s["operator_ruled_at"] == nil || !strings.HasPrefix(asStr(s["ruled_by"]), "operator") {
			t.Errorf("the operator's link over a proposal = %v, want asserted and recorded as the operator's ruling", s)
		}
	})
}

// remember is the one way a run files a fact as something a person said; the
// same row written through upsert_chunk — the remembered key namespace, or a
// self-citing evidential fact — needs the same grant. A fact distilled from a
// source keeps working.
func TestUpsertChunk_ARunCannotForgeARememberedFact(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, base context.Context) {
		run := tenantRunCtx(base)
		doc := newValidityDoc(t, d, run, "/facts")
		const text = "The user deploys on Fridays."
		forged := map[string]string{
			"the remembered key": fmt.Sprintf(`{"op":"upsert_chunk","scope":"user","document_id":%q,"natural_key":"memory/operator/the-user-deploys-on-fridays","title":%q,"body":%q}`, doc, text, text),
			"a self-citing fact": fmt.Sprintf(`{"op":"upsert_chunk","scope":"user","document_id":%q,"natural_key":"fact:fridays","title":%q,"body":%q,"source_quote":%q,"class":"evidential"}`, doc, text, text, text),
		}
		for name, req := range forged {
			if _, r := docExec(t, d, run, req); !r.IsError || !strings.Contains(r.Text, "remember") {
				t.Errorf("%s from an ungranted run = %v %s, want a refusal naming remember", name, r.IsError, r.Text)
			}
			if _, r := docExec(t, d, withStatementGrant(run, statementRememberGrant), req); r.IsError {
				t.Errorf("%s from a granted run was refused: %s", name, r.Text)
			}
		}
		distilled := fmt.Sprintf(`{"op":"upsert_chunk","scope":"user","document_id":%q,"natural_key":"fact:fridays-derived","title":%q,"body":%q,"source_quote":"we ship on fridays","source_session_id":"s-1"}`, doc, text, text)
		if _, r := docExec(t, d, run, distilled); r.IsError {
			t.Errorf("a fact distilled from a chat was refused: %s", r.Text)
		}
	})
}

// A code agent's extractor: relation is a model's finding: in the tenant it is
// filed for review even when the agent holds link; a rule: one is its code's
// and holds.
func TestFactRelation_AnExtractorRelationIsAlwaysReviewedInTheTenant(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, base context.Context) {
		op := reviewOperatorCtx(base, nil)
		a, b := twoChunks(t, d, op, "tenant", "/kb/x")
		out, _ := docExec(t, d, op, `{"op":"get_chunk","scope":"tenant","id":"`+a+`"}`)
		doc := asStr(out["document_id"])
		run := statementMethodRun(tenantRunCtx(base), "rule", "extractor", statementLinkGrant)
		for method, want := range map[string]string{"extractor:ner": edgeStatusProposed, "rule:cite": edgeStatusAsserted} {
			kind := strings.SplitN(method, ":", 2)[0] + "_rel"
			out, r := docExec(t, d, run, fmt.Sprintf(`{"op":"upsert_chunk","scope":"tenant","document_id":%q,"natural_key":%q,
				"title":"A and B.","body":"A and B.","about_ids":[%q,%q],"relation":%q,"method":%q}`, doc, "fact:"+method, a, b, kind, method))
			if r.IsError {
				t.Fatalf("%s: %s", method, r.Text)
			}
			if rel, _ := out["relation"].(map[string]any); rel == nil || rel["status"] != want {
				t.Errorf("%s relation = %v, want %s", method, out["relation"], want)
			}
		}
	})
}

// A run's proposal is the run's, not a person's: method agent, and a manual row a
// run wrote (before runs were named) is not a person's either.
func TestProposeStatement_ARunIsRecordedAsTheActor(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, base context.Context) {
		run := tenantRunCtx(base)
		a, b := twoChunks(t, d, run, "user", "/docs/p")
		p, r := docExec(t, d, run, fmt.Sprintf(`{"op":"propose_statement","from_id":%q,"to_id":%q,"kind":"cites"}`, a, b))
		if r.IsError {
			t.Fatal(r.Text)
		}
		if s := statementOf(t, d, run, "user", asStr(p["id"])); s["method"] != edgeMethodAgent {
			t.Errorf("a run's proposal = method %v, want agent", s["method"])
		}
		if madeByAPerson(&statementRow{Method: edgeMethodManual, RunID: "run-1"}) {
			t.Error("a manual statement a run wrote reads as a person's")
		}
		if !madeByAPerson(&statementRow{Method: edgeMethodManual}) {
			t.Error("a person's manual statement no longer reads as a person's")
		}
	})
}
