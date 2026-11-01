package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// The review queue at scale: list_statements' review filters and summary, bulk
// rulings and their undo.

// reviewWriterCtx is a code agent's run on the tenant scope, granted rule and
// extractor methods, with no set.
func reviewWriterCtx(base context.Context) context.Context {
	return tools.WithRunPolicy(tools.WithRunID(base, "run-linker"), tools.RunPolicy{
		Memory:           tools.MemoryPolicyValue{AllowedScopes: []string{"tenant"}},
		SqlMem:           tools.SqlMemPolicyValue{AllowedScopes: []string{"tenant"}},
		StatementMethods: []string{"rule", "extractor"},
	})
}

// reviewOperatorCtx is the off-run operator plane on the tenant scope, bound to
// eff (nil: no set, as open mode has).
func reviewOperatorCtx(base context.Context, eff *authz.Effective) context.Context {
	return tools.WithRunPolicy(base, tools.RunPolicy{
		Tools:  []string{"*"},
		Memory: tools.MemoryPolicyValue{AllowedScopes: []string{"tenant"}},
		SqlMem: tools.SqlMemPolicyValue{AllowedScopes: []string{"tenant"}},
		ACL:    eff,
	})
}

// reviewCorpus is a tenant with three documents and the proposals between them.
type reviewCorpus struct {
	board1, board2, notes              string // documents
	b1, b2, n1                         string // one chunk in each
	cites, repeals, elaborates, judged string // statement ids
}

// seedReviewCorpus files: two rule:citation cites from /board/reg/one (one to
// /board/reg/two, a repeals between them too), an extractor elaborates from
// /kb/notes/n, and a cites carrying a judge's verdict.
func seedReviewCorpus(t *testing.T, d *Document, base context.Context) reviewCorpus {
	t.Helper()
	op := reviewOperatorCtx(base, nil)
	var c reviewCorpus
	mk := func(path, title, body string) (string, string) {
		out, res := docExec(t, d, op, fmt.Sprintf(`{"op":"create_document","scope":"tenant","title":%q,"path":%q}`, title, path))
		if res.IsError {
			t.Fatalf("create_document %s: %s", path, res.Text)
		}
		doc := out["document_id"].(string)
		out, res = docExec(t, d, op, fmt.Sprintf(`{"op":"create_chunk","scope":"tenant","document_id":%q,"title":%q,"body":%q}`, doc, title+" part", body))
		if res.IsError {
			t.Fatalf("create_chunk %s: %s", path, res.Text)
		}
		return doc, out["id"].(string)
	}
	c.board1, c.b1 = mk("/board/reg/one", "One", "Article 4 cites the Act of 2015.")
	c.board2, c.b2 = mk("/board/reg/two", "Two", "The Act of 2015 sets the duty.")
	c.notes, c.n1 = mk("/kb/notes/n", "Notes", "These notes elaborate the duty.")
	w := reviewWriterCtx(base)
	propose := func(body string) string {
		out, res := docExec(t, d, w, body)
		if res.IsError || out["status"] != edgeStatusProposed {
			t.Fatalf("propose %s: %v %s", body, out, res.Text)
		}
		return out["id"].(string)
	}
	c.cites = propose(fmt.Sprintf(`{"op":"propose_statement","scope":"tenant","from_id":%q,"to_id":%q,"kind":"cites","method":"rule:citation","confidence":0.9,
		"evidence":[{"chunk_id":%q,"quote":"cites the Act of 2015"}]}`, c.b1, c.b2, c.b1))
	c.repeals = propose(fmt.Sprintf(`{"op":"propose_statement","scope":"tenant","from_id":%q,"to_id":%q,"kind":"repeals","method":"rule:citation","confidence":0.8}`, c.b1, c.b2))
	c.elaborates = propose(fmt.Sprintf(`{"op":"propose_statement","scope":"tenant","from_id":%q,"to_id":%q,"kind":"elaborates","method":"extractor:relate","confidence":0.4}`, c.n1, c.b2))
	c.judged = propose(fmt.Sprintf(`{"op":"propose_statement","scope":"tenant","from_id":%q,"to_id":%q,"kind":"cites","method":"rule:citation","confidence":0.95}`, c.n1, c.b1))
	// attributes.judge is the runtime's to write (a writer is refused it), so
	// the verdict is set in the store, as the judge's own write would.
	key, _, err := d.resolveScope(op, "tenant")
	if err == nil {
		err = d.exec(op, key, `UPDATE chunk_edges SET attributes = ? WHERE id = ?`, `{"judge":{"verdict":"accept","confidence":0.97}}`, c.judged)
	}
	if err != nil {
		t.Fatalf("record the judge's verdict: %v", err)
	}
	return c
}

