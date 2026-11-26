package builtin

import (
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/store"
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

// RFC GW P13 (D-P13-2, owner ruling Q2): an admin may mint a user's personal
// worker token here with worker_personal; it needs
// workspace.personal_workers on, an explicit subject that is an enabled user
// of tenant_id, scopes exactly [workers:attach], and never worker_shared. A
// tenant operator mints it on the per-user mint, never through this tool.
func TestOperatorTokenDef_WorkerPersonalIsAnAdminsWithTheSwitchAndAnEnabledUser(t *testing.T) {
	tool, ctx, st, cleanup := operatorTokenDefFixture(t)
	defer cleanup()
	for _, u := range []store.UserRow{
		{TenantID: "acme", Subject: "alice", AccessMode: "isolated", Status: "active"},
		{TenantID: "acme", Subject: "carol", AccessMode: "tenant", Status: "disabled"},
		{TenantID: "other", Subject: "dave", AccessMode: "tenant", Status: "active"},
	} {
		if err := st.UserCreate(ctx, u); err != nil {
			t.Fatal(err)
		}
	}
	good := `{"op":"create","name":"alice-laptop","tenant_id":"acme","subject":"alice","scopes":["workers:attach"],"worker_personal":true}`
	if res := execRaw(t, tool, ctx, good); !res.IsError {
		t.Fatalf("switch unset: accepted: %s", res.Text)
	}
	tool.PersonalWorkers = func() bool { return false }
	if res := execRaw(t, tool, ctx, good); !res.IsError {
		t.Fatalf("switch off: accepted: %s", res.Text)
	}
	tool.PersonalWorkers = func() bool { return true }
	for _, in := range []string{
		`{"op":"create","name":"p1","tenant_id":"acme","scopes":["workers:attach"],"worker_personal":true}`,
		`{"op":"create","name":"p2","tenant_id":"acme","subject":"carol","scopes":["workers:attach"],"worker_personal":true}`,
		`{"op":"create","name":"p3","tenant_id":"acme","subject":"dave","scopes":["workers:attach"],"worker_personal":true}`,
		`{"op":"create","name":"p4","tenant_id":"acme","subject":"nobody","scopes":["workers:attach"],"worker_personal":true}`,
		`{"op":"create","name":"p5","tenant_id":"acme","subject":"alice","scopes":["workers:attach"],"worker_personal":true,"worker_shared":true}`,
		`{"op":"create","name":"p6","tenant_id":"acme","subject":"alice","scopes":["substrate:user"],"worker_personal":true}`,
	} {
		if res := execRaw(t, tool, ctx, in); !res.IsError {
			t.Errorf("%s: accepted: %s", in, res.Text)
		}
	}
	if n := countTokenRows(t, st); n != 0 {
		t.Fatalf("%d rows stored by refused creates", n)
	}
	row := storedRow(t, st, mustOp(t, tool, ctx, good))
	r, err := auth.ParseRestrictions(row.RestrictionsJSON)
	if err != nil || !r.WorkerPersonal {
		t.Fatalf("restrictions %q, %v", row.RestrictionsJSON, err)
	}
	att, err := auth.WorkerAttachment(auth.Principal{TenantID: row.TenantID, Subject: row.Subject, Scopes: row.AllowedScopes, TokenDefID: row.DefID, Restrictions: r})
	if err != nil || !att.Personal || att.UserID != "alice" {
		t.Errorf("attaches as %+v (%v)", att, err)
	}
	rotated := storedRow(t, st, mustOp(t, tool, ctx, `{"op":"rotate","def_id":"`+row.DefID+`"}`))
	if r, err := auth.ParseRestrictions(rotated.RestrictionsJSON); err != nil || !r.WorkerPersonal {
		t.Errorf("rotated restrictions %q (%v), want worker_personal", rotated.RestrictionsJSON, err)
	}
	// A tenant operator, even with the switch on, is refused here.
	tctx := tenantOpCtx(t, ctx, tntOp)
	if res := execRaw(t, tool, tctx, `{"op":"create","name":"w","subject":"alice","scopes":["workers:attach"],"worker_personal":true}`); !res.IsError {
		t.Errorf("a tenant operator: accepted: %s", res.Text)
	}
}
