package builtin

import (
	"context"
	"fmt"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/connector"
)

// A bulk demote (RFC GM P3.3): live asserted statements a machine put in force
// go back to proposed by the review queue's filter, dry run by default, an
// operator's act alone.

// assertAsAMachine puts ids in force as a review policy's acceptance does —
// asserted, ruled_by policy, no operator_ruled_at — without going through a
// policy, so the test needs no ontology.
func assertAsAMachine(t *testing.T, d *Document, ctx context.Context, ids ...string) {
	t.Helper()
	key, _, err := d.resolveScope(ctx, "tenant")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		if err := d.exec(ctx, key, `UPDATE chunk_edges SET status = ?, ruled_by = ?, judged_by = ? WHERE id = ?`,
			edgeStatusAsserted, "policy:1@1", "policy", id); err != nil {
			t.Fatalf("assert %s: %v", id, err)
		}
	}
}

func TestBulkDemote_LivePlacementsOfAMethodGoBackToProposed(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, base context.Context) {
		c := seedReviewCorpus(t, d, base)
		op := reviewOperatorCtx(base, nil)
		assertAsAMachine(t, d, op, c.cites, c.repeals, c.elaborates)
		req := connector.RuleStatementsRequest{Action: rulingActionDemote, Reason: "the citation rule misread the title map",
			Filter: &connector.ReviewQueueFilter{Method: "rule:citation"}}
		res := ruleReal(t, d, op, req, RuleAsOperator)
		if res.RulingID == "" || res.Matched != 2 || res.Ruled != 2 || res.Failed != 0 {
			t.Fatalf("demote = %+v, want the two live rule:citation statements demoted", res)
		}
		for _, id := range []string{c.cites, c.repeals} {
			s := tenantEdge(t, d, op, id)
			if s["status"] != edgeStatusProposed || s["judge_reason"] != req.Reason || s["judged_by"] != "operator" {
				t.Errorf("%s = %v, want proposed with the reason recorded as maintain_statement demote records it", id, s)
			}
		}
		// Another method, and a proposal of the same method, are not touched.
		if s := tenantEdge(t, d, op, c.elaborates); s["status"] != edgeStatusAsserted {
			t.Errorf("the extractor's statement = %v, want still asserted", s["status"])
		}
		if s := tenantEdge(t, d, op, c.judged); s["status"] != edgeStatusProposed || s["judge_reason"] != nil {
			t.Errorf("a proposal of the method = %v, want untouched", s)
		}
		key, _, _ := d.resolveScope(op, "tenant")
		row, err := d.query(op, key, `SELECT action, reason, matched, ruled FROM statement_rulings WHERE id = ?`, res.RulingID)
		if err != nil || len(row.Rows) != 1 {
			t.Fatalf("ruling record: %v %v", row, err)
		}
		if r := row.Rows[0]; asStr(r[0]) != rulingActionDemote || asStr(r[1]) != req.Reason || asInt(r[2]) != 2 || asInt(r[3]) != 2 {
			t.Errorf("ruling record = %v", r)
		}
		items, err := d.query(op, key, `SELECT COUNT(*) FROM statement_ruling_items WHERE ruling_id = ?`, res.RulingID)
		if err != nil || asInt(items.Rows[0][0]) != 2 {
			t.Errorf("ruling items = %v %v, want one per demoted statement", items, err)
		}
		// A demote is undone by accepting again, never by restoring the status.
		if _, err := d.RevertRuling(op, "tenant", connector.RevertRulingRequest{RulingID: res.RulingID}, RuleAsOperator); connector.StatementReviewCode(err) != "invalid_request" {
			t.Errorf("revert of a demote: %v, want invalid_request", err)
		}
	})
}

func TestBulkDemote_DryRunChangesNothing(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, base context.Context) {
		c := seedReviewCorpus(t, d, base)
		op := reviewOperatorCtx(base, nil)
		assertAsAMachine(t, d, op, c.cites, c.repeals)
		dry := ruleDry(t, d, op, connector.RuleStatementsRequest{Action: rulingActionDemote, Reason: "recheck",
			Filter: &connector.ReviewQueueFilter{Method: "rule:", FromUnderPath: "/board"}}, RuleAsOperator)
		if !dry.DryRun || dry.RulingID != "" || dry.Matched != 2 || dry.ToRule != 2 || len(dry.Sample) != 2 {
			t.Fatalf("dry run = %+v", dry)
		}
		for _, id := range []string{c.cites, c.repeals} {
			if s := tenantEdge(t, d, op, id); s["status"] != edgeStatusAsserted || s["judge_reason"] != nil {
				t.Errorf("the dry run demoted %s: %v", id, s)
			}
		}
		key, _, _ := d.resolveScope(op, "tenant")
		if row, err := d.query(op, key, `SELECT COUNT(*) FROM statement_rulings`); err != nil || asInt(row.Rows[0][0]) != 0 {
			t.Errorf("the dry run recorded a ruling: %v %v", row, err)
		}
	})
}

