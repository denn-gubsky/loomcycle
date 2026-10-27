package http

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// canDriftQuestions covers an allow, a deny over an allow, a baseline allow,
// a condition on the transport, a scoped name, a type-level question and a
// question no rule answers.
var canDriftQuestions = []struct{ action, typ, id string }{
	{"run", "agent", "support/l2"},
	{"run", "agent", "admin/triage"},
	{"run", "agent", "ops/deploy"},
	{"run", "agent", "night/batch"},
	{"run", "agent", ""},
	{"invoke", "tool", "Bash"},
	{"invoke", "tool", "Read"},
	{"mint", "principal", "user:bob"},
	{"read", "memory", "tenant"},
	{"read", "document", "tenant:/kb/x"},
	{"use", "model", "claude-sonnet"},
}

// seedCanDrift gives alice (a member of acme) a group-bound list with an
// allow, a deny and a transport-conditional allow, and a tenant-wide deny.
func seedCanDrift(t *testing.T, st store.Store) {
	t.Helper()
	ctx := context.Background()
	if _, err := st.GroupPut(ctx, store.GroupRow{TenantID: "acme", Name: "support"}); err != nil {
		t.Fatal(err)
	}
	if err := st.GroupMemberAdd(ctx, store.GroupMemberRow{TenantID: "acme", GroupName: "support", MemberKind: "user", MemberID: "alice"}); err != nil {
		t.Fatal(err)
	}
	for name, rules := range map[string]string{
		"support-tier-1": `[{"effect":"allow","actions":["run"],"resources":["agent:support/**"]},
			{"effect":"deny","actions":["run"],"resources":["agent:admin/**"]},
			{"effect":"allow","actions":["run"],"resources":["agent:night/*"],"when":{"transport":["http"]}}]`,
		"no-shell": `[{"effect":"deny","actions":["invoke"],"resources":["tool:Bash"]}]`,
	} {
		if _, err := st.AclDefPut(ctx, store.AclDefRow{TenantID: "acme", Name: name, Rules: json.RawMessage(rules)}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.AclBindingPut(ctx, store.AclBindingRow{TenantID: "acme", ACLName: "support-tier-1", SubjectKind: "group", SubjectID: "support"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AclBindingPut(ctx, store.AclBindingRow{TenantID: "acme", ACLName: "no-shell", SubjectKind: "tenant"}); err != nil {
		t.Fatal(err)
	}
}

// decisionFields is the part of an answer both surfaces return.
func decisionFields(t *testing.T, raw string) string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("not JSON: %s", raw)
	}
	sub := map[string]any{}
	for _, k := range []string{"decision", "outcome", "reasons", "tenant_floor", "mode"} {
		if v, ok := m[k]; ok {
			sub[k] = v
		}
	}
	b, _ := json.Marshal(sub)
	return string(b)
}

// Context op=can inside a run answers exactly what POST /v1/_authz/evaluation
// answers for the same subject and question, in every mode: the in-run twin
// cannot drift from the operator one (or from the gates, which share the
// evaluation).
func TestContextCan_AgreesWithAuthzEvaluationForTheSameSubject(t *testing.T) {
	for _, mode := range []authz.Mode{authz.ModeCompat, authz.ModeShadow, authz.ModeEnforce} {
		t.Run(string(mode), func(t *testing.T) {
			srv, ts, st := aclMgmtServer(t, mode)
			seedCanDrift(t, st)
			// The run alice's token would start: her compiled set on the run
			// policy, under the mode and transport the HTTP request carries.
			p, err := srv.aclResolver.Resolve(context.Background(), auth.Principal{TenantID: "acme", Subject: "alice", TokenDefID: "def_alice",
				Scopes: []string{auth.ScopeRunsCreate, auth.ScopeRunsRead, auth.ScopeChannelPublish, auth.ScopeChannelRead}})
			if err != nil {
				t.Fatal(err)
			}
			run := authz.WithTransport(authz.WithMode(context.Background(), mode), "http")
			run = auth.WithPrincipal(run, p)
			run = tools.WithRunIdentity(run, tools.RunIdentityValue{TenantID: "acme", UserID: "alice"})
			run = tools.WithRunID(run, "r_drift")
			run = tools.WithRunPolicy(run, tools.RunPolicy{ACL: p.Effective})
			ctxTool := &builtin.Context{Store: st}

			queries := make([]string, 0, len(canDriftQuestions))
			var httpAnswers []string
			for _, q := range canDriftQuestions {
				res := `{"type":"` + q.typ + `"`
				if q.id != "" {
					res += `,"id":"` + q.id + `"`
				}
				res += `}`
				queries = append(queries, `{"action":"`+q.action+`","resource":`+res+`}`)

				code, body := bearerCall(t, ts, aliceBearer, "POST", "/v1/_authz/evaluation", `{"action":{"name":"`+q.action+`"},"resource":`+res+`}`)
				if code != 200 {
					t.Fatalf("%s %s:%s over HTTP: %d %s", q.action, q.typ, q.id, code, body)
				}
				httpAnswers = append(httpAnswers, decisionFields(t, body))

				out, err := ctxTool.Execute(run, json.RawMessage(`{"op":"can","action":"`+q.action+`","resource":`+res+`}`))
				if err != nil || out.IsError {
					t.Fatalf("%s %s:%s in-run: %v %s", q.action, q.typ, q.id, err, out.Text)
				}
				if got, want := decisionFields(t, out.Text), httpAnswers[len(httpAnswers)-1]; got != want {
					t.Errorf("%s %s:%s\n  Context op=can:      %s\n  /v1/_authz/evaluation: %s", q.action, q.typ, q.id, got, want)
				}
			}
			// The batch form answers the same, in order.
			out, err := ctxTool.Execute(run, json.RawMessage(`{"op":"can","queries":[`+strings.Join(queries, ",")+`]}`))
			if err != nil || out.IsError {
				t.Fatalf("batch: %v %s", err, out.Text)
			}
			var batch struct {
				Evaluations []json.RawMessage `json:"evaluations"`
			}
			if err := json.Unmarshal([]byte(out.Text), &batch); err != nil || len(batch.Evaluations) != len(httpAnswers) {
				t.Fatalf("batch = %s", out.Text)
			}
			for i, ev := range batch.Evaluations {
				if got := decisionFields(t, string(ev)); got != httpAnswers[i] {
					t.Errorf("batch[%d] = %s, want %s", i, got, httpAnswers[i])
				}
			}
			// The table must exercise every outcome, or agreement on it proves
			// little.
			seen := map[string]bool{}
			for _, a := range httpAnswers {
				var m map[string]any
				_ = json.Unmarshal([]byte(a), &m)
				seen[m["outcome"].(string)] = true
			}
			for _, o := range []string{"allow", "deny", "no_match"} {
				if !seen[o] {
					t.Errorf("no question in the table came out %s", o)
				}
			}
			// night/batch is allowed only on the http transport: agreement on
			// it is agreement on the transport the two evaluated under.
			if !strings.Contains(httpAnswers[3], `"outcome":"allow"`) {
				t.Errorf("the transport-conditional question = %s, want allow", httpAnswers[3])
			}
		})
	}
}
