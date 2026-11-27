package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/workspace/lifecycle"
)

// mcpCtx is a session of loomcycle's MCP server for (tenant, subject), with
// the grant the server gives a session plus templates (RFC GW D35, D-P6-24).
func mcpCtx(tenant, subject string, scopes ...string) context.Context {
	ctx := auth.WithPrincipal(context.Background(), auth.Principal{TenantID: tenant, Subject: subject, Scopes: scopes, TokenDefID: "tok-" + subject})
	ctx = tools.WithRunPolicy(ctx, tools.RunPolicy{Workspace: tools.WorkspacePolicyValue{Read: true, Write: true, Open: true, Templates: true}})
	return lifecycle.WithPrincipalOwner(ctx, lifecycle.PrincipalOwner(tenant, subject, "tok-"+subject))
}

// mcpCheckpoint opens name in the session, writes a file and checkpoints it.
func mcpCheckpoint(t *testing.T, f *wsFixture, ctx context.Context, name, content string) string {
	t.Helper()
	f.mustOK(t, ctx, `{"op":"open","name":"`+name+`"}`)
	f.mustOK(t, ctx, `{"op":"write","name":"`+name+`","path":"notes.txt","content":"`+content+`"}`)
	var ck struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(f.mustOK(t, ctx, `{"op":"checkpoint","name":"`+name+`"}`).Text), &ck); err != nil {
		t.Fatal(err)
	}
	return ck.ID
}

// D-P6-24 (ruled 2026-11-25): over the MCP server a principal saves a user
// template from its own checkpoint and lists and opens its own user
// templates and its tenant's, as that token's tenant and subject; another
// subject and another tenant reach none of them.
func TestWorkspaceTemplateMCP_APrincipalsTemplatesAreItsOwn(t *testing.T) {
	f, alice, ck := tplFixture(t)
	f.mustOK(t, withMemoryScopes(alice, "user", "tenant"), `{"op":"save_template","name":"work","checkpoint":"`+ck+`","as":"tenant-wide","scope":"tenant"}`)

	carol := mcpCtx("acme", "carol", auth.ScopeRunsCreate)
	cck := mcpCheckpoint(t, f, carol, "pw", "carol's")
	f.mustOK(t, carol, `{"op":"save_template","name":"pw","checkpoint":"`+cck+`","as":"mine","scope":"user"}`)
	got := f.mustOK(t, carol, `{"op":"templates"}`).Text
	if !strings.Contains(got, `"mine"`) || !strings.Contains(got, `"tenant-wide"`) {
		t.Errorf("the principal's list = %s", got)
	}
	f.mustOK(t, carol, `{"op":"open","name":"copy","template":"mine"}`)
	if got := f.mustOK(t, carol, `{"op":"read","name":"copy","path":"notes.txt"}`).Text; got != "carol's" {
		t.Errorf("the principal's opened copy = %q", got)
	}
	f.mustOK(t, carol, `{"op":"open","name":"from-tenant","template":"tenant-wide"}`)

	dave := mcpCtx("acme", "dave", auth.ScopeRunsCreate)
	if c := refusalClass(f.call(t, dave, `{"op":"open","name":"x","template":"mine"}`)); c != "template_not_found" {
		t.Errorf("another subject opening carol's template = %q, want template_not_found", c)
	}
	if got := f.mustOK(t, dave, `{"op":"templates"}`).Text; strings.Contains(got, `"mine"`) {
		t.Errorf("another subject's list = %s", got)
	}
	if c := refusalClass(f.call(t, dave, `{"op":"delete_template","template":"mine","scope":"user"}`)); c != "template_not_found" {
		t.Errorf("another subject deleting carol's template = %q, want template_not_found", c)
	}
	globex := mcpCtx("globex", "carol", auth.ScopeRunsCreate)
	if got := f.mustOK(t, globex, `{"op":"templates"}`).Text; got != `{"templates":[]}` {
		t.Errorf("the same subject in another tenant lists = %s", got)
	}
	for _, name := range []string{"mine", "tenant-wide"} {
		if c := refusalClass(f.call(t, globex, `{"op":"open","name":"x","template":"`+name+`"}`)); c != "template_not_found" {
			t.Errorf("another tenant opening %q = %q, want template_not_found", name, c)
		}
	}
	// A principal's checkpoint is its own: it cannot save a run's workspace.
	if c := refusalClass(f.call(t, carol, `{"op":"save_template","name":"work","checkpoint":"`+ck+`","as":"stolen","scope":"user"}`)); c != "not_found" {
		t.Errorf("a principal saving a run's workspace = %q, want not_found", c)
	}
	f.mustOK(t, carol, `{"op":"delete_template","template":"mine","scope":"user"}`)
}

// D-P6-43: a principal saves or deletes a TENANT template only with a
// tenant-operator token (substrate:tenant, or substrate:admin); a member
// token is refused both.
func TestWorkspaceTemplateMCP_TenantTemplatesNeedATenantOperatorToken(t *testing.T) {
	f, alice, ck := tplFixture(t)
	f.mustOK(t, withMemoryScopes(alice, "user", "tenant"), `{"op":"save_template","name":"work","checkpoint":"`+ck+`","as":"tenant-wide","scope":"tenant"}`)

	member := mcpCtx("acme", "carol", auth.ScopeRunsCreate)
	mck := mcpCheckpoint(t, f, member, "pw", "x")
	if c := refusalClass(f.call(t, member, `{"op":"save_template","name":"pw","checkpoint":"`+mck+`","as":"t2","scope":"tenant"}`)); c != "not_granted" {
		t.Errorf("a member token saving a tenant template = %q, want not_granted", c)
	}
	if c := refusalClass(f.call(t, member, `{"op":"delete_template","template":"tenant-wide","scope":"tenant"}`)); c != "not_granted" {
		t.Errorf("a member token deleting a tenant template = %q, want not_granted", c)
	}

	operator := mcpCtx("acme", "olga", auth.ScopeRunsCreate, auth.ScopeTenant)
	ock := mcpCheckpoint(t, f, operator, "pw", "y")
	f.mustOK(t, operator, `{"op":"save_template","name":"pw","checkpoint":"`+ock+`","as":"t3","scope":"tenant"}`)
	f.mustOK(t, operator, `{"op":"delete_template","template":"tenant-wide","scope":"tenant"}`)
}
