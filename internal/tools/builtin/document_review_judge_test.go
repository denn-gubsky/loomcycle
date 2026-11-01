package builtin

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// A judge's ruling (ruled_by judge:<run>) is a machine's verdict, as a review
// policy's is: judged_by judge and no operator mark, for the accept as for the
// reject. Its rejection is no tombstone: a later write of the relation reopens
// it as a proposal, and the verdict it carried — a verdict on the proposal as
// it was — is dropped, so the reopened one is judged afresh.
func TestJudgeRuling_IsAVerdictNotAnOperatorRuling(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, base context.Context) {
		f := newPolicyFixture(t, d, base, reviewOntologyMD)
		key, mscope, err := d.resolveScope(f.op, "tenant")
		if err != nil {
			t.Fatal(err)
		}
		acc := f.propose(f.one, f.notes, "elaborates", `,"method":"extractor:relate","confidence":0.7`)["id"].(string)
		rej := f.propose(f.notes, f.two, "elaborates", `,"method":"extractor:relate","confidence":0.7`)["id"].(string)
		if err := d.exec(f.op, key, `UPDATE chunk_edges SET attributes = ? WHERE id = ?`,
			`{"judge":{"verdict":"reject","confidence":0.95}}`, rej); err != nil {
			t.Fatal(err)
		}
		for _, c := range []struct {
			id     string
			accept bool
			want   string
		}{{acc, true, edgeStatusAsserted}, {rej, false, edgeStatusRejected}} {
			op := "reject_statement"
			if c.accept {
				op = "accept_statement"
			}
			r, err := d.applyRuling(f.op, key, mscope, op, c.id, c.accept, "the judge found it unsupported", "judge:run-judge")
			if err != nil || r.refusal != nil {
				t.Fatalf("%s: %v %v", op, err, r.refusal)
			}
			e := f.edge(c.id)
			if e["status"] != c.want || e["ruled_by"] != "judge:run-judge" || e["judged_by"] != "judge" || e["operator_ruled_at"] != nil {
				t.Errorf("%s by the judge = %v, want %s, judged_by judge, no operator mark", op, e, c.want)
			}
		}
		later, res := docExec(t, d, tools.WithRunID(f.run, "run-later"), fmt.Sprintf(
			`{"op":"propose_statement","scope":"tenant","from_id":%q,"to_id":%q,"kind":"elaborates","method":"extractor:relate","confidence":0.7}`, f.notes, f.two))
		if res.IsError {
			t.Fatalf("a judge's rejection refused a later proposal: %s", res.Text)
		}
		e := f.edge(rej)
		if later["id"] != rej || e["status"] != edgeStatusProposed || e["judged_by"] != nil {
			t.Errorf("the later proposal = %v, statement %v: want the judged one reopened as a proposal", later, e)
		}
		if attrs, _ := e["attributes"].(map[string]any); attrs[attrJudge] != nil {
			t.Errorf("the reopened proposal kept the old verdict: %v", attrs)
		}
	})
}

// judgeOntologyMD routes elaborates to the judge at floor 0.9, repeals at the
// section's 0.85, and supports to the judge for a recommendation only;
// everything else waits for a person.
const judgeOntologyMD = relationsOntologyMD +
	"## Review\n@section review\n" +
	"judge_floor: 0.85\n\n" +
	"- judge kind:elaborates floor:0.9\n" +
	"- judge kind:repeals\n" +
	"- judge kind:supports mode:recommend\n" +
	"- human *\n"

// judgeRunCtx is a run of a code agent granted statement_methods [judge], on
// the tenant scope, bound to eff (nil: no set).
func judgeRunCtx(base context.Context, eff *authz.Effective) context.Context {
	return tools.WithRunPolicy(tools.WithAgentName(tools.WithRunID(base, "run-judge"), "kb/judge"), tools.RunPolicy{
		Memory:           tools.MemoryPolicyValue{AllowedScopes: []string{"tenant"}},
		SqlMem:           tools.SqlMemPolicyValue{AllowedScopes: []string{"tenant"}},
		StatementMethods: []string{"judge"},
		ACL:              eff,
	})
}