func listIDs(t *testing.T, d *Document, ctx context.Context, body string) map[string]bool {
	t.Helper()
	out, res := docExec(t, d, ctx, body)
	if res.IsError {
		t.Fatalf("%s: %s", body, res.Text)
	}
	ids := map[string]bool{}
	raw, _ := out["statements"].([]any)
	for _, s := range raw {
		ids[s.(map[string]any)["id"].(string)] = true
	}
	return ids
}

func TestListStatements_ReviewFiltersSelectByEndWriterConfidenceTimeAndVerdict(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, base context.Context) {
		c := seedReviewCorpus(t, d, base)
		op := reviewOperatorCtx(base, nil)
		future := time.Now().Add(time.Hour).UnixNano()
		for _, tc := range []struct {
			name, filter string
			want         []string
		}{
			{"from_document_id", `"from_document_id":"` + c.board1 + `"`, []string{c.cites, c.repeals}},
			{"to_document_id", `"to_document_id":"` + c.board1 + `"`, []string{c.judged}},
			{"from_under_path", `"from_under_path":"/board"`, []string{c.cites, c.repeals}},
			{"to_under_path", `"to_under_path":"/board/reg/two"`, []string{c.cites, c.repeals, c.elaborates}},
			// In a run the writer is the agent.
			{"actor", `"actor":"` + tools.AgentName(base) + `"`, []string{c.cites, c.repeals, c.elaborates, c.judged}},
			{"actor other", `"actor":"someone-else"`, nil},
			{"min_confidence", `"min_confidence":0.85`, []string{c.cites, c.judged}},
			{"created_before future", fmt.Sprintf(`"created_before":%d`, future), []string{c.cites, c.repeals, c.elaborates, c.judged}},
			{"created_after future", fmt.Sprintf(`"created_after":%d`, future), nil},
			{"has_judge_verdict", `"has_judge_verdict":true`, []string{c.judged}},
			{"no judge verdict", `"has_judge_verdict":false`, []string{c.cites, c.repeals, c.elaborates}},
			{"judge_verdict", `"judge_verdict":"accept"`, []string{c.judged}},
			{"judge_verdict other", `"judge_verdict":"reject"`, nil},
			{"combined", `"from_under_path":"/board","kind":"cites","method_prefix":"rule:"`, []string{c.cites}},
		} {
			got := listIDs(t, d, op, `{"op":"list_statements","scope":"tenant",`+tc.filter+`}`)
			if len(got) != len(tc.want) {
				t.Errorf("%s: got %d statements %v, want %v", tc.name, len(got), got, tc.want)
				continue
			}
			for _, id := range tc.want {
				if !got[id] {
					t.Errorf("%s: %s missing from %v", tc.name, id, got)
				}
			}
		}
		for _, bad := range []string{`"min_confidence":1.5`, `"actor":"` + strings.Repeat("x", 300) + `"`, `"from_under_path":"no-slash"`} {
			if _, res := docExec(t, d, op, `{"op":"list_statements","scope":"tenant",`+bad+`}`); !res.IsError {
				t.Errorf("%s was accepted", bad)
			}
		}
	})
}