func TestBulkDemote_LeavesAPersonsStatementAlone(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, base context.Context) {
		c := seedReviewCorpus(t, d, base)
		op := reviewOperatorCtx(base, nil)
		assertAsAMachine(t, d, op, c.cites)
		// repeals: an operator's acceptance. A person's own link from the console.
		if _, r := docExec(t, d, op, `{"op":"accept_statement","scope":"tenant","id":"`+c.repeals+`"}`); r.IsError {
			t.Fatal(r.Text)
		}
		out, r := docExec(t, d, op, fmt.Sprintf(`{"op":"link_chunks","scope":"tenant","from_id":%q,"to_id":%q,"kind":"elaborates"}`, c.b2, c.n1))
		if r.IsError {
			t.Fatal(r.Text)
		}
		manual, _ := out["id"].(string)
		if s := tenantEdge(t, d, op, manual); s["status"] != edgeStatusAsserted || s["method"] != edgeMethodManual {
			t.Fatalf("the person's link = %v, want asserted manual", s)
		}
		// By filter, neither is selected: matched counts only what may be demoted.
		byFilter := connector.RuleStatementsRequest{Action: rulingActionDemote, Reason: "recheck", Filter: &connector.ReviewQueueFilter{}}
		if dry := ruleDry(t, d, op, byFilter, RuleAsOperator); dry.Matched != 1 || dry.ToRule != 1 || dry.Failed != 0 {
			t.Errorf("demote by filter dry run = %+v, want only the machine's statement matched", dry)
		}
		// By ids, each is reported, in the dry run as in the real call.
		req := connector.RuleStatementsRequest{Action: rulingActionDemote, Reason: "recheck", IDs: []string{c.cites, c.repeals, manual}}
		want := map[string]string{c.repeals: "statement_operator_ruled", manual: "statement_manual"}
		check := func(what string, res connector.RuleStatementsResult) {
			t.Helper()
			got := map[string]string{}
			for _, it := range res.Items {
				if it.Error != "" {
					got[it.ID] = it.Error
				}
			}
			if fmt.Sprint(got) != fmt.Sprint(want) || res.Failed != 2 {
				t.Errorf("%s refusals = %v (failed %d), want %v", what, got, res.Failed, want)
			}
		}
		dry := ruleDry(t, d, op, req, RuleAsOperator)
		check("dry run", dry)
		if dry.ToRule != 1 {
			t.Errorf("dry run to_rule = %d, want only the machine's statement", dry.ToRule)
		}
		res := ruleReal(t, d, op, req, RuleAsOperator)
		check("demote", res)
		if res.Ruled != 1 {
			t.Errorf("demote ruled %d, want 1", res.Ruled)
		}
		for id, status := range map[string]string{c.cites: edgeStatusProposed, c.repeals: edgeStatusAsserted, manual: edgeStatusAsserted} {
			if s := tenantEdge(t, d, op, id); s["status"] != status {
				t.Errorf("%s = %v, want %s", id, s["status"], status)
			}
		}
	})
}

// The newest asserted statements are an operator's: a demote by filter, max 1
// at a time, still works through the machine's ones beneath them and then
// finds nothing, rather than selecting the operator's on every call.
func TestBulkDemote_SuccessiveCallsProgressPastTheOperatorsStatements(t *testing.T) {
	d, base, _ := documentFixture(t)
	c := seedReviewCorpus(t, d, base)
	op := reviewOperatorCtx(base, nil)
	assertAsAMachine(t, d, op, c.cites, c.repeals)
	for _, id := range []string{c.elaborates, c.judged} {
		if _, r := docExec(t, d, op, `{"op":"accept_statement","scope":"tenant","id":"`+id+`"}`); r.IsError {
			t.Fatal(r.Text)
		}
	}
	req := connector.RuleStatementsRequest{Action: rulingActionDemote, Reason: "recheck", Filter: &connector.ReviewQueueFilter{}, Max: 1}
	for i, want := range []string{c.repeals, c.cites} {
		res := ruleReal(t, d, op, req, RuleAsOperator)
		if res.Ruled != 1 || res.Failed != 0 || len(res.Items) != 1 || res.Items[0].ID != want {
			t.Fatalf("call %d = %+v, want %s demoted", i+1, res, want)
		}
	}
	if dry := ruleDry(t, d, op, req, RuleAsOperator); dry.Matched != 0 {
		t.Errorf("after both are demoted the filter matches %d, want 0", dry.Matched)
	}
	for _, id := range []string{c.elaborates, c.judged} {
		if s := tenantEdge(t, d, op, id); s["status"] != edgeStatusAsserted {
			t.Errorf("the operator's %s = %v, want still asserted", id, s["status"])
		}
	}
}