// judge calls judge_statements; items are {id, verdict, confidence, rationale}
// objects as JSON text.
func judge(t *testing.T, d *Document, ctx context.Context, write string, items ...string) (map[string]any, tools.Result) {
	t.Helper()
	w := ""
	if write != "" {
		w = fmt.Sprintf(`,"write":%q`, write)
	}
	return docExec(t, d, ctx, fmt.Sprintf(`{"op":"judge_statements","scope":"tenant"%s,"judgements":[%s]}`, w, strings.Join(items, ",")))
}

func judgement(id, v string, conf float64) string {
	return fmt.Sprintf(`{"id":%q,"verdict":%q,"confidence":%v,"rationale":"the quotes show it"}`, id, v, conf)
}

// judgeItems indexes a judge_statements result's items by id.
func judgeItems(t *testing.T, out map[string]any) map[string]map[string]any {
	t.Helper()
	items := map[string]map[string]any{}
	list, _ := out["items"].([]any)
	for _, it := range list {
		m := it.(map[string]any)
		items[m["id"].(string)] = m
	}
	return items
}

func judgeAttr(e map[string]any) map[string]any {
	attrs, _ := e["attributes"].(map[string]any)
	j, _ := attrs[attrJudge].(map[string]any)
	return j
}

// Only a run whose agent holds statement_methods [judge] records verdicts: a
// linker's run (rule, extractor) and the operator plane are refused, and
// nothing about the statement changes.
func TestJudgeStatements_TheGrantIsRequired(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, base context.Context) {
		f := newPolicyFixture(t, d, base, judgeOntologyMD)
		id := f.propose(f.one, f.notes, "elaborates", `,"method":"extractor:relate"`)["id"].(string)
		for name, ctx := range map[string]context.Context{
			"a linker's run":     f.run,
			"the operator plane": f.op,
			"a run with no grant": tools.WithRunPolicy(tools.WithRunID(base, "run-x"), tools.RunPolicy{
				Memory: tools.MemoryPolicyValue{AllowedScopes: []string{"tenant"}}, SqlMem: tools.SqlMemPolicyValue{AllowedScopes: []string{"tenant"}}}),
		} {
			_, r := judge(t, d, ctx, "", judgement(id, "accept", 0.99))
			if !r.IsError || !strings.Contains(r.Text, "permission") {
				t.Errorf("%s: judge_statements = %s, want a permission refusal", name, r.Text)
			}
		}
		if e := f.edge(id); e["status"] != edgeStatusProposed || judgeAttr(e) != nil {
			t.Errorf("a refused call changed the statement: %v", e)
		}
	})
}

