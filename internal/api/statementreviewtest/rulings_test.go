package statementreviewtest

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/api/grpc/loomcyclepb"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// Parity of the review surface's later additions: listing rulings, and naming
// another subject's user scope (scope_id), on every transport.

// call is one review op with free arguments on one transport.
type call func(e *env, bearer, op string, args map[string]any) outcome

var calls = []struct {
	name string
	do   call
}{{"http", (*env).callHTTP}, {"grpc", (*env).callGRPC}, {"mcp", (*env).callMCP}}

func (e *env) callHTTP(bearer, op string, args map[string]any) outcome {
	switch op {
	case "queue", "rulings":
		q := url.Values{}
		for k, v := range args {
			q.Set(k, fmt.Sprint(v))
		}
		path := map[string]string{"queue": "/v1/_statements/review", "rulings": "/v1/_statements/rulings"}[op]
		return httpOutcome(e.do(http.MethodGet, path+"?"+q.Encode(), bearer, "", nil))
	}
	body, _ := json.Marshal(args)
	return httpOutcome(e.do(http.MethodPost, "/v1/_statements/"+op, bearer, string(body), nil))
}

func (e *env) callMCP(bearer, op string, args map[string]any) outcome {
	m := map[string]any{"op": op}
	for k, v := range args {
		m[k] = v
	}
	return e.reviewMCP(bearer, m)
}

func str(args map[string]any, k string) string { s, _ := args[k].(string); return s }

func (e *env) callGRPC(bearer, op string, args map[string]any) outcome {
	ctx := authed(bearer)
	tenant, scope, scopeID := str(args, "tenant"), str(args, "scope"), str(args, "scope_id")
	switch op {
	case "queue":
		resp, err := e.grpc.ListReviewQueue(ctx, &loomcyclepb.ListReviewQueueRequest{Tenant: tenant, Scope: scope, ScopeId: scopeID})
		if err != nil {
			return grpcOutcome(err, nil)
		}
		stmts := []json.RawMessage{}
		for _, s := range resp.GetStatementsJson() {
			stmts = append(stmts, s)
		}
		return grpcOutcome(nil, map[string]any{"tenant": resp.GetTenant(), "scope": resp.GetScope(), "statements": stmts})
	case "rule":
		in := &loomcyclepb.RuleStatementsRequest{Tenant: tenant, Scope: scope, ScopeId: scopeID, Action: str(args, "action"), Reason: str(args, "reason")}
		in.Ids, _ = args["ids"].([]string)
		if d, ok := args["dry_run"].(bool); ok {
			in.DryRun = &d
		}
		if n, ok := args["expect_count"].(float64); ok {
			c := int32(n)
			in.ExpectCount = &c
		}
		resp, err := e.grpc.RuleStatements(ctx, in)
		if err != nil {
			return grpcOutcome(err, nil)
		}
		m := map[string]any{"matched": resp.GetMatched(), "ruled": resp.GetRuled()}
		if resp.GetRulingId() != "" {
			m["ruling_id"] = resp.GetRulingId()
		}
		return grpcOutcome(nil, m)
	case "revert":
		resp, err := e.grpc.RevertRuling(ctx, &loomcyclepb.RevertRulingRequest{Tenant: tenant, Scope: scope, ScopeId: scopeID, RulingId: str(args, "ruling_id")})
		if err != nil {
			return grpcOutcome(err, nil)
		}
		return grpcOutcome(nil, map[string]any{"reverted": resp.GetReverted(), "revert_id": resp.GetRevertId()})
	case "rulings":
		resp, err := e.grpc.ListRulings(ctx, &loomcyclepb.ListRulingsRequest{Tenant: tenant, Scope: scope, ScopeId: scopeID})
		if err != nil {
			return grpcOutcome(err, nil)
		}
		rulings := []map[string]any{}
		for _, r := range resp.GetRulings() {
			m := map[string]any{"id": r.GetId(), "action": r.GetAction(), "ruled_by": r.GetRuledBy(), "matched": r.GetMatched(),
				"ruled": r.GetRuled(), "failed": r.GetFailed(), "skipped": r.GetSkipped(), "created_at": r.GetCreatedAt(), "reverted": r.GetReverted()}
			sel := map[string]any{}
			for k, v := range map[string]string{"actor": r.GetActor(), "reason": r.GetReason(), "reverted_by": r.GetRevertedBy()} {
				if v != "" {
					m[k] = v
				}
			}
			if r.GetSelector().GetIdsCount() != 0 {
				sel["ids_count"] = r.GetSelector().GetIdsCount()
			}
			if r.GetSelector().GetReverts() != "" {
				sel["reverts"] = r.GetSelector().GetReverts()
			}
			if r.GetSelector().GetHasFilter() {
				sel["filter"] = map[string]any{}
			}
			if p := r.GetSelector().GetPolicy(); p != nil {
				pm := map[string]any{"version": p.GetVersion()}
				for k, v := range map[string]string{"source": p.GetSource(), "run_id": p.GetRunId()} {
					if v != "" {
						pm[k] = v
					}
				}
				sel["policy"] = pm
			}
			if j := r.GetSelector().GetJudge(); j != nil {
				jm := map[string]any{"run_id": j.GetRunId()}
				if j.GetAgent() != "" {
					jm["agent"] = j.GetAgent()
				}
				sel["judge"] = jm
			}
			m["selector"] = sel
			rulings = append(rulings, m)
		}
		return grpcOutcome(nil, map[string]any{"tenant": resp.GetTenant(), "scope": resp.GetScope(), "rulings": rulings})
	}
	panic("unknown op " + op)
}

