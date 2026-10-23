package builtin

import (
	"context"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// RFC EZ §1.5 seam 5: CredentialDef asks `manage credential:<scope>:<name>`
// of a write and `use` of a metadata read, against the named credential.
func TestCredentialDefACL_ManageDenyRefusesRotateOfThatName(t *testing.T) {
	c, base := credFixture(t, "u1", testKEK())
	for _, body := range []string{
		`{"op":"generate","scope":"tenant","name":"X","key_type":"hmac-sha256"}`,
		`{"op":"generate","scope":"tenant","name":"Y","key_type":"hmac-sha256"}`,
		`{"op":"create","scope":"tenant","name":"S","value":"v1"}`,
	} {
		if _, res := credExec(t, c, base, body); res.IsError {
			t.Fatalf("seed %s: %s", body, res.Text)
		}
	}
	eff := aclSet(t, authz.Rule{Effect: authz.Deny, Actions: []string{"manage"}, Resources: []string{"credential:tenant:X", "credential:tenant:S"}})
	// Off a run (an operator surface): the principal's set.
	offRun := auth.WithPrincipal(base, auth.Principal{TenantID: "tnt", Subject: "u1", Effective: eff})
	// In a run's policy (a run started by that principal).
	inRun := tools.WithRunPolicy(base, tools.RunPolicy{ACL: eff})

	for _, tc := range []struct {
		name string
		ctx  context.Context
	}{{"off-run", offRun}, {"in-policy", inRun}} {
		name, ctx := tc.name, tc
		_, res := credExec(t, c, ctx.ctx, `{"op":"rotate","scope":"tenant","name":"X"}`)
		refusedByACL(t, name+": rotate X", res)
		_, res = credExec(t, c, ctx.ctx, `{"op":"delete","scope":"tenant","name":"S"}`)
		refusedByACL(t, name+": delete S", res)
		_, res = credExec(t, c, ctx.ctx, `{"op":"create","scope":"tenant","name":"S","value":"v2"}`)
		refusedByACL(t, name+": re-create (rotate the value of) S", res)
		_, res = credExec(t, c, ctx.ctx, `{"op":"rotate","scope":"tenant","name":"Y"}`)
		allowed(t, name+": rotate Y", res)
		_, res = credExec(t, c, ctx.ctx, `{"op":"get","scope":"tenant","name":"X"}`)
		allowed(t, name+": get X (use is not denied)", res)
	}
}

func TestCredentialDefACL_UseDenyRefusesGetAndHidesFromList(t *testing.T) {
	c, base := credFixture(t, "u1", testKEK())
	for _, n := range []string{"A", "B"} {
		if _, res := credExec(t, c, base, `{"op":"create","scope":"tenant","name":"`+n+`","value":"v"}`); res.IsError {
			t.Fatalf("seed %s: %s", n, res.Text)
		}
	}
	ctx := tools.WithRunPolicy(base, tools.RunPolicy{ACL: aclSet(t,
		authz.Rule{Effect: authz.Deny, Actions: []string{"use"}, Resources: []string{"credential:tenant:A"}})})
	_, res := credExec(t, c, ctx, `{"op":"get","scope":"tenant","name":"A"}`)
	refusedByACL(t, "get A", res)
	_, res = credExec(t, c, ctx, `{"op":"list","scope":"tenant"}`)
	if res.IsError || strings.Contains(res.Text, `"A"`) || !strings.Contains(res.Text, `"B"`) {
		t.Errorf("list = %s, want B only", res.Text)
	}
	// No set: both listed.
	_, res = credExec(t, c, base, `{"op":"list","scope":"tenant"}`)
	if !strings.Contains(res.Text, `"A"`) || !strings.Contains(res.Text, `"B"`) {
		t.Errorf("list with no set = %s", res.Text)
	}
}