// A verdict at or above the routing rule's floor rules — asserted or rejected,
// ruled_by judge:<run>, judged_by judge, no operator mark — and every verdict
// is recorded on the statement. Below the floor, or unsure, the statement is
// only annotated and stays proposed for a person. The floor is the rule's (or
// the section's); a floor in the call changes nothing.
func TestJudgeStatements_RulesAtTheFloorAndEscalatesBelowIt(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, base context.Context) {
		f := newPolicyFixture(t, d, base, judgeOntologyMD)
		accept := f.propose(f.one, f.notes, "elaborates", `,"method":"extractor:relate"`)["id"].(string)
		below := f.propose(f.oneB, f.notes, "elaborates", `,"method":"extractor:relate"`)["id"].(string)
		unsure := f.propose(f.two, f.notes, "elaborates", `,"method":"extractor:relate"`)["id"].(string)
		reject := f.propose(f.one, f.two, "repeals", `,"method":"extractor:relate"`)["id"].(string)
		section := f.propose(f.oneB, f.two, "repeals", `,"method":"extractor:relate"`)["id"].(string)
		if e := f.edge(accept); e["attributes"].(map[string]any)[attrReviewRoute] != reviewRouteJudge {
			t.Fatalf("the policy did not route the proposal to the judge: %v", e)
		}
		run := judgeRunCtx(base, nil)
		out, r := docExec(t, d, run, fmt.Sprintf(`{"op":"judge_statements","scope":"tenant","floor":0.5,"judgements":[%s]}`, strings.Join([]string{
			judgement(accept, "accept", 0.95), judgement(below, "accept", 0.89), judgement(unsure, "unsure", 0.99),
			judgement(reject, "reject", 0.9), judgement(section, "accept", 0.86)}, ",")))
		if r.IsError {
			t.Fatal(r.Text)
		}
		items := judgeItems(t, out)
		for _, c := range []struct {
			id, status, outcome string
		}{
			{accept, edgeStatusAsserted, judgeOutcomeRuled},
			{below, edgeStatusProposed, judgeOutcomeBelowFloor},
			{unsure, edgeStatusProposed, judgeOutcomeUnsure},
			{reject, edgeStatusRejected, judgeOutcomeRuled},
			{section, edgeStatusAsserted, judgeOutcomeRuled},
		} {
			e := f.edge(c.id)
			if e["status"] != c.status || items[c.id]["outcome"] != c.outcome {
				t.Errorf("%s: status %v outcome %v, want %s / %s", c.id, e["status"], items[c.id]["outcome"], c.status, c.outcome)
			}
			j := judgeAttr(e)
			if j["run_id"] != "run-judge" || j["rationale"] != "the quotes show it" || j["outcome"] != c.outcome || j["verdict"] == nil || j["confidence"] == nil {
				t.Errorf("%s: attributes.judge = %v", c.id, j)
			}
			if c.outcome == judgeOutcomeRuled {
				if e["ruled_by"] != "judge:run-judge" || e["judged_by"] != "judge" || e["operator_ruled_at"] != nil || e["ruling_id"] != out["ruling_id"] {
					t.Errorf("%s: ruled = %v, want ruled_by judge:run-judge under ruling %v, no operator mark", c.id, e, out["ruling_id"])
				}
			} else if e["ruled_by"] != nil {
				t.Errorf("%s: escalated, but ruled_by %v", c.id, e["ruled_by"])
			}
		}
		if j := judgeAttr(f.edge(below)); j["floor"] != 0.9 {
			t.Errorf("the floor recorded = %v, want the routing rule's 0.9", j["floor"])
		}
		if out["ruled"] != float64(3) || out["accepted"] != float64(2) || out["rejected"] != float64(1) || out["escalated"] != float64(2) || out["judged"] != float64(5) {
			t.Errorf("counts = %v", out)
		}
	})
}