func TestListStatements_SummarizeCountsByKindMethodPathPrefixAndVerdict(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, base context.Context) {
		c := seedReviewCorpus(t, d, base)
		op := reviewOperatorCtx(base, nil)
		out, res := docExec(t, d, op, `{"op":"list_statements","scope":"tenant","summarize":true}`)
		if res.IsError {
			t.Fatal(res.Text)
		}
		if out["total"] != float64(4) || out["statements"] != nil {
			t.Errorf("summary = %v, want total 4 and no rows", out)
		}
		got := map[string]float64{}
		for _, g := range out["summary"].([]any) {
			m := g.(map[string]any)
			got[fmt.Sprint(m["kind"], "|", m["method"], "|", m["from_path_prefix"], "|", m["judge_verdict"])] = m["count"].(float64)
		}
		want := map[string]float64{
			"cites|rule:citation|/board/reg|":        1,
			"repeals|rule:citation|/board/reg|":      1,
			"elaborates|extractor:relate|/kb/notes|": 1,
			"cites|rule:citation|/kb/notes|accept":   1,
		}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("summary groups = %v, want %v", got, want)
		}
		// The filters apply to the summary.
		out, _ = docExec(t, d, op, `{"op":"list_statements","scope":"tenant","summarize":true,"from_document_id":"`+c.board1+`"}`)
		if out["total"] != float64(2) {
			t.Errorf("filtered summary total = %v, want 2", out["total"])
		}
	})
}

func TestPathPrefix_KeepsTheFirstTwoSegmentsOfTheFirstName(t *testing.T) {
	for _, tc := range []struct {
		names []string
		want  string
	}{
		{nil, ""},
		{[]string{"/board/regulatory-external/act-1"}, "/board/regulatory-external"},
		{[]string{"/kb/report"}, "/kb/report"},
		{[]string{"/z/a/b", "/a/b/c"}, "/a/b"},
	} {
		if got := pathPrefix(tc.names); got != tc.want {
			t.Errorf("pathPrefix(%v) = %q, want %q", tc.names, got, tc.want)
		}
	}
}

func tenantEdge(t *testing.T, d *Document, ctx context.Context, id string) map[string]any {
	t.Helper()
	out, res := docExec(t, d, ctx, `{"op":"get_edge","scope":"tenant","id":"`+id+`"}`)
	if res.IsError {
		t.Fatalf("get_edge %s: %s", id, res.Text)
	}
	return out["statement"].(map[string]any)
}

func boolPtr(b bool) *bool { return &b }
func intPtr(n int) *int    { return &n }

func ruleDry(t *testing.T, d *Document, ctx context.Context, req connector.RuleStatementsRequest, as RulingAuthority) connector.RuleStatementsResult {
	t.Helper()
	req.DryRun = nil
	res, err := d.RuleStatements(ctx, "tenant", req, as)
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	return res
}

func ruleReal(t *testing.T, d *Document, ctx context.Context, req connector.RuleStatementsRequest, as RulingAuthority) connector.RuleStatementsResult {
	t.Helper()
	dry := ruleDry(t, d, ctx, req, as)
	req.DryRun, req.ExpectCount = boolPtr(false), intPtr(dry.Matched)
	res, err := d.RuleStatements(ctx, "tenant", req, as)
	if err != nil {
		t.Fatalf("rule: %v", err)
	}
	return res
}

