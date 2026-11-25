package builtin

import (
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/audit"
	"github.com/denn-gubsky/loomcycle/internal/auth"
)

// D-P6-48 (ruled 2026-11-25): a tenant operator's session lists every
// user's templates in its own tenant and deletes one by naming its owner;
// the audit line names both the operator and the owner. Another tenant's
// operator, a member token and a run (whatever bearer started it, RFC ES
// #15) are refused.
func TestWorkspaceTemplateOperator_ListsAndDeletesAnyUsersTemplateInItsTenant(t *testing.T) {
	f, alice, ck := tplFixture(t)
	rec := &recordingAudit{mu: make(chan struct{}, 1)}
	f.tool.Manager.SetAudit(rec)
	f.mustOK(t, alice, `{"op":"save_template","name":"work","checkpoint":"`+ck+`","as":"mine","scope":"user"}`)
	f.mustOK(t, withMemoryScopes(alice, "user", "tenant"), `{"op":"save_template","name":"work","checkpoint":"`+ck+`","as":"wide","scope":"tenant"}`)
	del := `{"op":"delete_template","template":"mine","scope":"user","owner":"alice"}`

	member := mcpCtx("acme", "carol", auth.ScopeRunsCreate)
	if c := refusalClass(f.call(t, member, `{"op":"templates","all_users":true}`)); c != "not_granted" {
		t.Errorf("a member listing every user's templates = %q, want not_granted", c)
	}
	if c := refusalClass(f.call(t, member, del)); c != "not_granted" {
		t.Errorf("a member deleting another user's template = %q, want not_granted", c)
	}
	globex := mcpCtx("globex", "olga", auth.ScopeRunsCreate, auth.ScopeTenant)
	if got := f.mustOK(t, globex, `{"op":"templates","all_users":true}`).Text; got != `{"templates":[]}` {
		t.Errorf("another tenant's operator lists = %s", got)
	}
	if c := refusalClass(f.call(t, globex, del)); c != "template_not_found" {
		t.Errorf("another tenant's operator deleting = %q, want template_not_found", c)
	}
	bobRun := f.runCtx(t, "bob", "", tplGrant)
	inRun := auth.WithPrincipal(bobRun, auth.Principal{TenantID: "acme", Subject: "bob", Scopes: []string{auth.ScopeTenant}})
	if c := refusalClass(f.call(t, inRun, del)); c != "not_granted" {
		t.Errorf("a run started by a tenant operator deleting another's template = %q, want not_granted", c)
	}

	op := mcpCtx("acme", "olga", auth.ScopeRunsCreate, auth.ScopeTenant)
	got := f.mustOK(t, op, `{"op":"templates","all_users":true}`).Text
	if !strings.Contains(got, `"template":"mine"`) || !strings.Contains(got, `"owner":"alice"`) || !strings.Contains(got, `"template":"wide"`) {
		t.Errorf("the operator's list = %s", got)
	}
	f.mustOK(t, op, del)
	if c := refusalClass(f.call(t, alice, `{"op":"open","name":"x","template":"mine"}`)); c != "template_not_found" {
		t.Errorf("alice's template after the operator's delete = %q", c)
	}
	var line *audit.Event
	for i, e := range rec.evs {
		if e.Action == "workspace_template_delete" {
			line = &rec.evs[i]
		}
	}
	if line == nil || line.ActorSubject != "olga" || line.TargetSubject != "alice" || line.TargetTenant != "acme" {
		t.Errorf("the delete's audit line = %+v, want the operator olga and the owner alice", line)
	}
}

// An owner argument is the operator's alone; naming yourself is a plain
// delete, and all_users/owner are refused where they do not belong.
func TestWorkspaceTemplateOperator_StrictArguments(t *testing.T) {
	f, alice, _ := tplFixture(t)
	for _, in := range []string{
		`{"op":"delete_template","template":"x","scope":"tenant","owner":"bob"}`,
		`{"op":"templates","owner":"alice"}`,
	} {
		if c := refusalClass(f.call(t, alice, in)); c != "invalid_argument" {
			t.Errorf("%s = %q, want invalid_argument", in, c)
		}
	}
}