// The op, not the agent, decides what may be ruled: a proposal the policy left
// for a person is only annotated however confident the verdict; a statement an
// operator ruled, one no longer proposed, one already judged, and one the run
// may not read are refused.
func TestJudgeStatements_RulesOnlyWhatThePolicyRoutedToIt(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, base context.Context) {
		f := newPolicyFixture(t, d, base, judgeOntologyMD)
		human := f.propose(f.one, f.two, "governs", `,"method":"extractor:relate"`)["id"].(string)
		operatorRuled := f.propose(f.one, f.notes, "elaborates", `,"method":"extractor:relate"`)["id"].(string)
		key := mustTenantKey(t, d, f.op)
		if err := d.exec(f.op, key, `UPDATE chunk_edges SET operator_ruled_at = 1 WHERE id = ?`, operatorRuled); err != nil {
			t.Fatal(err)
		}
		linked, r := docExec(t, d, f.op, fmt.Sprintf(`{"op":"link_chunks","scope":"tenant","from_id":%q,"to_id":%q,"kind":"elaborates"}`, f.oneB, f.notes))
		if r.IsError {
			t.Fatal(r.Text)
		}
		asserted := linked["id"].(string)
		// Routing needs both the mark the policy left and the policy deciding
		// judge now: a stale mark on a proposal the policy leaves for a person
		// rules nothing, and nor does a routed kind whose mark is gone.
		stale := f.propose(f.oneB, f.two, "governs", `,"method":"extractor:relate"`)["id"].(string)
		unmarked := f.propose(f.two, f.notes, "elaborates", `,"method":"extractor:relate"`)["id"].(string)
		if err := d.exec(f.op, key, `UPDATE chunk_edges SET attributes = '{"review_route":"judge"}' WHERE id = ?`, stale); err != nil {
			t.Fatal(err)
		}
		if err := d.exec(f.op, key, `UPDATE chunk_edges SET attributes = NULL WHERE id = ?`, unmarked); err != nil {
			t.Fatal(err)
		}
		run := judgeRunCtx(base, nil)
		out, r := judge(t, d, run, "", judgement(human, "accept", 0.99), judgement(operatorRuled, "reject", 0.99),
			judgement(asserted, "reject", 0.99), judgement("no-such-statement", "accept", 0.99),
			judgement(stale, "accept", 0.99), judgement(unmarked, "accept", 0.99))
		if r.IsError {
			t.Fatal(r.Text)
		}
		items := judgeItems(t, out)
		for _, id := range []string{human, stale, unmarked} {
			if items[id]["outcome"] != judgeOutcomeNotRouted || f.edge(id)["status"] != edgeStatusProposed {
				t.Errorf("an unrouted proposal: %v / %v", items[id], f.edge(id))
			}
		}
		for id, code := range map[string]string{operatorRuled: "operator_ruled", asserted: rulingNotProposal, "no-such-statement": rulingNotFound} {
			if items[id]["outcome"] != judgeOutcomeRefused || items[id]["error"] != code {
				t.Errorf("%s: item %v, want refused %s", id, items[id], code)
			}
		}
		if e := f.edge(operatorRuled); e["status"] != edgeStatusProposed || judgeAttr(e) != nil {
			t.Errorf("an operator-ruled statement was touched: %v", e)
		}
		if out["ruling_id"] != nil {
			t.Errorf("nothing was ruled, but a ruling was recorded: %v", out)
		}
		// Judged once, never again: a second verdict is refused, so a run cannot
		// ask until a verdict clears the floor.
		out, _ = judge(t, d, run, "", judgement(human, "accept", 0.99))
		if it := judgeItems(t, out)[human]; it["error"] != "already_judged" {
			t.Errorf("a second verdict on %s: %v", human, it)
		}

		// A statement the run may not read is answered as one that does not exist.
		hidden := f.propose(f.notes, f.two, "elaborates", `,"method":"extractor:relate"`)["id"].(string)
		denied := judgeRunCtx(base, aclSet(t,
			authz.Rule{Effect: authz.Allow, Actions: []string{"read", "write", "link", "propose"}, Resources: []string{"document:tenant"}},
			authz.Rule{Effect: authz.Deny, Actions: []string{"read"}, Resources: []string{"document:tenant:/kb/**"}}))
		out, r = judge(t, d, denied, "", judgement(hidden, "accept", 0.99))
		if r.IsError {
			t.Fatal(r.Text)
		}
		if it := judgeItems(t, out)[hidden]; it["error"] != rulingNotFound {
			t.Errorf("an unreadable statement: %v", it)
		}
		if e := f.edge(hidden); e["status"] != edgeStatusProposed || judgeAttr(e) != nil {
			t.Errorf("an unreadable statement was touched: %v", e)
		}
		// Where the run may read but not write, as an operator's ruling needs,
		// the verdict is recorded and the statement waits for a person.
		readOnly := judgeRunCtx(base, aclSet(t,
			authz.Rule{Effect: authz.Allow, Actions: []string{"read", "write", "link", "propose"}, Resources: []string{"document:tenant"}},
			authz.Rule{Effect: authz.Deny, Actions: []string{"write"}, Resources: []string{"document:tenant:/kb/**"}}))
		out, r = judge(t, d, readOnly, "", judgement(hidden, "accept", 0.99))
		if r.IsError {
			t.Fatal(r.Text)
		}
		if it, e := judgeItems(t, out)[hidden], f.edge(hidden); it["outcome"] != judgeOutcomeNotPermitted || e["status"] != edgeStatusProposed || judgeAttr(e) == nil {
			t.Errorf("a statement the run may not write: item %v, statement %v", it, e)
		}
	})
}

