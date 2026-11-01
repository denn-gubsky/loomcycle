package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/connector"
)

// Listing a scope's bulk rulings: what an undo needs once the ruling's own
// answer is gone.

func listRulings(t *testing.T, d *Document, ctx context.Context, req connector.ListRulingsRequest, as RulingAuthority) connector.ListRulingsResult {
	t.Helper()
	res, err := d.ListRulings(ctx, "tenant", req, as)
	if err != nil {
		t.Fatalf("list rulings: %v", err)
	}
	return res
}

func rulingIDs(res connector.ListRulingsResult) []string {
	out := []string{}
	for _, r := range res.Rulings {
		out = append(out, r.ID)
	}
	return out
}

// An operator sees every ruling of the scope; a member only its own, whoever
// else ruled there and however; a member with no subject sees none.
func TestListRulings_OperatorSeesAllAMemberOnlyItsOwn(t *testing.T) {
	d, base, _ := documentFixture(t)
	c := seedReviewCorpus(t, d, base)
	ops := WithRulingActor(reviewOperatorCtx(base, nil), "ops")
	rita := WithRulingActor(reviewOperatorCtx(base, nil), "rita")
	byOps := ruleReal(t, d, ops, connector.RuleStatementsRequest{Action: "accept", IDs: []string{c.cites}}, RuleAsOperator)
	byRita := ruleReal(t, d, rita, connector.RuleStatementsRequest{Action: "accept", IDs: []string{c.repeals}}, RuleAsOperator)
	if got := rulingIDs(listRulings(t, d, ops, connector.ListRulingsRequest{}, RuleAsOperator)); strings.Join(got, ",") != byRita.RulingID+","+byOps.RulingID {
		t.Errorf("operator sees %v, want both, newest first", got)
	}
	got := listRulings(t, d, rita, connector.ListRulingsRequest{}, RuleAsReviewer)
	if ids := rulingIDs(got); len(ids) != 1 || ids[0] != byRita.RulingID {
		t.Fatalf("member rita sees %v, want only her own %s", ids, byRita.RulingID)
	}
	if got.Rulings[0].Actor != "rita" || got.Rulings[0].RuledBy == "" {
		t.Errorf("rita's ruling = %+v", got.Rulings[0])
	}
	other := WithRulingActor(reviewOperatorCtx(base, nil), "alice")
	if ids := rulingIDs(listRulings(t, d, other, connector.ListRulingsRequest{}, RuleAsReviewer)); len(ids) != 0 {
		t.Errorf("member alice, who ruled nothing, sees %v", ids)
	}
	nobody := WithRulingActor(reviewOperatorCtx(base, nil), "")
	if ids := rulingIDs(listRulings(t, d, nobody, connector.ListRulingsRequest{}, RuleAsReviewer)); len(ids) != 0 {
		t.Errorf("a member with no subject sees %v", ids)
	}
	if _, err := d.ListRulings(reviewWriterCtx(base), "tenant", connector.ListRulingsRequest{}, RuleAsOperator); connector.StatementReviewCode(err) != "forbidden" {
		t.Errorf("listing from inside a run: %v, want forbidden", err)
	}
}

// A ruling by ids is listed with how many it named, never which.
func TestListRulings_CountsNeverStatementIds(t *testing.T) {
	d, base, _ := documentFixture(t)
	c := seedReviewCorpus(t, d, base)
	op := reviewOperatorCtx(base, nil)
	byIDs := ruleReal(t, d, op, connector.RuleStatementsRequest{Action: "reject", Reason: "wrong act", IDs: []string{c.cites, c.repeals, "0123456789abcdef0123456789abcdef"}}, RuleAsOperator)
	byFilter := ruleReal(t, d, op, connector.RuleStatementsRequest{Action: "accept", Filter: &connector.ReviewQueueFilter{FromUnderPath: "/kb"}, Max: 5}, RuleAsOperator)
	res := listRulings(t, d, op, connector.ListRulingsRequest{}, RuleAsOperator)
	body, _ := json.Marshal(res)
	for _, id := range []string{c.cites, c.repeals, c.elaborates, c.judged, "0123456789abcdef0123456789abcdef"} {
		if strings.Contains(string(body), id) {
			t.Errorf("the listing names statement %s: %s", id, body)
		}
	}
	got := map[string]connector.RulingRecord{}
	for _, r := range res.Rulings {
		got[r.ID] = r
	}
	if r := got[byIDs.RulingID]; r.Action != "reject" || r.Reason != "wrong act" || r.Selector.IDsCount != 3 || r.Selector.Filter != nil ||
		r.Matched != 2 || r.Ruled != 2 || r.Failed != 1 || r.Skipped != 0 || r.CreatedAt == 0 || r.Reverted {
		t.Errorf("ruling by ids = %+v", r)
	}
	if r := got[byFilter.RulingID]; r.Action != "accept" || r.Selector.IDsCount != 0 || r.Selector.Filter == nil ||
		r.Selector.Filter.FromUnderPath != "/kb" || r.Selector.Max != 5 || r.Ruled != byFilter.Ruled {
		t.Errorf("ruling by filter = %+v", r)
	}
}

