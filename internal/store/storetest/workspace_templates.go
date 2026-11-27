package storetest

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// RFC GW P6 — workspace templates (store.WorkspaceTemplateStore).

// RunWorkspaceTemplates asserts the workspace_templates contract against a
// backend, each case on a fresh store from factory.
func RunWorkspaceTemplates(t *testing.T, factory Factory) {
	t.Helper()
	for name, fn := range map[string]func(*testing.T, store.Store){
		"CreateReadsBackEveryField":   testWorkspaceTemplateCreateReadsBackEveryField,
		"CreateRefusesAHeldName":      testWorkspaceTemplateCreateRefusesAHeldName,
		"ScopesAndTenantsAreDisjoint": testWorkspaceTemplateScopesAndTenantsAreDisjoint,
		"DeleteIsByIDWithinTheTenant": testWorkspaceTemplateDeleteIsByIDWithinTheTenant,
	} {
		t.Run(name, func(t *testing.T) {
			s, cleanup := factory(t)
			defer cleanup()
			fn(t, s)
		})
	}
}

func mkTemplate(id, tenant, scope, scopeID, name string) store.WorkspaceTemplateRow {
	return store.WorkspaceTemplateRow{ID: id, TenantID: tenant, Scope: scope, ScopeID: scopeID, Name: name,
		Toolchains: []string{"python", "node"}, Engine: "container", Digest: "d-" + id, Bytes: 4096,
		AuthorUser: "alice", AuthorRun: "run_1"}
}

// What a create stores is what get and list return, created_at stamped.
func testWorkspaceTemplateCreateReadsBackEveryField(t *testing.T, s store.Store) {
	ctx := context.Background()
	in := mkTemplate("tpl_1", "acme", store.WorkspaceTemplateUser, "alice", "py")
	got, err := s.WorkspaceTemplateCreate(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if got.CreatedAt.IsZero() {
		t.Error("created_at not stamped")
	}
	got.CreatedAt = in.CreatedAt
	if !reflect.DeepEqual(got, in) {
		t.Errorf("read back %+v, want %+v", got, in)
	}
	list, err := s.WorkspaceTemplateList(ctx, "acme", store.WorkspaceTemplateUser, "alice")
	if err != nil || len(list) != 1 || list[0].ID != "tpl_1" || !reflect.DeepEqual(list[0].Toolchains, in.Toolchains) {
		t.Errorf("list = %+v, %v", list, err)
	}
	if n, err := s.WorkspaceTemplateCount(ctx, "acme"); err != nil || n != 1 {
		t.Errorf("count = %d, %v", n, err)
	}
}

// A template is never overwritten: a held name or id is a conflict.
func testWorkspaceTemplateCreateRefusesAHeldName(t *testing.T, s store.Store) {
	ctx := context.Background()
	if _, err := s.WorkspaceTemplateCreate(ctx, mkTemplate("tpl_1", "acme", store.WorkspaceTemplateTenant, "", "py")); err != nil {
		t.Fatal(err)
	}
	var conflict *store.ErrConflict
	if _, err := s.WorkspaceTemplateCreate(ctx, mkTemplate("tpl_2", "acme", store.WorkspaceTemplateTenant, "", "py")); !errors.As(err, &conflict) {
		t.Errorf("a held name = %v, want ErrConflict", err)
	}
	if _, err := s.WorkspaceTemplateCreate(ctx, mkTemplate("tpl_1", "acme", store.WorkspaceTemplateTenant, "", "other")); !errors.As(err, &conflict) {
		t.Errorf("a held id = %v, want ErrConflict", err)
	}
	got, _ := s.WorkspaceTemplateGet(ctx, "acme", store.WorkspaceTemplateTenant, "", "py")
	if got.ID != "tpl_1" {
		t.Errorf("the held template was replaced: %+v", got)
	}
}

// One name lives in each scope, each user's scope and each tenant apart.
func testWorkspaceTemplateScopesAndTenantsAreDisjoint(t *testing.T, s store.Store) {
	ctx := context.Background()
	for _, r := range []store.WorkspaceTemplateRow{
		mkTemplate("tpl_a", "acme", store.WorkspaceTemplateUser, "alice", "py"),
		mkTemplate("tpl_b", "acme", store.WorkspaceTemplateUser, "bob", "py"),
		mkTemplate("tpl_t", "acme", store.WorkspaceTemplateTenant, "", "py"),
		mkTemplate("tpl_g", "globex", store.WorkspaceTemplateTenant, "", "py"),
	} {
		if _, err := s.WorkspaceTemplateCreate(ctx, r); err != nil {
			t.Fatalf("%s: %v", r.ID, err)
		}
	}
	for _, c := range []struct{ tenant, scope, scopeID, want string }{
		{"acme", store.WorkspaceTemplateUser, "alice", "tpl_a"},
		{"acme", store.WorkspaceTemplateUser, "bob", "tpl_b"},
		{"acme", store.WorkspaceTemplateTenant, "", "tpl_t"},
		{"globex", store.WorkspaceTemplateTenant, "", "tpl_g"},
	} {
		got, err := s.WorkspaceTemplateGet(ctx, c.tenant, c.scope, c.scopeID, "py")
		if err != nil || got.ID != c.want {
			t.Errorf("%s/%s/%s = %+v, %v; want %s", c.tenant, c.scope, c.scopeID, got, err, c.want)
		}
	}
	var nf *store.ErrNotFound
	if _, err := s.WorkspaceTemplateGet(ctx, "globex", store.WorkspaceTemplateUser, "alice", "py"); !errors.As(err, &nf) {
		t.Errorf("alice's template from another tenant = %v, want ErrNotFound", err)
	}
	if n, _ := s.WorkspaceTemplateCount(ctx, "acme"); n != 3 {
		t.Errorf("acme count = %d, want 3", n)
	}
	// A tenant operator's listing: every user's and the tenant's, of this
	// tenant only.
	all, err := s.WorkspaceTemplateListTenant(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, r := range all {
		ids = append(ids, r.ID)
	}
	if got := strings.Join(ids, ","); got != "tpl_t,tpl_a,tpl_b" {
		t.Errorf("acme's whole listing = %s, want tpl_t,tpl_a,tpl_b (tenant, then each user's)", got)
	}
}

// Delete takes the tenant: another tenant's id is not found.
func testWorkspaceTemplateDeleteIsByIDWithinTheTenant(t *testing.T, s store.Store) {
	ctx := context.Background()
	if _, err := s.WorkspaceTemplateCreate(ctx, mkTemplate("tpl_1", "acme", store.WorkspaceTemplateTenant, "", "py")); err != nil {
		t.Fatal(err)
	}
	if found, err := s.WorkspaceTemplateDelete(ctx, "globex", "tpl_1"); err != nil || found {
		t.Errorf("delete from another tenant = %v, %v", found, err)
	}
	if found, err := s.WorkspaceTemplateDelete(ctx, "acme", "tpl_1"); err != nil || !found {
		t.Errorf("delete = %v, %v", found, err)
	}
	if n, _ := s.WorkspaceTemplateCount(ctx, "acme"); n != 0 {
		t.Errorf("count after delete = %d", n)
	}
}