// An operator's ruling made after the selection is checked again at the
// write: the demote reports the statement changed and leaves it.
func TestBulkDemote_AnOperatorRulingAfterTheSelectionIsLeft(t *testing.T) {
	d, base, _ := documentFixture(t)
	c := seedReviewCorpus(t, d, base)
	op := reviewOperatorCtx(base, nil)
	assertAsAMachine(t, d, op, c.cites)
	key, mscope, err := d.resolveScope(op, "tenant")
	if err != nil {
		t.Fatal(err)
	}
	s, err := d.statementByID(op, key, c.cites)
	if err != nil || s == nil {
		t.Fatalf("read %s: %v", c.cites, err)
	}
	// Selected as a machine's; an operator rules on it before the write.
	if err := d.exec(op, key, `UPDATE chunk_edges SET operator_ruled_at = ? WHERE id = ?`, int64(1), c.cites); err != nil {
		t.Fatal(err)
	}
	item, ok := d.demoteOne(op, key, mscope, "ruling-1", "recheck", reviewCandidate{s: *s})
	if ok || item.Error != rulingChanged {
		t.Errorf("demote after an operator's ruling = %+v ok %v, want %s", item, ok, rulingChanged)
	}
	if got := tenantEdge(t, d, op, c.cites); got["status"] != edgeStatusAsserted {
		t.Errorf("%s = %v, want still asserted", c.cites, got["status"])
	}
}

func TestBulkDemote_RefusedToAReviewerAndInsideARun(t *testing.T) {
	d, base, _ := documentFixture(t)
	c := seedReviewCorpus(t, d, base)
	assertAsAMachine(t, d, reviewOperatorCtx(base, nil), c.cites)
	req := connector.RuleStatementsRequest{Action: rulingActionDemote, Reason: "recheck", IDs: []string{c.cites}}
	if _, err := d.RuleStatements(reviewOperatorCtx(base, nil), "tenant", req, RuleAsReviewer); connector.StatementReviewCode(err) != "forbidden" {
		t.Errorf("a reviewer's demote: %v, want forbidden", err)
	}
	if _, err := d.RuleStatements(reviewWriterCtx(base), "tenant", req, RuleAsOperator); connector.StatementReviewCode(err) != "forbidden" {
		t.Errorf("a demote from inside a run: %v, want forbidden", err)
	}
	if s := tenantEdge(t, d, reviewOperatorCtx(base, nil), c.cites); s["status"] != edgeStatusAsserted {
		t.Errorf("a refused demote demoted: %v", s)
	}
}

func TestBulkDemote_NeedsAReasonAnAssertedFilterAndABoundedBatch(t *testing.T) {
	d, base, _ := documentFixture(t)
	op := reviewOperatorCtx(base, nil)
	ids := make([]string, reviewRulingMax+1)
	for i := range ids {
		ids[i] = fmt.Sprintf("%032x", i)
	}
	for what, req := range map[string]connector.RuleStatementsRequest{
		"no reason":       {Action: rulingActionDemote, Filter: &connector.ReviewQueueFilter{}},
		"blank reason":    {Action: rulingActionDemote, Reason: "  ", Filter: &connector.ReviewQueueFilter{}},
		"proposed filter": {Action: rulingActionDemote, Reason: "x", Filter: &connector.ReviewQueueFilter{Status: edgeStatusProposed}},
		"too many ids":    {Action: rulingActionDemote, Reason: "x", IDs: ids},
		"max over cap":    {Action: rulingActionDemote, Reason: "x", Filter: &connector.ReviewQueueFilter{}, Max: reviewRulingMax + 1},
	} {
		if _, err := d.RuleStatements(op, "tenant", req, RuleAsOperator); connector.StatementReviewCode(err) != "invalid_request" {
			t.Errorf("%s: %v, want invalid_request", what, err)
		}
	}
	// A statement that is not asserted, named by id, is reported, not demoted.
	c := seedReviewCorpus(t, d, base)
	dry := ruleDry(t, d, op, connector.RuleStatementsRequest{Action: rulingActionDemote, Reason: "x", IDs: []string{c.cites}}, RuleAsOperator)
	if dry.ToRule != 0 || len(dry.Items) != 1 || dry.Items[0].Error != rulingNotAsserted {
		t.Errorf("demote of a proposal by id = %+v, want it reported %s", dry, rulingNotAsserted)
	}
}
