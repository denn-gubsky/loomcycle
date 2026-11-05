package http

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// A rule naming a pair no surface asks is refused at write, on the route and
// on its twin, with a code a client can branch on and a message naming the
// pair (RFC FY Part E). Before, it was stored and decided nothing.
func TestACLDef_ARuleOnAnUnenforcedPairIsRefused(t *testing.T) {
	srv, ts, _ := aclMgmtServer(t, authz.ModeCompat)
	inert := `[{"effect":"deny","actions":["browse"],"resources":["document:user"]}]`
	for _, op := range []string{"create", "update", "put", "validate"} {
		code, body := bearerCall(t, ts, operatorBearer, "POST", "/v1/_acldef", `{"op":"`+op+`","name":"inert","rules":`+inert+`}`)
		if code != 400 || jsonField(t, body, "code") != "rule_not_enforceable" || !strings.Contains(body, "browse document") {
			t.Errorf("%s: %d %s, want 400 rule_not_enforceable naming the pair", op, code, body)
		}
	}
	op := aclCaller(t, srv, "acme", "ops", auth.ScopeTenant)
	res := twinCall(t, srv, op, "acldef", `{"op":"create","name":"inert","rules":`+inert+`}`)
	if !res.IsError || textCode(res.Text) != "rule_not_enforceable" {
		t.Errorf("twin create: %s, want rule_not_enforceable", res.Text)
	}
	// An enforced pair is still written.
	if code, body := bearerCall(t, ts, operatorBearer, "POST", "/v1/_acldef", `{"op":"create","name":"live","rules":[{"effect":"deny","actions":["invoke"],"resources":["tool:Bash"]}]}`); code != 201 {
		t.Errorf("an enforced rule: %d %s", code, body)
	}
}

// A list stored before the refusal, or declared in config, is reported at
// boot and still loads: its other rules hold.
func TestReportUnenforceableACLs_NamesStoredAndConfiguredLists(t *testing.T) {
	srv, _, st := aclMgmtServerWith(t, authz.ModeCompat, func(c *config.Config) {
		c.ResolvedACL = &authz.Static{ACLs: map[string]authz.ACL{"yaml-old": {Name: "yaml-old", Rules: []authz.Rule{
			{Effect: authz.Deny, Actions: []string{"use"}, Resources: []string{"search:*"}},
		}}}}
	})
	rules := `[{"effect":"deny","actions":["invoke"],"resources":["tool:Bash"]},{"effect":"deny","actions":["recall"],"resources":["memory:user"]}]`
	if _, err := st.AclDefPut(context.Background(), store.AclDefRow{TenantID: "acme", Name: "old", Rules: json.RawMessage(rules)}); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(srv.ReportUnenforceableACLs(context.Background()), "\n")
	for _, want := range []string{"store acme/old#1: recall memory", "config /yaml-old#0: use search"} {
		if !strings.Contains(got, want) {
			t.Errorf("report %q lacks %q", got, want)
		}
	}
	if strings.Contains(got, "#0: invoke tool") || strings.Contains(got, "acme/old#0") {
		t.Errorf("an enforced rule was reported: %q", got)
	}
}
