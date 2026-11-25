package builtin

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/workspace/lifecycle"
)

// tplFixture is a workspace fixture whose manager keeps templates, and a
// run of alice whose workspace "work" holds notes.txt and a checkpoint.
func tplFixture(t *testing.T) (*wsFixture, context.Context, string) {
	t.Helper()
	f := newWSFixture(t, lifecycle.Limits{})
	f.tool.Manager.SetTemplateDir(filepath.Join(t.TempDir(), "templates"))
	alice := withMemoryScopes(f.runCtx(t, "alice", "", tplGrant), "user")
	f.mustOK(t, alice, `{"op":"open","name":"work"}`)
	f.mustOK(t, alice, `{"op":"write","name":"work","path":"notes.txt","content":"prepared"}`)
	var ck struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(f.mustOK(t, alice, `{"op":"checkpoint","name":"work"}`).Text), &ck); err != nil {
		t.Fatal(err)
	}
	return f, alice, ck.ID
}

var tplGrant = tools.WorkspacePolicyValue{Read: true, Write: true, Open: true, Templates: true}

func withMemoryScopes(ctx context.Context, scopes ...string) context.Context {
	return tools.UpdateRunPolicy(ctx, func(rp *tools.RunPolicy) { rp.Memory.AllowedScopes = scopes })
}

// RFC GW §5.5: a checkpoint saved as a user template opens as a new
// workspace for a later run of its author, and for no other user (D-P6-24);
// the listing shows each run what it may open.
func TestWorkspaceTemplate_SavedForItsUserAndOpenedByALaterRun(t *testing.T) {
	f, alice, ck := tplFixture(t)
	res := f.mustOK(t, alice, `{"op":"save_template","name":"work","checkpoint":"`+ck+`","as":"py","scope":"user"}`)
	if !strings.Contains(res.Text, `"template":"py"`) || strings.Contains(res.Text, "digest") || strings.Contains(res.Text, "tpl_") {
		t.Errorf("save answer = %s", res.Text)
	}
	if c := refusalClass(f.call(t, alice, `{"op":"save_template","name":"work","checkpoint":"`+ck+`","as":"py","scope":"user"}`)); c != "exists" {
		t.Errorf("a second save of one name = %q, want exists", c)
	}
	later := f.runCtx(t, "alice", "", tplGrant)
	f.mustOK(t, later, `{"op":"open","name":"copy","template":"py"}`)
	if got := f.mustOK(t, later, `{"op":"read","name":"copy","path":"notes.txt"}`).Text; got != "prepared" {
		t.Errorf("the opened workspace's file = %q", got)
	}
	if got := f.mustOK(t, later, `{"op":"templates"}`).Text; !strings.Contains(got, `"template":"py"`) {
		t.Errorf("alice's templates = %s", got)
	}
	bob := f.runCtx(t, "bob", "", tplGrant)
	if c := refusalClass(f.call(t, bob, `{"op":"open","name":"copy","template":"py"}`)); c != "template_not_found" {
		t.Errorf("bob opening alice's user template = %q, want template_not_found", c)
	}
	if got := f.mustOK(t, bob, `{"op":"templates"}`).Text; got != `{"templates":[]}` {
		t.Errorf("bob's templates = %s", got)
	}
	if c := refusalClass(f.call(t, withMemoryScopes(bob, "user"), `{"op":"delete_template","template":"py","scope":"user"}`)); c != "template_not_found" {
		t.Errorf("bob deleting alice's user template = %q, want template_not_found", c)
	}
	f.mustOK(t, later, `{"op":"delete_template","template":"py","scope":"user"}`)
	if c := refusalClass(f.call(t, later, `{"op":"open","name":"again","template":"py"}`)); c != "template_not_found" {
		t.Errorf("opening a deleted template = %q", c)
	}
}

// The grants (RFC GW §5.5, D-P6-26 to D-P6-28): saving and deleting need
// workspace.templates, a tenant template also the tenant value in
// memory_scopes; listing needs open or templates; opening from one needs
// open. Over the MCP server none is offered.
func TestWorkspaceTemplate_Grants(t *testing.T) {
	f, alice, ck := tplFixture(t)
	save := `{"op":"save_template","name":"work","checkpoint":"` + ck + `","as":"t1","scope":"tenant"}`
	if c := refusalClass(f.call(t, alice, save)); c != "not_granted" {
		t.Errorf("a tenant template without the tenant memory scope = %q, want not_granted", c)
	}
	noTemplates := tools.UpdateRunPolicy(alice, func(rp *tools.RunPolicy) { rp.Workspace.Templates = false })
	if c := refusalClass(f.call(t, noTemplates, `{"op":"save_template","name":"work","checkpoint":"`+ck+`","as":"t2","scope":"user"}`)); c != "not_granted" {
		t.Errorf("a save without workspace.templates = %q, want not_granted", c)
	}
	if c := refusalClass(f.call(t, noTemplates, `{"op":"delete_template","template":"t2","scope":"user"}`)); c != "not_granted" {
		t.Errorf("a delete without workspace.templates = %q, want not_granted", c)
	}
	f.mustOK(t, withMemoryScopes(alice, "user", "tenant"), save)
	bob := f.runCtx(t, "bob", "", tools.WorkspacePolicyValue{Read: true, Open: true})
	if got := f.mustOK(t, bob, `{"op":"templates"}`).Text; !strings.Contains(got, `"template":"t1"`) {
		t.Errorf("bob's list of the tenant's templates = %s", got)
	}
	f.mustOK(t, bob, `{"op":"open","name":"from-tenant","template":"t1"}`)
	if c := refusalClass(f.call(t, withMemoryScopes(bob, "tenant"), `{"op":"delete_template","template":"t1","scope":"tenant"}`)); c != "not_granted" {
		t.Errorf("a tenant delete without workspace.templates = %q, want not_granted", c)
	}
	readOnly := f.runCtx(t, "bob", "", tools.WorkspacePolicyValue{Read: true})
	if c := refusalClass(f.call(t, readOnly, `{"op":"templates"}`)); c != "not_granted" {
		t.Errorf("a list with neither open nor templates = %q, want not_granted", c)
	}
	if c := refusalClass(f.call(t, readOnly, `{"op":"open","name":"x","template":"t1"}`)); c != "not_granted" {
		t.Errorf("an open from a template without open = %q, want not_granted", c)
	}
	principal := lifecycle.WithPrincipalOwner(tools.WithRunPolicy(context.Background(), tools.RunPolicy{Workspace: tplGrant}),
		lifecycle.PrincipalOwner("acme", "alice", "tok"))
	for _, in := range []string{`{"op":"open","name":"x","template":"t1"}`, `{"op":"templates"}`,
		`{"op":"save_template","name":"work","checkpoint":"` + ck + `","as":"t3","scope":"user"}`} {
		if c := refusalClass(f.call(t, principal, in)); c != "not_over_mcp" {
			t.Errorf("over MCP %s = %q, want not_over_mcp", in, c)
		}
	}
}

