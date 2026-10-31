package statementreviewtest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/api/grpc/loomcyclepb"
	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// reviewRules is the policy the parity tests apply: the two /board citations
// are accepted, the /kb elaboration routed to the judge.
const reviewRules = "- accept kind:cites,repeals when self:false\n- judge kind:elaborates\n- human *\n"

// installPolicy writes a confirmed tenant ontology whose Review section is
// rules, after the queue was seeded, so the policy has proposals to apply to.
func (e *env) installPolicy(rules string) {
	e.t.Helper()
	ctx := tools.WithSubstrateOperator(e.docCtx())
	call := func(body string) map[string]any {
		res, err := e.doc.Execute(ctx, json.RawMessage(body))
		if err != nil || res.IsError {
			e.t.Fatalf("%s: %v %s", body, err, res.Text)
		}
		var out map[string]any
		_ = json.Unmarshal([]byte(res.Text), &out)
		return out
	}
	md, _ := json.Marshal("# Tenant Ontology\n\n## Review\n@section review\n" + rules)
	out := call(`{"op":"import_md","scope":"tenant","markdown":` + string(md) + `}`)
	path, _ := json.Marshal(memrank.OntologyPath)
	call(fmt.Sprintf(`{"op":"set_path","scope":"tenant","id":%q,"path":%s}`, out["document_id"], path))
	root := out["root_chunk_id"].(string)
	g := call(fmt.Sprintf(`{"op":"get_chunk","scope":"tenant","id":%q}`, root))
	call(fmt.Sprintf(`{"op":"update_chunk","scope":"tenant","id":%q,"status":"confirmed","revision":%d}`, root, int(g["revision"].(float64))))
}

type policySurface struct {
	name  string
	apply func(e *env, bearer string, req map[string]any) outcome
}

var policySurfaces = []policySurface{
	{"http", (*env).applyHTTP}, {"grpc", (*env).applyGRPC}, {"mcp", (*env).applyMCP},
}

func (e *env) applyHTTP(bearer string, req map[string]any) outcome {
	body, _ := json.Marshal(req)
	return httpOutcome(e.do(http.MethodPost, "/v1/_statements/apply_policy", bearer, string(body), nil))
}

func (e *env) applyGRPC(bearer string, req map[string]any) outcome {
	in := &loomcyclepb.ApplyReviewPolicyRequest{}
	if f, ok := req["filter"].(map[string]any); ok {
		in.Filter = &loomcyclepb.ReviewQueueFilter{}
		in.Filter.FromUnderPath, _ = f["from_under_path"].(string)
	}
	if d, ok := req["dry_run"].(bool); ok {
		in.DryRun = &d
	}
	if n, ok := req["expect_count"].(float64); ok {
		c := int32(n)
		in.ExpectCount = &c
	}
	resp, err := e.grpc.ApplyReviewPolicy(authed(bearer), in)
	if err != nil {
		return grpcOutcome(err, nil)
	}
	rules := []map[string]any{}
	for _, r := range resp.GetRules() {
		m := map[string]any{"rule": r.GetRule(), "action": r.GetAction(), "count": r.GetCount()}
		if r.GetText() != "" {
			m["text"] = r.GetText()
		}
		rules = append(rules, m)
	}
	m := map[string]any{"tenant": resp.GetTenant(), "scope": resp.GetScope(), "dry_run": resp.GetDryRun(), "source": resp.GetSource(),
		"version": resp.GetVersion(), "matched": resp.GetMatched(), "skipped": resp.GetSkipped(), "accept": resp.GetAccept(),
		"reject": resp.GetReject(), "judge": resp.GetJudge(), "human": resp.GetHuman(), "to_rule": resp.GetToRule(),
		"deferred": resp.GetDeferred(), "ruled": resp.GetRuled(), "failed": resp.GetFailed(), "marked": resp.GetMarked(), "rules": rules}
	if resp.GetRulingId() != "" {
		m["ruling_id"] = resp.GetRulingId()
	}
	if len(resp.GetItems()) > 0 {
		m["items"] = items(resp.GetItems())
	}
	if len(resp.GetSampleJson()) > 0 {
		var sample []json.RawMessage
		for _, s := range resp.GetSampleJson() {
			sample = append(sample, s)
		}
		m["sample"] = sample
	}
	return grpcOutcome(nil, m)
}

func (e *env) applyMCP(bearer string, req map[string]any) outcome {
	args := map[string]any{"op": "apply_policy"}
	for k, v := range req {
		args[k] = v
	}
	return e.reviewMCP(bearer, args)
}