func TestRuleStatements_DryRunMatchesWhatTheRealCallRules(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, base context.Context) {
		c := seedReviewCorpus(t, d, base)
		op := reviewOperatorCtx(base, nil)
		req := connector.RuleStatementsRequest{Action: "accept", Filter: &connector.ReviewQueueFilter{Method: "rule:citation", FromUnderPath: "/board"}}
		dry := ruleDry(t, d, op, req, RuleAsOperator)
		if !dry.DryRun || dry.Matched != 2 || dry.ToRule != 2 || dry.RulingID != "" || len(dry.Sample) != 2 {
			t.Fatalf("dry run = %+v", dry)
		}
		if fmt.Sprint(dry.Breakdown) != fmt.Sprint([]connector.RulingBreakdown{{Kind: "cites", Method: "rule:citation", Count: 1}, {Kind: "repeals", Method: "rule:citation", Count: 1}}) {
			t.Errorf("breakdown = %v", dry.Breakdown)
		}
		sampled := map[string]bool{}
		for _, s := range dry.Sample {
			var m map[string]any
			_ = json.Unmarshal(s, &m)
			sampled[m["id"].(string)] = true
		}
		// A dry run changes nothing.
		if s := tenantEdge(t, d, op, c.cites); s["status"] != edgeStatusProposed {
			t.Fatalf("the dry run ruled: %v", s)
		}
		real := ruleReal(t, d, op, req, RuleAsOperator)
		if real.DryRun || real.RulingID == "" || real.Ruled != 2 || real.Failed != 0 || real.Matched != dry.Matched {
			t.Fatalf("rule = %+v", real)
		}
		for _, it := range real.Items {
			if !sampled[it.ID] || it.Status != edgeStatusAsserted {
				t.Errorf("ruled %+v, which the dry run did not show", it)
			}
		}
		for _, id := range []string{c.elaborates, c.judged} {
			if s := tenantEdge(t, d, op, id); s["status"] != edgeStatusProposed {
				t.Errorf("%s outside the filter was ruled: %v", id, s)
			}
		}
	})
}

func TestRuleStatements_RefusesWhenTheQueueChangedSinceTheDryRun(t *testing.T) {
	d, base, _ := documentFixture(t)
	c := seedReviewCorpus(t, d, base)
	op := reviewOperatorCtx(base, nil)
	req := connector.RuleStatementsRequest{Action: "accept", Filter: &connector.ReviewQueueFilter{Method: "rule:"}}
	dry := ruleDry(t, d, op, req, RuleAsOperator)
	// A new proposal lands between the dry run and the ruling.
	if _, res := docExec(t, d, reviewWriterCtx(base), fmt.Sprintf(`{"op":"propose_statement","scope":"tenant","from_id":%q,"to_id":%q,"kind":"based_on","method":"rule:citation"}`, c.b2, c.n1)); res.IsError {
		t.Fatal(res.Text)
	}
	req.DryRun, req.ExpectCount = boolPtr(false), intPtr(dry.Matched)
	_, err := d.RuleStatements(op, "tenant", req, RuleAsOperator)
	if connector.StatementReviewCode(err) != "queue_changed" {
		t.Fatalf("rule after the queue changed: %v, want queue_changed", err)
	}
	if s := tenantEdge(t, d, op, c.cites); s["status"] != edgeStatusProposed {
		t.Errorf("a refused ruling ruled: %v", s)
	}
	// A real call without expect_count is refused too.
	req.ExpectCount = nil
	if _, err := d.RuleStatements(op, "tenant", req, RuleAsOperator); connector.StatementReviewCode(err) != "invalid_request" {
		t.Errorf("rule without expect_count: %v", err)
	}
}

func TestRuleStatements_OneFailureDoesNotStopTheRest(t *testing.T) {
	d, base, _ := documentFixture(t)
	c := seedReviewCorpus(t, d, base)
	op := reviewOperatorCtx(base, nil)
	// One of the ids was already ruled on; one does not exist.
	if _, res := docExec(t, d, op, `{"op":"accept_statement","scope":"tenant","id":"`+c.cites+`"}`); res.IsError {
		t.Fatal(res.Text)
	}
	res := ruleReal(t, d, op, connector.RuleStatementsRequest{Action: "reject", Reason: "not a citation",
		IDs: []string{c.cites, c.repeals, "0123456789abcdef0123456789abcdef", c.elaborates}}, RuleAsOperator)
	if res.Ruled != 2 || res.Failed != 2 || res.Matched != 3 {
		t.Fatalf("rule = %+v, want 2 ruled and 2 failed of 3 matched", res)
	}
	codes := map[string]string{}
	for _, it := range res.Items {
		codes[it.ID] = it.Error + it.Status
	}
	want := map[string]string{c.cites: rulingNotProposal, c.repeals: edgeStatusRejected, c.elaborates: edgeStatusRejected,
		"0123456789abcdef0123456789abcdef": rulingNotFound}
	if fmt.Sprint(codes) != fmt.Sprint(want) {
		t.Errorf("items = %v, want %v", codes, want)
	}
	if s := tenantEdge(t, d, op, c.cites); s["status"] != edgeStatusAsserted || s["ruled_by"] != "operator" {
		t.Errorf("the failed item changed: %v", s)
	}
}