// rulingLines renders a rulings answer without its ids and times: each
// ruling's action, who, counts, selector, and whether (and by which listed
// record) it was reverted.
func rulingLines(o outcome) []string {
	raw, _ := o.result["rulings"].([]any)
	pos := map[string]int{}
	for i, r := range raw {
		pos[r.(map[string]any)["id"].(string)] = i
	}
	var out []string
	for _, r := range raw {
		m := r.(map[string]any)
		sel, _ := m["selector"].(map[string]any)
		by := ""
		if v, ok := m["reverted_by"].(string); ok {
			by = fmt.Sprint("#", pos[v])
		}
		reverts := ""
		if v, ok := sel["reverts"].(string); ok {
			reverts = fmt.Sprint("#", pos[v])
		}
		out = append(out, fmt.Sprint(m["action"], "|", m["ruled_by"], "|", m["actor"], "|", m["reason"], "|ids=", sel["ids_count"],
			"|filter=", sel["filter"] != nil, "|reverts=", reverts, "|", m["matched"], "/", m["ruled"], "/", m["failed"], "/", m["skipped"],
			"|reverted=", m["reverted"], by))
	}
	return out
}

// TestStatementReview_SameRulingsListedOnEveryTransport: a ruling and its
// revert, listed, read the same on every transport, newest first, with the
// ruling naming its revert and no statement named.
func TestStatementReview_SameRulingsListedOnEveryTransport(t *testing.T) {
	var first []string
	for _, c := range calls {
		e := newEnv(t)
		ruled := e.ruleTwice(c.do, operator, map[string]any{"action": "reject", "reason": "wrong act", "ids": []string{e.cites, e.repeals}})
		if rev := c.do(e, operator, "revert", map[string]any{"ruling_id": ruled}); !rev.ok || rev.result["revert_id"] == "" {
			t.Fatalf("%s: revert = %s %s", c.name, rev.code, rev.raw)
		}
		o := c.do(e, operator, "rulings", map[string]any{})
		if !o.ok {
			t.Fatalf("%s: rulings refused: %s %s", c.name, o.code, o.raw)
		}
		for _, id := range []string{e.cites, e.repeals, e.elaborates} {
			if strings.Contains(o.raw, id) {
				t.Errorf("%s: the listing names statement %s: %s", c.name, id, o.raw)
			}
		}
		got := rulingLines(o)
		if first == nil {
			first = got
			want := []string{
				"revert|operator:ops|ops|<nil>|ids=<nil>|filter=false|reverts=#1|2/2/0/0|reverted=false",
				"reject|operator:ops|ops|wrong act|ids=2|filter=false|reverts=|2/2/0/0|reverted=true#0",
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("%s: rulings = %v, want %v", c.name, got, want)
			}
			continue
		}
		if !reflect.DeepEqual(got, first) {
			t.Errorf("%s rulings = %v, http %v", c.name, got, first)
		}
	}
}

// ruleTwice is a dry run and the ruling with its count, through do; it returns
// the ruling id.
func (e *env) ruleTwice(do call, bearer string, args map[string]any) string {
	e.t.Helper()
	dry := do(e, bearer, "rule", args)
	if !dry.ok {
		e.t.Fatalf("dry run refused: %s %s", dry.code, dry.raw)
	}
	real := map[string]any{"dry_run": false, "expect_count": dry.result["matched"]}
	for k, v := range args {
		real[k] = v
	}
	o := do(e, bearer, "rule", real)
	if !o.ok {
		e.t.Fatalf("ruling refused: %s %s", o.code, o.raw)
	}
	return o.result["ruling_id"].(string)
}

