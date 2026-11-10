package builtin

import (
	"context"
	"encoding/json"
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
		out, err := d.applyBulkRuling(f.op, key, mscope, req, true, "", connector.RuleStatementsResult{Matched: 1}, picked, nil, nil)
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

// A bulk accept that names its statements by id never reverses a rejection: a
// statement rejected between the dry run and the real call (which selects
// again, and reads it rejected) is reported and left rejected. Reversing a
// rejection in bulk takes selecting the rejected statements by filter. A bulk
// reject by id is unchanged.
func TestRuleStatements_ABulkAcceptByIdsNeverReversesARejection(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, base context.Context) {
		f := newPolicyFixture(t, d, base, judgeOntologyMD)
		rejected := f.propose(f.one, f.notes, "elaborates", `,"method":"extractor:relate"`)["id"].(string)
		proposed := f.propose(f.two, f.notes, "elaborates", `,"method":"extractor:relate"`)["id"].(string)
		req := connector.RuleStatementsRequest{Action: "accept", IDs: []string{rejected, proposed}}
		dry := ruleDry(t, d, f.op, req, RuleAsOperator)
		if dry.Matched != 2 {
			t.Fatalf("dry run = %+v, want both matched", dry)
		}
		if _, r := docExec(t, d, operatorCtx(f.op), fmt.Sprintf(`{"op":"reject_statement","scope":"tenant","id":%q,"reason":"not an elaboration"}`, rejected)); r.IsError {
			t.Fatalf("reject: %s", r.Text)
		}
		req.DryRun, req.ExpectCount = boolPtr(false), intPtr(dry.Matched)
		res, err := d.RuleStatements(f.op, "tenant", req, RuleAsOperator)
		if err != nil {
			t.Fatal(err)
		}
		items := map[string]connector.RulingItem{}
		for _, it := range res.Items {
			items[it.ID] = it
		}
		if res.Ruled != 1 || res.Failed != 1 || items[rejected].Error != rulingNotProposal || items[proposed].Status != edgeStatusAsserted {
			t.Errorf("bulk accept by ids = ruled %d failed %d items %+v, want the rejected one reported %s and the proposal accepted", res.Ruled, res.Failed, res.Items, rulingNotProposal)
		}
		if e := f.edge(rejected); e["status"] != edgeStatusRejected || !strings.HasPrefix(asStr(e["ruled_by"]), "operator") {
			t.Errorf("after the bulk accept by ids = %v, want the operator's rejection kept", e)
		}

		// A bulk reject by id still rules on what it names (here: a statement
		// already asserted is not a proposal, as before; a proposal is rejected).
		third := f.propose(f.oneB, f.notes, "elaborates", `,"method":"extractor:relate"`)["id"].(string)
		rj := ruleReal(t, d, f.op, connector.RuleStatementsRequest{Action: "reject", Reason: "no", IDs: []string{third}}, RuleAsOperator)
		if rj.Ruled != 1 || f.edge(third)["status"] != edgeStatusRejected {
			t.Errorf("bulk reject by ids = %+v, want the proposal rejected", rj)
		}
	})
}

// The dry run of a bulk accept by ids reports what the real call will do: a
// statement already rejected is refused not_a_proposal by the real call, so the
// dry run leaves it out of to_rule, the breakdown and the sample and reports
// it as the real call does. matched still counts it, so an expect_count taken
// from the dry run matches the real call.
func TestRuleStatements_ABulkAcceptByIdsDryRunReportsARejectionAsRefused(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, base context.Context) {
		f := newPolicyFixture(t, d, base, judgeOntologyMD)
		rejected := f.propose(f.one, f.notes, "elaborates", `,"method":"extractor:relate"`)["id"].(string)
		proposed := f.propose(f.two, f.notes, "elaborates", `,"method":"extractor:relate"`)["id"].(string)
		if _, r := docExec(t, d, operatorCtx(f.op), fmt.Sprintf(`{"op":"reject_statement","scope":"tenant","id":%q,"reason":"not an elaboration"}`, rejected)); r.IsError {
			t.Fatalf("reject: %s", r.Text)
		}
		req := connector.RuleStatementsRequest{Action: "accept", IDs: []string{rejected, proposed}}
		dry := ruleDry(t, d, f.op, req, RuleAsOperator)
		inBreakdown := 0
		for _, b := range dry.Breakdown {
			inBreakdown += b.Count
		}
		var sampled []string
		for _, raw := range dry.Sample {
			var s struct {
				ID string `json:"id"`
			}
			if err := json.Unmarshal(raw, &s); err != nil {
				t.Fatal(err)
			}
			sampled = append(sampled, s.ID)
		}
		if dry.Matched != 2 || dry.ToRule != 1 || inBreakdown != 1 || len(sampled) != 1 || sampled[0] != proposed {
			t.Errorf("dry run = matched %d to_rule %d breakdown %d sample %v, want matched 2 and only the proposal to rule", dry.Matched, dry.ToRule, inBreakdown, sampled)
		}
		if dry.Failed != 1 || len(dry.Items) != 1 || dry.Items[0].ID != rejected || dry.Items[0].Error != rulingNotProposal {
			t.Errorf("dry run = failed %d items %+v, want the rejection reported %s", dry.Failed, dry.Items, rulingNotProposal)
		}

		req.DryRun, req.ExpectCount = boolPtr(false), intPtr(dry.Matched)
		res, err := d.RuleStatements(f.op, "tenant", req, RuleAsOperator)
		if err != nil {
			t.Fatal(err)
		}
		if res.ToRule != dry.ToRule || res.Ruled != 1 || res.Failed != dry.Failed {
			t.Errorf("real call = to_rule %d ruled %d failed %d, want what the dry run reported (to_rule %d failed %d)", res.ToRule, res.Ruled, res.Failed, dry.ToRule, dry.Failed)
		}
	})
}