func TestRuleStatements_EachStatementRecordsTheBulkRuling(t *testing.T) {
	d, base, _ := documentFixture(t)
	c := seedReviewCorpus(t, d, base)
	op := reviewOperatorCtx(base, nil)
	res := ruleReal(t, d, op, connector.RuleStatementsRequest{Action: "reject", Reason: "title map points at the amending act",
		IDs: []string{c.cites, c.repeals}}, RuleAsOperator)
	for _, id := range []string{c.cites, c.repeals} {
		s := tenantEdge(t, d, op, id)
		if s["ruled_by"] != "bulk:"+res.RulingID || s["judge_reason"] != "title map points at the amending act" || s["status"] != edgeStatusRejected {
			t.Errorf("%s = %v, want ruled_by bulk:%s and the reason", id, s, res.RulingID)
		}
	}
	key, _, _ := d.resolveScope(op, "tenant")
	row, err := d.query(op, key, `SELECT action, matched, ruled, failed, reason, selector FROM statement_rulings WHERE id = ?`, res.RulingID)
	if err != nil || len(row.Rows) != 1 {
		t.Fatalf("ruling record: %v %v", row, err)
	}
	r := row.Rows[0]
	if asStr(r[0]) != "reject" || asInt(r[1]) != 2 || asInt(r[2]) != 2 || asInt(r[3]) != 0 || asStr(r[4]) == "" || !strings.Contains(asStr(r[5]), c.cites) {
		t.Errorf("ruling record = %v", r)
	}
}

func TestRuleStatements_RefusesMoreThanOneThousand(t *testing.T) {
	d, base, _ := documentFixture(t)
	op := reviewOperatorCtx(base, nil)
	ids := make([]string, reviewRulingMax+1)
	for i := range ids {
		ids[i] = fmt.Sprintf("%032x", i)
	}
	for _, req := range []connector.RuleStatementsRequest{
		{Action: "accept", IDs: ids},
		{Action: "accept", Filter: &connector.ReviewQueueFilter{}, Max: reviewRulingMax + 1},
	} {
		if _, err := d.RuleStatements(op, "tenant", req, RuleAsOperator); connector.StatementReviewCode(err) != "invalid_request" {
			t.Errorf("%d ids, max %d: %v, want invalid_request", len(req.IDs), req.Max, err)
		}
	}
	// Exactly the cap is fine.
	if _, err := d.RuleStatements(op, "tenant", connector.RuleStatementsRequest{Action: "accept", IDs: ids[:reviewRulingMax]}, RuleAsOperator); err != nil {
		t.Errorf("%d ids: %v", reviewRulingMax, err)
	}
}

func TestRuleStatements_MaxRulesTheFirstInQueueOrderAndLeavesTheRest(t *testing.T) {
	d, base, _ := documentFixture(t)
	c := seedReviewCorpus(t, d, base)
	op := reviewOperatorCtx(base, nil)
	res := ruleReal(t, d, op, connector.RuleStatementsRequest{Action: "accept", Filter: &connector.ReviewQueueFilter{}, Max: 1}, RuleAsOperator)
	if res.Matched != 4 || res.Selected != 1 || res.Ruled != 1 || res.Items[0].ID != c.judged {
		t.Fatalf("rule max 1 = %+v, want the newest (%s) only", res, c.judged)
	}
}

func TestRuleStatements_RefusedInsideARunAndForRejectWithoutAReason(t *testing.T) {
	d, base, _ := documentFixture(t)
	c := seedReviewCorpus(t, d, base)
	req := connector.RuleStatementsRequest{Action: "accept", IDs: []string{c.cites}}
	if _, err := d.RuleStatements(reviewWriterCtx(base), "tenant", req, RuleAsOperator); connector.StatementReviewCode(err) != "forbidden" {
		t.Errorf("a ruling from inside a run: %v, want forbidden", err)
	}
	req.Action = "reject"
	if _, err := d.RuleStatements(reviewOperatorCtx(base, nil), "tenant", req, RuleAsOperator); connector.StatementReviewCode(err) != "invalid_request" {
		t.Errorf("a reject without a reason: %v, want invalid_request", err)
	}
}

