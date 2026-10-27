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

// Identity op=can about another person answers exactly what POST
// /v1/_authz/evaluation answers for the same named subject, in every mode:
// both compile that person's set in the class their user row gives them.
// A question the asking operator could not answer yes to for itself is
// refused by the tool (no oracle), and the operator's own Context op=can
// must then say no.
func TestIdentityCan_AgreesWithAuthzEvaluationForANamedSubject(t *testing.T) {
	for _, mode := range []authz.Mode{authz.ModeCompat, authz.ModeShadow, authz.ModeEnforce} {
		t.Run(string(mode), func(t *testing.T) {
			srv, ts, st := aclMgmtServer(t, mode)
			seedCanDrift(t, st)
			if err := st.UserCreate(context.Background(), store.UserRow{TenantID: "acme", Subject: "alice", DisplayName: "Alice", AccessMode: "tenant", Status: "active"}); err != nil {
				t.Fatal(err)
			}
			p, err := srv.aclResolver.Resolve(context.Background(), auth.Principal{TenantID: "acme", Subject: "ops", TokenDefID: "def_op", Scopes: []string{auth.ScopeTenant}})
			if err != nil {
				t.Fatal(err)
			}
			run := authz.WithTransport(authz.WithMode(context.Background(), mode), "http")
			run = auth.WithPrincipal(run, p)
			run = tools.WithRunIdentity(run, tools.RunIdentityValue{TenantID: "acme", UserID: "ops"})
			run = tools.WithRunID(run, "r_identity_drift")
			run = tools.WithRunPolicy(run, tools.RunPolicy{ACL: p.Effective})
			idTool := &builtin.Identity{Store: st, ACL: srv.aclResolver}
			ctxTool := &builtin.Context{Store: st}

			compared, refused := 0, 0
			for _, q := range canDriftQuestions {
				res := `{"type":"` + q.typ + `"`
				if q.id != "" {
					res += `,"id":"` + q.id + `"`
				}
				res += `}`
				out, err := idTool.Execute(run, json.RawMessage(`{"op":"can","subject":"alice","action":"`+q.action+`","resource":`+res+`}`))
				if err != nil {
					t.Fatal(err)
				}
				if out.IsError {
					if !strings.Contains(out.Text, "identity_not_found") {
						t.Fatalf("%s %s:%s: %s", q.action, q.typ, q.id, out.Text)
					}
					own, _ := ctxTool.Execute(run, json.RawMessage(`{"op":"can","action":"`+q.action+`","resource":`+res+`}`))
					if !strings.Contains(own.Text, `"allowed":false`) {
						t.Errorf("%s %s:%s refused though the operator may: %s", q.action, q.typ, q.id, own.Text)
					}
					refused++
					continue
				}
				code, body := bearerCall(t, ts, operatorBearer, "POST", "/v1/_authz/evaluation",
					`{"subject":{"type":"user","id":"alice"},"action":{"name":"`+q.action+`"},"resource":`+res+`}`)
				if code != 200 {
					t.Fatalf("%s %s:%s over HTTP: %d %s", q.action, q.typ, q.id, code, body)
				}
				if got, want := decisionFields(t, out.Text), decisionFields(t, body); got != want {
					t.Errorf("%s %s:%s\n  Identity op=can:       %s\n  /v1/_authz/evaluation: %s", q.action, q.typ, q.id, got, want)
				}
				compared++
			}
			// In shadow the rules are only observed, so the operator's own
			// answer is always yes and nothing is refused.
			if compared < len(canDriftQuestions)-2 || (refused == 0) != (mode == authz.ModeShadow) {
				t.Errorf("compared %d, refused %d of %d: the table no longer exercises both paths", compared, refused, len(canDriftQuestions))
			}
		})
	}
}