// recommend — the call's write mode, or the routing rule's mode — records the
// verdict and rules nothing.
func TestJudgeStatements_RecommendWritesNoRuling(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, base context.Context) {
		f := newPolicyFixture(t, d, base, judgeOntologyMD)
		byCall := f.propose(f.one, f.notes, "elaborates", `,"method":"extractor:relate"`)["id"].(string)
		byRule := f.propose(f.one, f.two, "supports", `,"method":"extractor:relate"`)["id"].(string)
		run := judgeRunCtx(base, nil)
		out, r := judge(t, d, run, "recommend", judgement(byCall, "accept", 0.99))
		if r.IsError {
			t.Fatal(r.Text)
		}
		out2, r := judge(t, d, run, "", judgement(byRule, "reject", 0.99))
		if r.IsError {
			t.Fatal(r.Text)
		}
		for id, o := range map[string]map[string]any{byCall: out, byRule: out2} {
			e := f.edge(id)
			if e["status"] != edgeStatusProposed || e["ruled_by"] != nil || o["ruling_id"] != nil || judgeAttr(e)["outcome"] != judgeOutcomeRecommend {
				t.Errorf("%s: %v (result %v), want a recommendation and no ruling", id, e, o)
			}
		}
		key := mustTenantKey(t, d, f.op)
		res, err := d.query(f.op, key, `SELECT COUNT(*) FROM statement_rulings`)
		if err != nil || asInt(res.Rows[0][0]) != 0 {
			t.Errorf("a recommendation recorded a ruling: %v %v", res, err)
		}
	})
}

// Every ruling a judge run makes, over all its calls, is one ruling: listed
// once, and undone together by revert_ruling. The verdicts stay on the
// statements, so a later run does not rule them again.
func TestJudgeStatements_TheRunRevertsAsOneRuling(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, base context.Context) {
		f := newPolicyFixture(t, d, base, judgeOntologyMD)
		a := f.propose(f.one, f.notes, "elaborates", `,"method":"extractor:relate"`)["id"].(string)
		b := f.propose(f.oneB, f.notes, "elaborates", `,"method":"extractor:relate"`)["id"].(string)
		c := f.propose(f.one, f.two, "repeals", `,"method":"extractor:relate"`)["id"].(string)
		run := judgeRunCtx(base, nil)
		first, _ := judge(t, d, run, "", judgement(a, "accept", 0.95), judgement(b, "accept", 0.97))
		second, _ := judge(t, d, run, "", judgement(c, "reject", 0.9))
		if first["ruling_id"] == nil || first["ruling_id"] != second["ruling_id"] {
			t.Fatalf("two calls of one run: rulings %v and %v, want one", first["ruling_id"], second["ruling_id"])
		}
		rulingID := first["ruling_id"].(string)
		list, err := d.ListRulings(f.op, "tenant", connector.ListRulingsRequest{}, RuleAsOperator)
		if err != nil || len(list.Rulings) != 1 {
			t.Fatalf("rulings = %+v %v", list, err)
		}
		if r := list.Rulings[0]; r.ID != rulingID || r.Action != rulingActionJudge || r.RuledBy != "judge:run-judge" || r.Ruled != 3 {
			t.Errorf("the judge's ruling = %+v", r)
		}
		rev, err := d.RevertRuling(f.op, "tenant", connector.RevertRulingRequest{RulingID: rulingID}, RuleAsOperator)
		if err != nil || rev.Reverted != 3 || rev.Changed != 0 {
			t.Fatalf("revert = %+v %v", rev, err)
		}
		for _, id := range []string{a, b, c} {
			e := f.edge(id)
			if e["status"] != edgeStatusProposed || e["ruled_by"] != nil || judgeAttr(e) == nil {
				t.Errorf("%s after the revert = %v, want proposed with its verdict kept", id, e)
			}
		}
		out, _ := judge(t, d, tools.WithRunID(run, "run-judge-2"), "", judgement(a, "accept", 0.99))
		if it := judgeItems(t, out)[a]; it["error"] != "already_judged" {
			t.Errorf("a later run re-ruled a reverted statement: %v", it)
		}
	})
}