// Newest first, a page at a time; the limit is bounded.
func TestListRulings_NewestFirstWithACursor(t *testing.T) {
	d, base, _ := documentFixture(t)
	c := seedReviewCorpus(t, d, base)
	op := reviewOperatorCtx(base, nil)
	var made []string
	for _, id := range []string{c.cites, c.repeals, c.elaborates} {
		made = append(made, ruleReal(t, d, op, connector.RuleStatementsRequest{Action: "accept", IDs: []string{id}}, RuleAsOperator).RulingID)
	}
	first := listRulings(t, d, op, connector.ListRulingsRequest{Limit: 2}, RuleAsOperator)
	if ids := rulingIDs(first); strings.Join(ids, ",") != made[2]+","+made[1] || first.NextCursor == "" {
		t.Fatalf("first page = %v cursor %q, want %v then a cursor", ids, first.NextCursor, []string{made[2], made[1]})
	}
	second := listRulings(t, d, op, connector.ListRulingsRequest{Limit: 2, After: first.NextCursor}, RuleAsOperator)
	if ids := rulingIDs(second); strings.Join(ids, ",") != made[0] || second.NextCursor != "" {
		t.Errorf("second page = %v cursor %q, want [%s] and no cursor", ids, second.NextCursor, made[0])
	}
	if all := listRulings(t, d, op, connector.ListRulingsRequest{}, RuleAsOperator); len(all.Rulings) != 3 || all.NextCursor != "" {
		t.Errorf("default page = %v", rulingIDs(all))
	}
	for _, bad := range []connector.ListRulingsRequest{{Limit: 101}, {Limit: -1}, {After: "not-a-cursor"}} {
		if _, err := d.ListRulings(op, "tenant", bad, RuleAsOperator); connector.StatementReviewCode(err) != "invalid_request" {
			t.Errorf("%+v: %v, want invalid_request", bad, err)
		}
	}
}

// A reverted ruling names the revert that undid it, and the revert is listed
// with what it reverted; a revert is not itself undone.
func TestListRulings_ARevertedRulingNamesItsRevert(t *testing.T) {
	d, base, _ := documentFixture(t)
	c := seedReviewCorpus(t, d, base)
	op := WithRulingActor(reviewOperatorCtx(base, nil), "ops")
	ruling := ruleReal(t, d, op, connector.RuleStatementsRequest{Action: "accept", IDs: []string{c.cites, c.repeals}}, RuleAsOperator)
	rev, err := d.RevertRuling(op, "tenant", connector.RevertRulingRequest{RulingID: ruling.RulingID}, RuleAsOperator)
	if err != nil || rev.RevertID == "" || rev.Reverted != 2 {
		t.Fatalf("revert = %+v %v", rev, err)
	}
	res := listRulings(t, d, op, connector.ListRulingsRequest{}, RuleAsOperator)
	if len(res.Rulings) != 2 {
		t.Fatalf("listing = %+v, want the ruling and its revert", res.Rulings)
	}
	r, v := res.Rulings[1], res.Rulings[0]
	if r.ID != ruling.RulingID || !r.Reverted || r.RevertedBy != rev.RevertID || r.RevertedAt == 0 {
		t.Errorf("the ruling = %+v, want reverted by %s", r, rev.RevertID)
	}
	if v.ID != rev.RevertID || v.Action != "revert" || v.Selector.Reverts != ruling.RulingID || v.Ruled != 2 || v.Failed != 0 ||
		v.Matched != 2 || v.Actor != "ops" || v.Reverted {
		t.Errorf("the revert = %+v", v)
	}
	if _, err := d.RevertRuling(op, "tenant", connector.RevertRulingRequest{RulingID: rev.RevertID}, RuleAsOperator); connector.StatementReviewCode(err) != "invalid_request" {
		t.Errorf("reverting a revert: %v, want invalid_request", err)
	}
}
