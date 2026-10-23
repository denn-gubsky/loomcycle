package aclres

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

func staticFixture() *authz.Static {
	return &authz.Static{
		ACLs: map[string]authz.ACL{
			"no-shell": {Name: "no-shell", Rules: []authz.Rule{{Effect: authz.Deny, Actions: []string{"invoke"}, Resources: []string{"tool:Bash"}}}},
			"no-admin": {Name: "no-admin", Rules: []authz.Rule{{Effect: authz.Deny, Actions: []string{"run"}, Resources: []string{"agent:admin/**"}}}},
		},
		Groups: []authz.StaticGroup{
			{Tenant: "acme", Name: "staff", Groups: []string{"support"}},
			{Tenant: "acme", Name: "support", Members: []string{"alice"}},
		},
		Bindings: []authz.StaticBinding{
			{Tenant: "acme", Binding: authz.Binding{ACL: "no-shell", SubjectKind: authz.SubjectGroup, SubjectID: "staff"}},
			{Tenant: "acme", Binding: authz.Binding{ACL: "no-admin", SubjectKind: authz.SubjectUser, SubjectID: "bob"}},
		},
	}
}

// Config-declared groups and bindings apply beside the store's; a config
// list is reachable from a binding of any tenant; a stored list of the same
// name shadows it.
func TestResolve_StaticLayerUnderTheStore(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	r := New(st, authz.ModeCompat, 0)
	r.SetStatic(staticFixture)

	alice, err := r.Resolve(ctx, member("alice"))
	if err != nil {
		t.Fatal(err)
	}
	if got := alice.Groups; len(got) != 2 || got[0] != "staff" || got[1] != "support" {
		t.Errorf("alice groups = %v", got)
	}
	if decide(alice.Effective, "invoke", "tool", "Bash") != authz.Denied {
		t.Error("the static group binding should deny Bash for alice")
	}
	bob, _ := r.Resolve(ctx, member("bob"))
	if decide(bob.Effective, "run", "agent", "admin/x") != authz.Denied || decide(bob.Effective, "invoke", "tool", "Bash") != authz.Allowed {
		t.Error("the static user binding should deny bob admin agents only")
	}
	// A store binding may name a config list.
	if _, err := st.AclBindingPut(ctx, store.AclBindingRow{TenantID: "acme", ACLName: "no-admin", SubjectKind: "user", SubjectID: "alice"}); err != nil {
		t.Fatal(err)
	}
	alice, _ = r.Resolve(ctx, member("alice"))
	if decide(alice.Effective, "run", "agent", "admin/x") != authz.Denied {
		t.Error("a stored binding should reach the config list")
	}
	// A stored list of the same name shadows the config one.
	if _, err := st.AclDefPut(ctx, store.AclDefRow{TenantID: "acme", Name: "no-shell", Rules: json.RawMessage(`[{"effect":"deny","actions":["invoke"],"resources":["tool:Write"]}]`)}); err != nil {
		t.Fatal(err)
	}
	alice, _ = r.Resolve(ctx, member("alice"))
	if decide(alice.Effective, "invoke", "tool", "Bash") != authz.Allowed || decide(alice.Effective, "invoke", "tool", "Write") != authz.Denied {
		t.Error("the stored no-shell should shadow the config one")
	}
	// A list stored in the shared tenant "" is reachable from acme too.
	if _, err := st.AclDefPut(ctx, store.AclDefRow{TenantID: "", Name: "shared-list", Rules: json.RawMessage(`[{"effect":"deny","actions":["use"],"resources":["gateway:chat"]}]`)}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AclBindingPut(ctx, store.AclBindingRow{TenantID: "acme", ACLName: "shared-list", SubjectKind: "tenant"}); err != nil {
		t.Fatal(err)
	}
	alice, _ = r.Resolve(ctx, member("alice"))
	if decide(alice.Effective, "use", "gateway", "chat") != authz.Denied {
		t.Error("a shared-tenant list should resolve for a tenant binding")
	}
	// Another tenant sees none of acme's static groups or bindings.
	other, _ := r.Resolve(ctx, auth.Principal{TenantID: "other", Subject: "alice", Scopes: []string{auth.ScopeRunsCreate}})
	if len(other.Groups) != 0 || decide(other.Effective, "invoke", "tool", "Bash") != authz.Allowed {
		t.Error("tenant closure over the static layer")
	}
	// No static source: nothing changes.
	r.SetStatic(nil)
	r.Invalidate("")
	alice, _ = r.Resolve(ctx, member("alice"))
	if len(alice.Groups) != 0 {
		t.Errorf("without the static source alice's groups = %v", alice.Groups)
	}
}