// routes reads each seeded statement's judge routing.
func (e *env) routes() map[string]string {
	e.t.Helper()
	out := map[string]string{}
	for name, id := range map[string]string{"cites": e.cites, "repeals": e.repeals, "elaborates": e.elaborates} {
		s := e.docCall(fmt.Sprintf(`{"op":"get_edge","scope":"tenant","id":%q}`, id))["statement"].(map[string]any)
		attrs, _ := s["attributes"].(map[string]any)
		route, _ := attrs["review_route"].(string)
		out[name] = route
	}
	return out
}

// TestApplyReviewPolicy_SameApplicationAndRevertOnEveryTransport: the policy
// applied as dry run, real call and revert, once per transport against
// identical tenants, answers the same at each step and leaves the same store.
func TestApplyReviewPolicy_SameApplicationAndRevertOnEveryTransport(t *testing.T) {
	var first []any
	for _, s := range policySurfaces {
		e := newEnv(t)
		e.installPolicy(reviewRules)
		dry := s.apply(e, operator, map[string]any{})
		if !dry.ok {
			t.Fatalf("%s: dry run refused: %s %s", s.name, dry.code, dry.raw)
		}
		real := s.apply(e, operator, map[string]any{"dry_run": false, "expect_count": dry.result["matched"]})
		if !real.ok {
			t.Fatalf("%s: application refused: %s %s", s.name, real.code, real.raw)
		}
		applied, routed := e.state(), e.routes()
		rev := surfaces[map[string]int{"http": 0, "grpc": 1, "mcp": 2}[s.name]].revert(e, operator, real.result["ruling_id"].(string))
		if !rev.ok {
			t.Fatalf("%s: revert refused: %s %s", s.name, rev.code, rev.raw)
		}
		steps := []any{e.normalized(dry.result), e.normalized(real.result), applied, routed, e.normalized(rev.result), e.state()}
		if first == nil {
			first = steps
			want := map[string]string{"cites": "asserted|policy", "repeals": "asserted|policy", "elaborates": "proposed|"}
			if !reflect.DeepEqual(applied, want) || routed["elaborates"] != "judge" {
				t.Errorf("%s: after the application %v %v, want %v and elaborates routed to the judge", s.name, applied, routed, want)
			}
			if r := real.result; r["matched"] != float64(3) || r["ruled"] != float64(2) || r["marked"] != float64(1) || r["source"] != "review" {
				t.Errorf("%s: application = %v", s.name, r)
			}
			if after := e.state(); after["cites"] != "proposed|" || after["repeals"] != "proposed|" {
				t.Errorf("%s: after the revert %v", s.name, after)
			}
			continue
		}
		for i, step := range []string{"dry run", "application", "store after it", "routes after it", "revert", "store after the revert"} {
			if !reflect.DeepEqual(steps[i], first[i]) {
				t.Errorf("%s %s = %v, http %v", s.name, step, steps[i], first[i])
			}
		}
	}
}

// TestApplyReviewPolicy_SameRefusalsOnEveryTransport: a member is refused even
// holding review (forbidden), an isolated user at the gate, a stale count and
// a tenant with no policy in force carry the same codes; none changes anything.
func TestApplyReviewPolicy_SameRefusalsOnEveryTransport(t *testing.T) {
	bare := newEnv(t)
	e := newEnv(t)
	e.installPolicy(reviewRules)
	before := e.state()
	for _, s := range policySurfaces {
		for _, c := range []struct {
			name, bearer string
			on           *env
			req          map[string]any
			code         string
		}{
			{"a reviewer", reviewer, e, map[string]any{}, "forbidden"},
			{"a member", member, e, map[string]any{}, "forbidden"},
			{"an isolated user", isolated, e, map[string]any{}, "insufficient_scope"},
			{"a stale count", operator, e, map[string]any{"dry_run": false, "expect_count": float64(99)}, "queue_changed"},
			{"no policy in force", operator, bare, map[string]any{}, "invalid_request"},
		} {
			if o := s.apply(c.on, c.bearer, c.req); o.ok || o.code != c.code {
				t.Errorf("%s %s: ok=%v code %q, want %q (%s)", s.name, c.name, o.ok, o.code, c.code, o.raw)
			} else if c.code == "invalid_request" && !strings.Contains(o.raw, "no review policy") {
				t.Errorf("%s %s: %s", s.name, c.name, o.raw)
			}
		}
	}
	if after := e.state(); !reflect.DeepEqual(after, before) {
		t.Errorf("a refused call changed the store: %v, was %v", after, before)
	}
}