// Each judge run marks a random share of its acceptances for a person's audit
// (attributes.judge.audited pending): the policy's judge_audit, and at least
// one when it accepted anything. A rejection is never sampled. The mark is the
// run's own write, so the run still reverts as a whole.
func TestJudgeStatements_SamplesItsAcceptancesForAudit(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, base context.Context) {
		f := newPolicyFixture(t, d, base, judgeOntologyMD)
		a := f.propose(f.one, f.notes, "elaborates", `,"method":"extractor:relate"`)["id"].(string)
		b := f.propose(f.oneB, f.notes, "elaborates", `,"method":"extractor:relate"`)["id"].(string)
		r := f.propose(f.one, f.two, "repeals", `,"method":"extractor:relate"`)["id"].(string)
		out, res := judge(t, d, judgeRunCtx(base, nil), "", judgement(a, "accept", 0.95), judgement(b, "accept", 0.95), judgement(r, "reject", 0.95))
		if res.IsError {
			t.Fatal(res.Text)
		}
		if n, _ := out["audit_marked"].(float64); n < 1 {
			t.Fatalf("a run that accepted two marked %v for audit, want at least one", out["audit_marked"])
		}
		items := judgeItems(t, out)
		for _, id := range []string{a, b} {
			marked := items[id]["audit"] == judgeAuditPending
			if got := judgeAttr(f.edge(id))["audited"]; marked != (got == judgeAuditPending) {
				t.Errorf("%s: item audit %v, attributes.judge.audited %v", id, items[id]["audit"], got)
			}
		}
		if items[r]["audit"] != nil || judgeAttr(f.edge(r))["audited"] != nil {
			t.Errorf("a rejection was sampled: %v", items[r])
		}
		rev, err := d.RevertRuling(f.op, "tenant", connector.RevertRulingRequest{RulingID: out["ruling_id"].(string)}, RuleAsOperator)
		if err != nil || rev.Reverted != 3 {
			t.Fatalf("revert after the audit mark = %+v %v, want all three reverted", rev, err)
		}
	})
}

// judge_audit: 1 samples every acceptance.
func TestJudgeStatements_AuditRateOneSamplesEveryAcceptance(t *testing.T) {
	d, base, _ := documentFixture(t)
	md := strings.Replace(judgeOntologyMD, "judge_floor: 0.85\n", "judge_floor: 0.85\njudge_audit: 1\n", 1)
	f := newPolicyFixture(t, d, base, md)
	var items []string
	var ids []string
	for _, from := range []string{f.one, f.oneB, f.two} {
		id := f.propose(from, f.notes, "elaborates", `,"method":"extractor:relate"`)["id"].(string)
		ids = append(ids, id)
		items = append(items, judgement(id, "accept", 0.95))
	}
	out, res := judge(t, d, judgeRunCtx(base, nil), "", items...)
	if res.IsError {
		t.Fatal(res.Text)
	}
	if out["audit_marked"] != float64(3) {
		t.Errorf("audit_marked = %v, want 3", out["audit_marked"])
	}
	for _, id := range ids {
		if judgeAttr(f.edge(id))["audited"] != judgeAuditPending {
			t.Errorf("%s not marked: %v", id, f.edge(id))
		}
	}
}