// Arguments are strict: a template carries its own toolchains, scope is
// required, and a save names its checkpoint and template.
func TestWorkspaceTemplate_StrictArguments(t *testing.T) {
	f, alice, ck := tplFixture(t)
	for _, in := range []string{
		`{"op":"open","name":"x","template":"py","toolchain":"python"}`,
		`{"op":"save_template","name":"work","checkpoint":"` + ck + `","as":"py"}`,
		`{"op":"save_template","name":"work","checkpoint":"` + ck + `","as":"py","scope":"everyone"}`,
		`{"op":"save_template","name":"work","as":"py","scope":"user"}`,
		`{"op":"save_template","name":"work","checkpoint":"` + ck + `","scope":"user"}`,
		`{"op":"delete_template","scope":"user"}`,
		`{"op":"templates","name":"work"}`,
	} {
		if c := refusalClass(f.call(t, alice, in)); c != "invalid_argument" {
			t.Errorf("%s = %q, want invalid_argument", in, c)
		}
	}
	if c := refusalClass(f.call(t, alice, `{"op":"save_template","name":"work@a_bob","checkpoint":"`+ck+`","as":"py","scope":"user"}`)); c == "" {
		t.Error("a save from a shared name was accepted")
	}
}

// D-P6-39: an isolated member (substrate:user) reaches no tenant-shared
// data, so it neither lists nor opens a tenant template; its own user
// templates are its.
func TestWorkspaceTemplate_AnIsolatedMemberSeesNoTenantTemplate(t *testing.T) {
	f, alice, ck := tplFixture(t)
	f.mustOK(t, withMemoryScopes(alice, "user", "tenant"), `{"op":"save_template","name":"work","checkpoint":"`+ck+`","as":"shared","scope":"tenant"}`)
	f.mustOK(t, alice, `{"op":"save_template","name":"work","checkpoint":"`+ck+`","as":"own","scope":"user"}`)
	iso := f.runCtx(t, "alice", "", tplGrant)
	id := tools.RunIdentity(iso)
	id.Isolated = true
	iso = tools.WithRunIdentity(iso, id)
	if got := f.mustOK(t, iso, `{"op":"templates"}`).Text; strings.Contains(got, `"shared"`) || !strings.Contains(got, `"own"`) {
		t.Errorf("an isolated member's list = %s", got)
	}
	if c := refusalClass(f.call(t, iso, `{"op":"open","name":"x","template":"shared"}`)); c != "template_not_found" {
		t.Errorf("an isolated member opening a tenant template = %q, want template_not_found", c)
	}
	f.mustOK(t, iso, `{"op":"open","name":"y","template":"own"}`)
}

// The scope report is the call's own check: a scope it refuses is refused
// by save_template with the same reason, and one it grants gets past it.
func TestWorkspaceTemplate_ScopeGrantsAgreeWithTheCall(t *testing.T) {
	f, alice, ck := tplFixture(t)
	n := 0
	for _, ctx := range []context.Context{alice, withMemoryScopes(alice, "user", "tenant"),
		tools.UpdateRunPolicy(alice, func(rp *tools.RunPolicy) { rp.Workspace.Templates = false })} {
		for _, g := range f.tool.ScopeGrants(ctx)[0].Grants {
			n++
			res := f.call(t, ctx, `{"op":"save_template","name":"work","checkpoint":"`+ck+`","as":"g`+strconv.Itoa(n)+`","scope":"`+g.Scope+`"}`)
			if g.Granted == res.IsError {
				t.Errorf("scope %s: report granted=%v, call %s", g.Scope, g.Granted, res.Text)
			}
			if !g.Granted && !strings.Contains(res.Text, g.Reason) {
				t.Errorf("scope %s: reason %q, call %s", g.Scope, g.Reason, res.Text)
			}
		}
	}
}