// A member reviews where an ACL grants it `review`, is skipped (counted, never
// named) elsewhere, and holds nothing without the grant — whatever the baseline
// allows it to read, write or link.
func TestRuleStatements_ReviewerRulesOnlyWhereGrantedAndNeverNamesTheRest(t *testing.T) {
	d, base, _ := documentFixture(t)
	c := seedReviewCorpus(t, d, base)
	readAll := authz.Rule{Effect: authz.Allow, Actions: []string{"read", "write", "link", "propose"}, Resources: []string{"document:tenant"}}
	reviewer := reviewOperatorCtx(base, aclSet(t, readAll,
		authz.Rule{Effect: authz.Allow, Actions: []string{"review"}, Resources: []string{"document:tenant:/board/**"}}))
	req := connector.RuleStatementsRequest{Action: "accept", Filter: &connector.ReviewQueueFilter{}}
	dry := ruleDry(t, d, reviewer, req, RuleAsReviewer)
	if dry.Matched != 4 || dry.Skipped != 2 || dry.ToRule != 2 {
		t.Fatalf("reviewer dry run = %+v, want 2 of 4 rulable", dry)
	}
	res := ruleReal(t, d, reviewer, req, RuleAsReviewer)
	if res.Ruled != 2 || res.Skipped != 2 {
		t.Fatalf("reviewer rule = %+v", res)
	}
	body, _ := json.Marshal(res)
	for _, hidden := range []string{c.elaborates, c.judged} {
		if strings.Contains(string(body), hidden) {
			t.Errorf("the result names %s, a statement the reviewer may not rule on: %s", hidden, body)
		}
		if s := tenantEdge(t, d, reviewOperatorCtx(base, nil), hidden); s["status"] != edgeStatusProposed {
			t.Errorf("%s outside the grant was ruled: %v", hidden, s)
		}
	}
	// Without the grant a member rules on nothing.
	none := reviewOperatorCtx(base, aclSet(t, readAll))
	if dry := ruleDry(t, d, none, connector.RuleStatementsRequest{Action: "accept", IDs: []string{c.elaborates}}, RuleAsReviewer); dry.ToRule != 0 || dry.Skipped != 1 {
		t.Errorf("a member without review: %+v", dry)
	}
}

// An operator whose rules deny write on a from-document is skipped there, as a
// member without the grant is.
func TestRuleStatements_OperatorNeedsWriteOnTheFromDocument(t *testing.T) {
	d, base, _ := documentFixture(t)
	c := seedReviewCorpus(t, d, base)
	op := reviewOperatorCtx(base, aclSet(t,
		authz.Rule{Effect: authz.Allow, Actions: []string{"read", "write", "link", "propose"}, Resources: []string{"document:tenant"}},
		authz.Rule{Effect: authz.Deny, Actions: []string{"write"}, Resources: []string{"document:tenant:/kb/**"}}))
	dry := ruleDry(t, d, op, connector.RuleStatementsRequest{Action: "accept", Filter: &connector.ReviewQueueFilter{}}, RuleAsOperator)
	if dry.Matched != 4 || dry.Skipped != 2 || dry.ToRule != 2 {
		t.Errorf("operator denied write under /kb: %+v, want the two from /kb/notes skipped", dry)
	}
	_ = c
}