// The queue selects the proposals the policy routed to the judge
// (review_route), and the judge's acceptances sampled for audit (judge_audit,
// asked of asserted statements), on list_statements and the review queue.
func TestReviewQueue_SelectsByRoutingAndAudit(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, base context.Context) {
		md := strings.Replace(judgeOntologyMD, "judge_floor: 0.85\n", "judge_floor: 0.85\njudge_audit: 1\n", 1)
		f := newPolicyFixture(t, d, base, md)
		routed := f.propose(f.one, f.notes, "elaborates", `,"method":"extractor:relate"`)["id"].(string)
		human := f.propose(f.one, f.two, "governs", `,"method":"extractor:relate"`)["id"].(string)
		ids := func(body string) []string {
			t.Helper()
			var out []string
			for _, e := range edgesOfKey(t, d, f.op, body, "statements") {
				out = append(out, e["id"].(string))
			}
			return out
		}
		if got := ids(`{"op":"list_statements","scope":"tenant","review_route":"judge"}`); len(got) != 1 || got[0] != routed {
			t.Errorf("review_route judge = %v, want [%s] (not %s)", got, routed, human)
		}
		q, err := d.ListReviewQueue(f.op, "tenant", connector.ListReviewQueueRequest{Filter: connector.ReviewQueueFilter{ReviewRoute: "judge"}})
		if err != nil || len(q.Statements) != 1 {
			t.Errorf("the review queue by routing = %d statements, %v", len(q.Statements), err)
		}
		if _, r := judge(t, d, judgeRunCtx(base, nil), "", judgement(routed, "accept", 0.95)); r.IsError {
			t.Fatal(r.Text)
		}
		if got := ids(`{"op":"list_statements","scope":"tenant","status":"asserted","judge_audit":"pending"}`); len(got) != 1 || got[0] != routed {
			t.Errorf("judge_audit pending = %v, want [%s]", got, routed)
		}
		if got := ids(`{"op":"list_statements","scope":"tenant","status":"asserted","judge_audit":"confirmed"}`); len(got) != 0 {
			t.Errorf("judge_audit confirmed = %v, want none yet", got)
		}
		q, err = d.ListReviewQueue(f.op, "tenant", connector.ListReviewQueueRequest{Filter: connector.ReviewQueueFilter{Status: "asserted", JudgeAudit: "pending"}})
		if err != nil || len(q.Statements) != 1 {
			t.Errorf("the review queue by audit = %d statements, %v", len(q.Statements), err)
		}
		if _, r := docExec(t, d, f.op, `{"op":"list_statements","scope":"tenant","judge_audit":"maybe"}`); !r.IsError {
			t.Errorf("an unknown judge_audit value was accepted")
		}
	})
}

// edgesOfKey runs a listing op and returns the rows under field.
func edgesOfKey(t *testing.T, d *Document, ctx context.Context, body, field string) []map[string]any {
	t.Helper()
	out, r := docExec(t, d, ctx, body)
	if r.IsError {
		t.Fatalf("%s: %s", body, r.Text)
	}
	raw, _ := out[field].([]any)
	res := make([]map[string]any, 0, len(raw))
	for _, e := range raw {
		res = append(res, e.(map[string]any))
	}
	return res
}

