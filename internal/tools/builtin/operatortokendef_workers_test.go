package builtin

import (
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// An admin mints a workspace worker's token bound to a tenant, or shared with
// worker_shared (RFC GW Q2); the stored record says which, and the principal
// that record resolves to is what auth.WorkerAttachment reads.
func TestOperatorTokenDef_AnAdminMintsTenantBoundAndSharedWorkerTokens(t *testing.T) {
	tool, ctx, st, cleanup := operatorTokenDefFixture(t)
	defer cleanup()
	for _, admin := range []auth.Principal{
		{TenantID: "ops", Subject: "root", Scopes: []string{auth.ScopeAdmin}},
		{TenantID: "default", Subject: "default", Scopes: []string{auth.ScopeAdmin}, Legacy: true},
	} {
		actx := auth.WithPrincipal(ctx, admin)
		actx = tools.WithOperatorTokenDefPolicy(actx, tools.OperatorTokenDefPolicyFor(admin, true))
		suffix := "-" + admin.Subject
		bound := storedRow(t, st, mustOp(t, tool, actx, `{"op":"create","name":"wk-acme`+suffix+`","tenant_id":"acme","scopes":["workers:attach"]}`))
		shared := storedRow(t, st, mustOp(t, tool, actx, `{"op":"create","name":"wk-pool`+suffix+`","tenant_id":"ops","scopes":["workers:attach"],"worker_shared":true}`))
		for _, c := range []struct {
			name       string
			wantShared bool
			row        func() (auth.Restrictions, error)
		}{
			{"bound", false, func() (auth.Restrictions, error) { return auth.ParseRestrictions(bound.RestrictionsJSON) }},
			{"shared", true, func() (auth.Restrictions, error) { return auth.ParseRestrictions(shared.RestrictionsJSON) }},
		} {
			r, err := c.row()
			if err != nil || r.WorkerShared != c.wantShared {
				t.Errorf("%s %s: restrictions %+v (%v), want worker_shared=%v", admin.Subject, c.name, r, err, c.wantShared)
			}
		}
		att, err := auth.WorkerAttachment(auth.Principal{TenantID: bound.TenantID, Subject: bound.Subject, Scopes: bound.AllowedScopes, TokenDefID: bound.DefID})
		if err != nil || att.Shared || !att.Serves("acme") || att.Serves("ops") {
			t.Errorf("%s: the tenant-bound token attaches as %+v (%v)", admin.Subject, att, err)
		}
		r, _ := auth.ParseRestrictions(shared.RestrictionsJSON)
		att, err = auth.WorkerAttachment(auth.Principal{TenantID: shared.TenantID, Subject: shared.Subject, Scopes: shared.AllowedScopes, TokenDefID: shared.DefID, Restrictions: r})
		if err != nil || !att.Shared || !att.Serves("acme") {
			t.Errorf("%s: the shared token attaches as %+v (%v)", admin.Subject, att, err)
		}
	}
}

// worker_shared fits a token holding workers:attach alone, and workers:attach
// fits no other scope beside it.
func TestOperatorTokenDef_WorkerSharedOnlyOnAWorkersAttachToken(t *testing.T) {
	tool, ctx, st, cleanup := operatorTokenDefFixture(t)
	defer cleanup()
	for in, want := range map[string]string{
		`{"op":"create","name":"a","tenant_id":"ops","scopes":["substrate:tenant"],"worker_shared":true}`:                 "worker_shared",
		`{"op":"create","name":"b","tenant_id":"ops","scopes":["metrics:read"],"worker_shared":true}`:                     "worker_shared",
		`{"op":"create","name":"c","tenant_id":"ops","scopes":["runs:create","runs:read"],"worker_shared":true}`:          "worker_shared",
		`{"op":"create","name":"d","tenant_id":"ops","scopes":["workers:attach","runs:read"]}`:                            "only scope",
		`{"op":"create","name":"e","tenant_id":"ops","scopes":["workers:attach","substrate:admin"],"worker_shared":true}`: "only scope",
	} {
		if res := execRaw(t, tool, ctx, in); !res.IsError || !strings.Contains(res.Text, want) {
			t.Errorf("%s: %s, want refused naming %q", in, res.Text, want)
		}
	}
	if n := countTokenRows(t, st); n != 0 {
		t.Errorf("%d rows stored by refused creates", n)
	}
}

// A tenant operator mints no worker token through the token tool, and above
// all no shared one: a worker serving every tenant is an admin's to mint.
func TestOperatorTokenDef_ATenantOperatorCannotMintAWorkerToken(t *testing.T) {
	tool, base, st, cleanup := operatorTokenDefFixture(t)
	defer cleanup()
	ctx := tenantOpCtx(t, base, tntOp)
	for _, in := range []string{
		`{"op":"create","name":"w","scopes":["workers:attach"]}`,
		`{"op":"create","name":"w","scopes":["workers:attach"],"worker_shared":true}`,
		`{"op":"create","name":"w","scopes":["workers:attach"],"worker_shared":false}`,
		`{"op":"create","name":"w","scopes":["metrics:read"],"worker_shared":true}`,
		`{"op":"create","name":"w","scopes":["metrics:read"],"worker_shared":false}`,
	} {
		if res := execRaw(t, tool, ctx, in); !res.IsError {
			t.Errorf("%s: accepted: %s", in, res.Text)
		}
	}
	if n := countTokenRows(t, st); n != 0 {
		t.Errorf("%d rows stored by refused creates", n)
	}
}

// A rotation carries the marker: the new secret serves the same tenants.
func TestOperatorTokenDef_ARotationKeepsWorkerShared(t *testing.T) {
	tool, ctx, st, cleanup := operatorTokenDefFixture(t)
	defer cleanup()
	created := mustOp(t, tool, ctx, `{"op":"create","name":"pool","tenant_id":"ops","scopes":["workers:attach"],"worker_shared":true}`)
	rotated := storedRow(t, st, mustOp(t, tool, ctx, `{"op":"rotate","def_id":"`+created["def_id"].(string)+`"}`))
	if r, err := auth.ParseRestrictions(rotated.RestrictionsJSON); err != nil || !r.WorkerShared {
		t.Errorf("rotated restrictions %q (%v), want worker_shared", rotated.RestrictionsJSON, err)
	}
}
