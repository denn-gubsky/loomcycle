package builtin

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/connector"
)

// A review policy or a judge rules only on a proposal. A machine ruling that read
// its candidates before an operator rejected one must not accept that rejection
// afterwards: an operator's rejection is a tombstone, and only a person reverses it.
func TestApplyRuling_APolicyCannotOverturnAnOperatorsRejection(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, base context.Context) {
		f := newPolicyFixture(t, d, base, judgeOntologyMD)
		id := f.propose(f.one, f.notes, "elaborates", `,"method":"extractor:relate"`)["id"].(string)
		if e := f.edge(id); e["status"] != edgeStatusProposed {
			t.Fatalf("filed = %v, want proposed", e)
		}
		if _, r := docExec(t, d, operatorCtx(f.op), fmt.Sprintf(`{"op":"reject_statement","scope":"tenant","id":%q,"reason":"not an elaboration"}`, id)); r.IsError {
			t.Fatalf("reject: %s", r.Text)
		}
		key, mscope, err := d.resolveScope(f.op, "tenant")
		if err != nil {
			t.Fatal(err)
		}
		for _, by := range []string{"policy:1@3", "judge:run-judge"} {
			r, err := d.applyRuling(f.op, key, mscope, "accept_statement", id, true, "", by, "")
			if err != nil || r.code != rulingNotProposal {
				t.Errorf("%s accepting an operator's rejection = %+v %v, want not_a_proposal", by, r, err)
			}
		}
		e := f.edge(id)
		if e["status"] != edgeStatusRejected || e["operator_ruled_at"] == nil || !strings.HasPrefix(asStr(e["ruled_by"]), "operator") {
			t.Errorf("after the machine rulings = %v, want the operator's rejection kept", e)
		}
		// A person may still reverse it.
		if _, r := docExec(t, d, operatorCtx(f.op), fmt.Sprintf(`{"op":"accept_statement","scope":"tenant","id":%q}`, id)); r.IsError {
			t.Errorf("an operator's accept of a rejection was refused: %s", r.Text)
		}
	})
}

// A person's bulk accept rules on what its selection read. One that selected a
// proposal before another operator rejected it must leave that rejection — the
// bulk meant to settle a proposal, not to reverse a ruling it never saw — and
// report the statement as changed. One that selected the rejection itself (a
// filter on status rejected) still reverses it, as accept_statement would.
func TestRuleStatements_ABulkAcceptLeavesARejectionMadeAfterItsSelection(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, base context.Context) {
		f := newPolicyFixture(t, d, base, judgeOntologyMD)
		id := f.propose(f.one, f.notes, "elaborates", `,"method":"extractor:relate"`)["id"].(string)
		key, mscope, err := d.resolveScope(f.op, "tenant")
		if err != nil {
			t.Fatal(err)
		}
		req := connector.RuleStatementsRequest{Action: "accept", IDs: []string{id}}
		_, picked, _, err := d.selectForRuling(f.op, key, req, reviewRulingMax)
		if err != nil || len(picked) != 1 || picked[0].s.Status != edgeStatusProposed {
			t.Fatalf("selection = %+v %v, want the one proposal", picked, err)
		}
		// Another operator rejects it after the selection read it.
		if _, r := docExec(t, d, operatorCtx(f.op), fmt.Sprintf(`{"op":"reject_statement","scope":"tenant","id":%q,"reason":"not an elaboration"}`, id)); r.IsError {
			t.Fatalf("reject: %s", r.Text)
		}
		out, err := d.applyBulkRuling(f.op, key, mscope, req, true, "", connector.RuleStatementsResult{Matched: 1}, picked, nil)
		if err != nil {
			t.Fatal(err)
		}
		if out.Ruled != 0 || out.Failed != 1 || len(out.Items) != 1 || out.Items[0].Error != rulingChanged {
			t.Errorf("bulk accept = ruled %d failed %d items %+v, want the statement reported %s", out.Ruled, out.Failed, out.Items, rulingChanged)
		}
		if e := f.edge(id); e["status"] != edgeStatusRejected || !strings.HasPrefix(asStr(e["ruled_by"]), "operator") {
			t.Errorf("after the bulk accept = %v, want the operator's rejection kept", e)
		}

		// Selecting the rejection itself is a decision to reverse it.
		req = connector.RuleStatementsRequest{Action: "accept", Filter: &connector.ReviewQueueFilter{Status: edgeStatusRejected}}
		res := ruleReal(t, d, f.op, req, RuleAsOperator)
		if res.Ruled != 1 || f.edge(id)["status"] != edgeStatusAsserted {
			t.Errorf("bulk accept of a selected rejection = %+v, status %v, want it reversed", res, f.edge(id)["status"])
		}
	})
}