// A person settles a judge's acceptance through the ordinary rulings: accept
// confirms it (it stays asserted, now a person's: operator mark, ruled_by
// operator) and reject overturns it (rejected, a person's tombstone). The
// outcome is recorded on the verdict (attributes.judge.audited confirmed or
// overturned, with who and when), so the audit sample can be counted. A bulk
// confirmation reverts to the pending sample. Nothing else that is asserted
// becomes rulable.
func TestJudgeAudit_APersonConfirmsOrOverturnsAnAcceptance(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, base context.Context) {
		md := strings.Replace(judgeOntologyMD, "judge_floor: 0.85\n", "judge_floor: 0.85\njudge_audit: 1\n", 1)
		f := newPolicyFixture(t, d, base, md)
		a := f.propose(f.one, f.notes, "elaborates", `,"method":"extractor:relate"`)["id"].(string)
		b := f.propose(f.oneB, f.notes, "elaborates", `,"method":"extractor:relate"`)["id"].(string)
		c := f.propose(f.two, f.notes, "elaborates", `,"method":"extractor:relate"`)["id"].(string)
		if _, r := judge(t, d, judgeRunCtx(base, nil), "", judgement(a, "accept", 0.95), judgement(b, "accept", 0.95), judgement(c, "accept", 0.95)); r.IsError {
			t.Fatal(r.Text)
		}
		// A machine never settles one: not a policy, not another judge run.
		key, mscope, err := d.resolveScope(f.op, "tenant")
		if err != nil {
			t.Fatal(err)
		}
		for _, by := range []string{"judge:run-judge-2", "policy:1@3"} {
			if r, err := d.applyRuling(f.op, key, mscope, "reject_statement", a, false, "machine", by); err != nil || r.code != rulingNotProposal {
				t.Errorf("%s ruling on a judge's acceptance = %+v %v, want not_a_proposal", by, r, err)
			}
		}
		op := operatorCtx(f.op)
		if _, r := docExec(t, d, op, fmt.Sprintf(`{"op":"accept_statement","scope":"tenant","id":%q}`, a)); r.IsError {
			t.Fatalf("confirm: %s", r.Text)
		}
		if _, r := docExec(t, d, op, fmt.Sprintf(`{"op":"reject_statement","scope":"tenant","id":%q,"reason":"the quote is about another article"}`, b)); r.IsError {
			t.Fatalf("overturn: %s", r.Text)
		}
		ea, eb := f.edge(a), f.edge(b)
		if ea["status"] != edgeStatusAsserted || ea["operator_ruled_at"] == nil || !strings.HasPrefix(ea["ruled_by"].(string), "operator") ||
			judgeAttr(ea)["audited"] != judgeAuditConfirmed || judgeAttr(ea)["audited_by"] == nil || judgeAttr(ea)["audited_at"] == nil {
			t.Errorf("confirmed = %v", ea)
		}
		if eb["status"] != edgeStatusRejected || eb["operator_ruled_at"] == nil || eb["judged_by"] != "operator" || judgeAttr(eb)["audited"] != judgeAuditOverturned {
			t.Errorf("overturned = %v", eb)
		}
		again := fmt.Sprintf(`{"op":"propose_statement","scope":"tenant","from_id":%q,"to_id":%q,"kind":"elaborates","method":"extractor:relate"}`, f.oneB, f.notes)
		if _, r := docExec(t, d, f.run, again); !r.IsError || !strings.Contains(r.Text, "statement_rejected") {
			t.Errorf("an overturned acceptance is a person's rejection, a tombstone: %s", r.Text)
		}

		// The pending sample, confirmed in bulk, and the confirmation undone.
		req := connector.RuleStatementsRequest{Action: "accept", Filter: &connector.ReviewQueueFilter{Status: edgeStatusAsserted, JudgeAudit: judgeAuditPending}}
		res := ruleReal(t, d, op, req, RuleAsOperator)
		if res.Ruled != 1 || judgeAttr(f.edge(c))["audited"] != judgeAuditConfirmed {
			t.Fatalf("bulk confirmation = %+v, statement %v", res, f.edge(c))
		}
		rev, err := d.RevertRuling(op, "tenant", connector.RevertRulingRequest{RulingID: res.RulingID}, RuleAsOperator)
		if err != nil || rev.Reverted != 1 {
			t.Fatalf("revert = %+v %v", rev, err)
		}
		if ec := f.edge(c); ec["ruled_by"] != "judge:run-judge" || ec["operator_ruled_at"] != nil || judgeAttr(ec)["audited"] != judgeAuditPending {
			t.Errorf("after the revert %v, want the judge's pending acceptance back", ec)
		}

		// An assertion no judge made is not a ruling's to change.
		linked, r := docExec(t, d, f.op, fmt.Sprintf(`{"op":"link_chunks","scope":"tenant","from_id":%q,"to_id":%q,"kind":"elaborates"}`, f.two, f.one))
		if r.IsError {
			t.Fatal(r.Text)
		}
		if _, r := docExec(t, d, op, fmt.Sprintf(`{"op":"reject_statement","scope":"tenant","id":%q,"reason":"no"}`, linked["id"])); !r.IsError || !strings.Contains(r.Text, "not_a_proposal") {
			t.Errorf("rejecting an assertion no judge made: %s", r.Text)
		}
	})
}