// TestStatementReview_AMemberListsOnlyItsOwnRulingsOnEveryTransport: rita (a
// reviewer) and the operator each rule; rita lists only hers, the operator
// both, alice (a member who ruled nothing) none — on every transport.
func TestStatementReview_AMemberListsOnlyItsOwnRulingsOnEveryTransport(t *testing.T) {
	e := newEnv(t)
	byRita := e.ruleTwice(calls[0].do, reviewer, map[string]any{"action": "accept", "ids": []string{e.cites}})
	byOps := e.ruleTwice(calls[0].do, operator, map[string]any{"action": "accept", "ids": []string{e.elaborates}})
	ids := func(o outcome) []string {
		out := []string{}
		raw, _ := o.result["rulings"].([]any)
		for _, r := range raw {
			out = append(out, r.(map[string]any)["id"].(string))
		}
		sort.Strings(out)
		return out
	}
	both := []string{byRita, byOps}
	sort.Strings(both)
	for _, c := range calls {
		for _, tc := range []struct {
			who, bearer string
			want        []string
		}{{"rita", reviewer, []string{byRita}}, {"the operator", operator, both}, {"alice", member, []string{}}} {
			o := c.do(e, tc.bearer, "rulings", map[string]any{})
			if !o.ok || !reflect.DeepEqual(ids(o), tc.want) {
				t.Errorf("%s: %s lists %v (%s %s), want %v", c.name, tc.who, ids(o), o.code, o.raw, tc.want)
			}
		}
		if o := c.do(e, isolated, "rulings", map[string]any{}); o.ok || o.code != "insufficient_scope" {
			t.Errorf("%s: an isolated user's rulings: ok=%v code %q", c.name, o.ok, o.code)
		}
	}
}

// seedUser files one proposal in subject's user scope of tenantID.
func (e *env) seedUser(tenantID, subject string) string {
	e.t.Helper()
	ctx := tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{TenantID: tenantID, UserID: subject})
	do := func(body string) map[string]any {
		res, err := e.doc.Execute(ctx, json.RawMessage(body))
		if err != nil || res.IsError {
			e.t.Fatalf("%s: %v %s", body, err, res.Text)
		}
		var out map[string]any
		_ = json.Unmarshal([]byte(res.Text), &out)
		return out
	}
	chunk := func(path string) string {
		d := do(fmt.Sprintf(`{"op":"create_document","scope":"user","title":%q,"path":%q}`, path, path))["document_id"].(string)
		return do(fmt.Sprintf(`{"op":"create_chunk","scope":"user","document_id":%q,"title":"part","body":"text"}`, d))["id"].(string)
	}
	a, b := chunk("/notes/a"), chunk("/notes/b")
	return do(fmt.Sprintf(`{"op":"propose_statement","scope":"user","from_id":%q,"to_id":%q,"kind":"cites"}`, a, b))["id"].(string)
}

// TestStatementReview_ScopeIDOnEveryTransport: the operator queues, rules,
// lists and reverts on alice's user scope; rita (a reviewer) naming alice is
// refused forbidden; the operator naming another tenant's subject is refused
// forbidden — the same on every transport.
func TestStatementReview_ScopeIDOnEveryTransport(t *testing.T) {
	for _, c := range calls {
		e := newEnv(t)
		stmt := e.seedUser(tenant, "alice")
		e.seedUser("globex", "dave")
		at := map[string]any{"scope": "user", "scope_id": "alice"}
		with := func(extra map[string]any) map[string]any {
			m := map[string]any{}
			for k, v := range at {
				m[k] = v
			}
			for k, v := range extra {
				m[k] = v
			}
			return m
		}
		if o := c.do(e, operator, "queue", at); !o.ok || !strings.Contains(o.raw, stmt) {
			t.Errorf("%s: the operator's queue of alice = %s %s", c.name, o.code, o.raw)
		}
		ruling := e.ruleTwice(c.do, operator, with(map[string]any{"action": "accept", "ids": []string{stmt}}))
		if o := c.do(e, operator, "rulings", at); !o.ok || !strings.Contains(o.raw, ruling) {
			t.Errorf("%s: the operator's rulings of alice = %s %s", c.name, o.code, o.raw)
		}
		if o := c.do(e, operator, "revert", with(map[string]any{"ruling_id": ruling})); !o.ok || fmt.Sprint(o.result["reverted"]) != "1" {
			t.Errorf("%s: the operator's revert on alice = %s %s", c.name, o.code, o.raw)
		}
		for _, op := range []struct {
			name string
			args map[string]any
		}{
			{"queue", with(nil)},
			{"rulings", with(nil)},
			{"rule", with(map[string]any{"action": "accept", "ids": []string{stmt}})},
			{"revert", with(map[string]any{"ruling_id": ruling})},
		} {
			if o := c.do(e, reviewer, op.name, op.args); o.ok || o.code != "forbidden" {
				t.Errorf("%s: rita's %s on alice: ok=%v code %q (%s)", c.name, op.name, o.ok, o.code, o.raw)
			}
			cross := map[string]any{"tenant": "globex", "scope": "user", "scope_id": "dave"}
			for k, v := range op.args {
				if _, set := cross[k]; !set {
					cross[k] = v
				}
			}
			if o := c.do(e, operator, op.name, cross); o.ok || o.code != "forbidden" {
				t.Errorf("%s: the operator's %s on globex/dave: ok=%v code %q (%s)", c.name, op.name, o.ok, o.code, o.raw)
			}
		}
	}
}
