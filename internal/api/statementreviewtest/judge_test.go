package statementreviewtest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"sort"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/api/grpc/loomcyclepb"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// judgeRun records a judge run's verdicts through the Document tool, as the
// kb/judge agent does: a run whose agent holds statement_methods [judge].
func (e *env) judgeRun(runID string, judgements string) map[string]any {
	e.t.Helper()
	ctx := tools.WithAgentName(tools.WithRunID(e.docCtx(), runID), "kb/judge")
	ctx = tools.UpdateRunPolicy(ctx, func(p *tools.RunPolicy) { p.StatementMethods = []string{"judge"} })
	res, err := e.doc.Execute(ctx, json.RawMessage(`{"op":"judge_statements","scope":"tenant","judgements":`+judgements+`}`))
	if err != nil || res.IsError {
		e.t.Fatalf("judge_statements: %v %s", err, res.Text)
	}
	var out map[string]any
	_ = json.Unmarshal([]byte(res.Text), &out)
	return out
}

// queueFiltered lists the queue under filter on one transport and returns the
// ids it lists, sorted.
func (e *env) queueFiltered(transport, bearer string, filter map[string]string) ([]string, outcome) {
	e.t.Helper()
	var o outcome
	switch transport {
	case "http":
		q := url.Values{}
		for k, v := range filter {
			q.Set(k, v)
		}
		o = httpOutcome(e.do(http.MethodGet, "/v1/_statements/review?"+q.Encode(), bearer, "", nil))
	case "grpc":
		f := &loomcyclepb.ReviewQueueFilter{Status: filter["status"], ReviewRoute: filter["review_route"], JudgeAudit: filter["judge_audit"]}
		resp, err := e.grpc.ListReviewQueue(authed(bearer), &loomcyclepb.ListReviewQueueRequest{Filter: f})
		if err != nil {
			return nil, grpcOutcome(err, nil)
		}
		stmts := []json.RawMessage{}
		for _, s := range resp.GetStatementsJson() {
			stmts = append(stmts, s)
		}
		o = grpcOutcome(nil, map[string]any{"statements": stmts})
	case "mcp":
		f := map[string]any{}
		for k, v := range filter {
			f[k] = v
		}
		o = e.reviewMCP(bearer, map[string]any{"op": "queue", "filter": f})
	}
	if !o.ok {
		return nil, o
	}
	var ids []string
	for _, s := range o.result["statements"].([]any) {
		ids = append(ids, s.(map[string]any)["id"].(string))
	}
	sort.Strings(ids)
	return ids, o
}

// TestJudge_RoutingAuditAndRulingAlikeOnEveryTransport: the judge's queue
// (review_route judge), its acceptances sampled for audit (status asserted +
// judge_audit pending) and the judge run's ruling in the rulings listing are
// the same on HTTP, gRPC and MCP.
func TestJudge_RoutingAuditAndRulingAlikeOnEveryTransport(t *testing.T) {
	e := newEnv(t)
	e.installPolicy("- judge kind:elaborates\n- human *\n")
	dry := e.applyHTTP(operator, map[string]any{})
	if real := e.applyHTTP(operator, map[string]any{"dry_run": false, "expect_count": dry.result["matched"]}); !real.ok {
		t.Fatalf("application refused: %s %s", real.code, real.raw)
	}
	for _, tr := range []string{"http", "grpc", "mcp"} {
		ids, o := e.queueFiltered(tr, operator, map[string]string{"review_route": "judge"})
		if !o.ok || !reflect.DeepEqual(ids, []string{e.elaborates}) {
			t.Errorf("%s: the judge's queue = %v (%s %s), want the routed elaboration only", tr, ids, o.code, o.raw)
		}
	}
	out := e.judgeRun("run-judge-parity", fmt.Sprintf(`[{"id":%q,"verdict":"accept","confidence":0.95,"rationale":"the quotes show it"}]`, e.elaborates))
	if out["ruled"] != float64(1) || out["audit_marked"] != float64(1) {
		t.Fatalf("judge run = %v, want one acceptance, sampled", out)
	}
	for _, tr := range []string{"http", "grpc", "mcp"} {
		ids, o := e.queueFiltered(tr, operator, map[string]string{"status": "asserted", "judge_audit": "pending"})
		if !o.ok || !reflect.DeepEqual(ids, []string{e.elaborates}) {
			t.Errorf("%s: the audit sample = %v (%s %s)", tr, ids, o.code, o.raw)
		}
	}
	var first map[string]any
	for _, c := range calls {
		o := c.do(e, operator, "rulings", map[string]any{})
		if !o.ok {
			t.Fatalf("%s: rulings refused: %s %s", c.name, o.code, o.raw)
		}
		var got map[string]any
		for _, r := range o.result["rulings"].([]any) {
			if m := r.(map[string]any); m["id"] == out["ruling_id"] {
				got = map[string]any{"action": m["action"], "ruled_by": m["ruled_by"], "ruled": m["ruled"], "judge": m["selector"].(map[string]any)["judge"]}
			}
		}
		if got == nil {
			t.Fatalf("%s: the judge run is not listed: %s", c.name, o.raw)
		}
		if first == nil {
			first = got
			j, _ := got["judge"].(map[string]any)
			if got["action"] != "judge" || got["ruled_by"] != "judge:run-judge-parity" || got["ruled"] != float64(1) ||
				j["run_id"] != "run-judge-parity" || j["agent"] != "kb/judge" {
				t.Errorf("%s: listed as %v", c.name, got)
			}
			continue
		}
		if !reflect.DeepEqual(got, first) {
			t.Errorf("%s lists it as %v, http %v", c.name, got, first)
		}
	}
}

// GET /v1/_statements/agreement counts the judge's acceptances and the audit
// sample a person settled. It is an operator's: a member is refused.
func TestJudge_AgreementReportOverHTTP(t *testing.T) {
	e := newEnv(t)
	e.installPolicy("judge_audit: 1\n- judge kind:elaborates\n- human *\n")
	if real := e.applyHTTP(operator, map[string]any{"dry_run": false, "expect_count": e.applyHTTP(operator, map[string]any{}).result["matched"]}); !real.ok {
		t.Fatalf("application refused: %s %s", real.code, real.raw)
	}
	e.judgeRun("run-judge-agree", fmt.Sprintf(`[{"id":%q,"verdict":"accept","confidence":0.95,"rationale":"the quotes show it"}]`, e.elaborates))
	if o := e.ruleHTTP(operator, map[string]any{"action": "reject", "ids": []string{e.elaborates}, "reason": "another article", "dry_run": false, "expect_count": 1}); !o.ok || o.result["ruled"] != float64(1) {
		t.Fatalf("overturn refused: %s %s", o.code, o.raw)
	}
	o := httpOutcome(e.do(http.MethodGet, "/v1/_statements/agreement", operator, "", nil))
	if !o.ok {
		t.Fatalf("agreement refused: %s %s", o.code, o.raw)
	}
	total, _ := o.result["total"].(map[string]any)
	if total["accepted"] != float64(1) || total["sampled"] != float64(1) || total["overturned"] != float64(1) || total["agreement"] != float64(0) {
		t.Errorf("total = %v, want one sampled acceptance, overturned, agreement 0", total)
	}
	if m := httpOutcome(e.do(http.MethodGet, "/v1/_statements/agreement", member, "", nil)); m.ok || m.code != "forbidden" {
		t.Errorf("a member read the agreement report: ok=%v code %q (%s)", m.ok, m.code, m.raw)
	}
}