func TestRevertRuling_ReturnsUnchangedStatementsAndLiftsARejection(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, base context.Context) {
		c := seedReviewCorpus(t, d, base)
		op := reviewOperatorCtx(base, nil)
		res := ruleReal(t, d, op, connector.RuleStatementsRequest{Action: "reject", Reason: "wrong act", IDs: []string{c.cites, c.repeals}}, RuleAsOperator)
		// A rejection is a tombstone: the relation cannot be proposed again.
		again := fmt.Sprintf(`{"op":"propose_statement","scope":"tenant","from_id":%q,"to_id":%q,"kind":"cites","method":"rule:citation"}`, c.b1, c.b2)
		if _, r := docExec(t, d, reviewWriterCtx(base), again); !r.IsError || !strings.Contains(r.Text, "statement_rejected") {
			t.Fatalf("re-proposing a rejected relation: %s", r.Text)
		}
		rev, err := d.RevertRuling(op, "tenant", connector.RevertRulingRequest{RulingID: res.RulingID}, RuleAsOperator)
		if err != nil || rev.Reverted != 2 || rev.Changed != 0 || rev.Action != "reject" {
			t.Fatalf("revert = %+v %v", rev, err)
		}
		for _, id := range []string{c.cites, c.repeals} {
			s := tenantEdge(t, d, op, id)
			if s["status"] != edgeStatusProposed || s["ruled_by"] != nil || s["judge_reason"] != nil || s["operator_ruled_at"] != nil {
				t.Errorf("%s after revert = %v, want proposed with no ruling", id, s)
			}
		}
		if out, r := docExec(t, d, reviewWriterCtx(base), again); r.IsError || out["id"] != c.cites {
			t.Errorf("the rejection's tombstone held after the revert: %v %s", out, r.Text)
		}
	})
}

func TestRevertRuling_LeavesAndReportsWhatChangedSince(t *testing.T) {
	d, base, _ := documentFixture(t)
	c := seedReviewCorpus(t, d, base)
	op := reviewOperatorCtx(base, nil)
	res := ruleReal(t, d, op, connector.RuleStatementsRequest{Action: "accept", IDs: []string{c.cites, c.repeals, c.elaborates}}, RuleAsOperator)
	// cites: new evidence; repeals: maintenance; elaborates: untouched.
	if _, r := docExec(t, d, reviewWriterCtx(base), fmt.Sprintf(`{"op":"link_chunks","scope":"tenant","from_id":%q,"to_id":%q,"kind":"cites",
		"evidence":[{"chunk_id":%q,"quote":"The Act of 2015"}]}`, c.b1, c.b2, c.b2)); r.IsError {
		t.Fatal(r.Text)
	}
	if _, r := docExec(t, d, op, `{"op":"maintain_statement","scope":"tenant","id":"`+c.repeals+`","action":"verify"}`); r.IsError {
		t.Fatal(r.Text)
	}
	rev, err := d.RevertRuling(op, "tenant", connector.RevertRulingRequest{RulingID: res.RulingID}, RuleAsOperator)
	if err != nil || rev.Reverted != 1 || rev.Changed != 2 {
		t.Fatalf("revert = %+v %v", rev, err)
	}
	for _, it := range rev.Items {
		want := "changed"
		if it.ID == c.elaborates {
			want = ""
		}
		if it.Error != want {
			t.Errorf("item %+v, want error %q", it, want)
		}
	}
	for id, status := range map[string]string{c.cites: edgeStatusAsserted, c.repeals: edgeStatusAsserted, c.elaborates: edgeStatusProposed} {
		if s := tenantEdge(t, d, op, id); s["status"] != status {
			t.Errorf("%s = %v, want %s", id, s["status"], status)
		}
	}
	// A later ruling is a change too: the reverted statement ruled again by hand
	// is not undone by reverting the bulk ruling once more.
	if _, r := docExec(t, d, op, `{"op":"accept_statement","scope":"tenant","id":"`+c.elaborates+`"}`); r.IsError {
		t.Fatal(r.Text)
	}
	rev, err = d.RevertRuling(op, "tenant", connector.RevertRulingRequest{RulingID: res.RulingID}, RuleAsOperator)
	if err != nil || rev.Reverted != 0 || rev.Changed != 3 {
		t.Errorf("second revert = %+v %v", rev, err)
	}
	if _, err := d.RevertRuling(op, "tenant", connector.RevertRulingRequest{RulingID: "nope"}, RuleAsOperator); connector.StatementReviewCode(err) != "ruling_not_found" {
		t.Errorf("revert of an unknown ruling: %v", err)
	}
}
