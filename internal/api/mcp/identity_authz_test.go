package mcp

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/aclres"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
	loommcp "github.com/denn-gubsky/loomcycle/internal/tools/mcp"
)

// TestIdentity_TenantsIsRefusedForANonAdmin pins the one sub-op that is
// cross-tenant.
//
// `identity` is classified tenant-confinable so its useful ops stay reachable —
// the tenant comes from the principal and there is no wire field, so the
// directory ops physically cannot escape. But op=tenants IS cross-tenant
// information, and it is REFUSED for a non-admin rather than filtered: an empty
// list would be a lie, and a one-entry list would still confirm the caller's own
// tenant in a shape they cannot distinguish from "you are the only tenant".
func TestIdentity_TenantsIsRefusedForANonAdmin(t *testing.T) {
	env := &handlerEnv{connector: &mockConnector{}, logger: discardLogger()}

	tenantCtx := auth.WithPrincipal(context.Background(), auth.Principal{
		TenantID: "acme", Subject: "op", Scopes: []string{auth.ScopeTenant},
	})
	res, err := handleIdentity(tenantCtx, env, json.RawMessage(`{"op":"tenants"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatal("a substrate:tenant principal enumerated tenants — the list itself is " +
			"cross-tenant information")
	}
	txt := dirText(res)
	if !strings.Contains(txt, "operator-admin") {
		t.Errorf("the refusal does not say what is required: %s", txt)
	}

	// The confined ops stay reachable for that same principal — a refusal that took
	// the whole tool with it would be the wrong fix.
	for _, args := range []string{`{"op":"users"}`, `{"op":"held","subject":"alice"}`} {
		res, err := handleIdentity(tenantCtx, env, json.RawMessage(args))
		if err != nil {
			t.Fatal(err)
		}
		if res.IsError {
			t.Errorf("%s was refused for a tenant principal: %s", args, dirText(res))
		}
	}

	// An admin may enumerate.
	adminCtx := auth.WithPrincipal(context.Background(), auth.Principal{
		TenantID: "ops", Subject: "root", Scopes: []string{auth.ScopeAdmin},
	})
	res, err = handleIdentity(adminCtx, env, json.RawMessage(`{"op":"tenants"}`))
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Errorf("an admin was refused op=tenants: %s", dirText(res))
	}
}

// TestIdentity_TenantComesFromThePrincipal — the tool must not take a tenant
// argument, because a subject id is only unique within one tenant.
func TestIdentity_TenantComesFromThePrincipal(t *testing.T) {
	mc := &mockConnector{}
	env := &handlerEnv{connector: mc, logger: discardLogger()}
	ctx := auth.WithPrincipal(context.Background(), auth.Principal{
		TenantID: "acme", Subject: "op", Scopes: []string{auth.ScopeTenant},
	})
	// A wire tenant is passed and must be IGNORED, not honoured.
	if _, err := handleIdentity(ctx, env,
		json.RawMessage(`{"op":"held","subject":"alice","tenant":"globex"}`)); err != nil {
		t.Fatal(err)
	}
	if got, _ := mc.identityTenant.Load().(string); got != "acme" {
		t.Errorf("the tool saw tenant %q — want the principal's", got)
	}
}

// identityDirectory is a store with alice (acme) and a principal on ctx with
// its compiled set, as the MCP route's auth stamps it.
func identityDirectory(t *testing.T, mode authz.Mode) (*mockConnector, func(p auth.Principal) context.Context, store.Store) {
	t.Helper()
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "identity.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.UserCreate(context.Background(), store.UserRow{TenantID: "acme", Subject: "alice", DisplayName: "Alice", AccessMode: "tenant", Status: "active"}); err != nil {
		t.Fatal(err)
	}
	res := aclres.New(st, mode, 0)
	mc := &mockConnector{identityTool: &builtin.Identity{Store: st, ACL: res}}
	ctxFor := func(p auth.Principal) context.Context {
		resolved, err := res.Resolve(context.Background(), p)
		if err != nil {
			t.Fatal(err)
		}
		ctx := authz.WithTransport(authz.WithMode(context.Background(), mode), "mcp")
		return auth.WithPrincipal(ctx, resolved)
	}
	return mc, ctxFor, st
}

// The replacement widens nothing: the former directory's op=inspect, now
// op=held, needs the grant to manage principals. A tenant operator holds it
// by class; a list may withhold it; a member never had it.
func TestIdentity_HeldOverMCPNeedsTheManageGrant(t *testing.T) {
	op := auth.Principal{TenantID: "acme", Subject: "op", TokenDefID: "def_op", Scopes: []string{auth.ScopeTenant}}
	member := auth.Principal{TenantID: "acme", Subject: "bob", TokenDefID: "def_bob", Scopes: []string{auth.ScopeRunsCreate, auth.ScopeRunsRead}}
	held := json.RawMessage(`{"op":"held","subject":"alice"}`)

	mc, ctxFor, st := identityDirectory(t, authz.ModeCompat)
	env := &handlerEnv{connector: mc, logger: discardLogger()}
	if res, _ := handleIdentity(ctxFor(op), env, held); res.IsError {
		t.Errorf("a tenant operator was refused held: %s", dirText(res))
	}
	res, _ := handleIdentity(ctxFor(member), env, held)
	if !res.IsError || !strings.Contains(dirText(res), "identity_not_found") {
		t.Errorf("a member read what is held for another: %s", dirText(res))
	}

	if _, err := st.AclDefPut(context.Background(), store.AclDefRow{TenantID: "acme", Name: "no-manage", Rules: json.RawMessage(
		`[{"effect":"deny","actions":["manage"],"resources":["principal:user:*"]}]`)}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AclBindingPut(context.Background(), store.AclBindingRow{TenantID: "acme", ACLName: "no-manage", SubjectKind: "user", SubjectID: "op"}); err != nil {
		t.Fatal(err)
	}
	mc, ctxFor, _ = identityDirectoryOn(t, st, authz.ModeEnforce)
	env = &handlerEnv{connector: mc, logger: discardLogger()}
	res, _ = handleIdentity(ctxFor(op), env, held)
	if !res.IsError || !strings.Contains(dirText(res), "identity_not_found") {
		t.Errorf("a tenant token without manage read held: %s", dirText(res))
	}
	// users stays: it is the directory, not what is held.
	if res, _ := handleIdentity(ctxFor(op), env, json.RawMessage(`{"op":"users","active_only":true}`)); res.IsError {
		t.Errorf("users was refused: %s", dirText(res))
	}
}

// identityDirectoryOn is identityDirectory over an existing store.
func identityDirectoryOn(t *testing.T, st store.Store, mode authz.Mode) (*mockConnector, func(p auth.Principal) context.Context, store.Store) {
	t.Helper()
	res := aclres.New(st, mode, 0)
	mc := &mockConnector{identityTool: &builtin.Identity{Store: st, ACL: res}}
	return mc, func(p auth.Principal) context.Context {
		resolved, err := res.Resolve(context.Background(), p)
		if err != nil {
			t.Fatal(err)
		}
		return auth.WithPrincipal(authz.WithTransport(authz.WithMode(context.Background(), mode), "mcp"), resolved)
	}, st
}

// The `directory` tool is gone, with no alias.
func TestIdentity_ReplacesDirectory(t *testing.T) {
	if _, ok := handlersByName["directory"]; ok {
		t.Error("the directory handler is still registered")
	}
	names := map[string]bool{}
	for _, td := range toolDescriptors() {
		names[td.Name] = true
	}
	if names["directory"] || !names["identity"] {
		t.Errorf("tools/list: directory=%v identity=%v", names["directory"], names["identity"])
	}
	var schema struct {
		Properties struct {
			Op struct {
				Enum []string `json:"enum"`
			} `json:"op"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(identityMCPSchema(), &schema); err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"users": false, "held": false, "tenants": false}
	for _, op := range schema.Properties.Op.Enum {
		if _, ok := want[op]; ok {
			want[op] = true
		}
	}
	for op, ok := range want {
		if !ok {
			t.Errorf("identity has no op %s", op)
		}
	}
}

// dirText pulls the first content block's text from a tool result.
func dirText(res *loommcp.CallToolResult) string {
	if res == nil || len(res.Content) == 0 {
		return ""
	}
	return res.Content[0].Text
}
