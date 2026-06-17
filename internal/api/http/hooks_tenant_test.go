package http

import (
	"context"
	"errors"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/connector"
)

// tenantCtx returns a context carrying a non-admin principal bound to the given
// tenant (the RFC AF tenant operator). adminCtx / legacy callers use the bare
// t.Context() (no principal → allTenants).
func tenantCtx(tenant string) context.Context {
	return auth.WithPrincipal(context.Background(), auth.Principal{
		TenantID: tenant,
		Subject:  "subj-" + tenant,
		Scopes:   []string{auth.ScopeTenant},
	})
}

func adminCtx() context.Context {
	return auth.WithPrincipal(context.Background(), auth.Principal{
		TenantID: "ops",
		Subject:  "admin",
		Scopes:   []string{auth.ScopeAdmin},
	})
}

func registerHook(t *testing.T, s *Server, ctx context.Context, owner, name string) string {
	t.Helper()
	resp, err := s.RegisterHook(ctx, connector.RegisterHookRequest{
		Owner: owner, Name: name, Phase: "pre",
		Tools: []string{"WebFetch"}, CallbackURL: "https://x/" + name,
	})
	if err != nil {
		t.Fatalf("RegisterHook(%s): %v", name, err)
	}
	return resp.ID
}

// TestConnector_RegisterHook_StampsAuthoritativeTenant locks RFC AF: a non-admin
// tenant operator's hook is stamped with ITS tenant (confined), while an
// admin/legacy registration stamps "" (operator-global, fires on every run).
// The tenant is never taken from a wire field.
func TestConnector_RegisterHook_StampsAuthoritativeTenant(t *testing.T) {
	s := minimalServer(t)

	registerHook(t, s, tenantCtx("jobember"), "jobember-app", "scan")
	registerHook(t, s, adminCtx(), "ops-app", "global-scan")

	byName := map[string]string{} // name → tenant
	for _, h := range s.hookRegistry.List() {
		byName[h.Name] = h.Tenant
	}
	if byName["scan"] != "jobember" {
		t.Errorf("tenant-operator hook tenant = %q, want jobember", byName["scan"])
	}
	if byName["global-scan"] != "" {
		t.Errorf("admin hook tenant = %q, want \"\" (operator-global)", byName["global-scan"])
	}
}

// TestConnector_ListHooks_TenantScoped: a tenant operator sees ONLY its own
// tenant's hooks — not another tenant's, and not the operator/global hooks
// (whose callback URLs are infra it must not introspect). Admin sees all.
func TestConnector_ListHooks_TenantScoped(t *testing.T) {
	s := minimalServer(t)
	registerHook(t, s, tenantCtx("alpha"), "alpha-app", "a-hook")
	registerHook(t, s, tenantCtx("beta"), "beta-app", "b-hook")
	registerHook(t, s, adminCtx(), "ops-app", "global-hook")

	// Tenant alpha sees only a-hook.
	resp, err := s.ListHooks(tenantCtx("alpha"))
	if err != nil {
		t.Fatalf("ListHooks(alpha): %v", err)
	}
	if len(resp.Hooks) != 1 || resp.Hooks[0].Name != "a-hook" {
		t.Errorf("alpha ListHooks = %v, want [a-hook] only (no beta, no global)", hookNamesOf(resp))
	}

	// Admin sees all three.
	adminResp, err := s.ListHooks(adminCtx())
	if err != nil {
		t.Fatalf("ListHooks(admin): %v", err)
	}
	if len(adminResp.Hooks) != 3 {
		t.Errorf("admin ListHooks len = %d, want 3 (sees every tenant + global)", len(adminResp.Hooks))
	}
}

// TestConnector_DeleteHook_CrossTenantOpaque404: a tenant operator cannot delete
// another tenant's (or a global) hook — the cross-tenant id folds into the same
// opaque ErrHookNotFound a missing id returns (no existence oracle). Admin can.
func TestConnector_DeleteHook_CrossTenantOpaque404(t *testing.T) {
	s := minimalServer(t)
	betaID := registerHook(t, s, tenantCtx("beta"), "beta-app", "b-hook")
	globalID := registerHook(t, s, adminCtx(), "ops-app", "global-hook")

	// Tenant alpha tries to delete beta's hook → opaque ErrHookNotFound.
	if err := s.DeleteHook(tenantCtx("alpha"), betaID); !errors.Is(err, connector.ErrHookNotFound) {
		t.Errorf("cross-tenant DeleteHook err = %v, want ErrHookNotFound (opaque)", err)
	}
	// And the global hook is equally invisible to a tenant operator.
	if err := s.DeleteHook(tenantCtx("alpha"), globalID); !errors.Is(err, connector.ErrHookNotFound) {
		t.Errorf("tenant delete of global hook err = %v, want ErrHookNotFound", err)
	}
	// beta's hook must still be registered (the cross-tenant delete was a no-op).
	if got := len(s.hookRegistry.List()); got != 2 {
		t.Errorf("registry len after refused deletes = %d, want 2 (nothing deleted)", got)
	}

	// beta deletes its own hook → ok.
	if err := s.DeleteHook(tenantCtx("beta"), betaID); err != nil {
		t.Errorf("own-tenant DeleteHook err = %v, want nil", err)
	}
	// admin deletes the global hook → ok.
	if err := s.DeleteHook(adminCtx(), globalID); err != nil {
		t.Errorf("admin DeleteHook(global) err = %v, want nil", err)
	}
}

func hookNamesOf(resp connector.ListHooksResponse) []string {
	out := make([]string, 0, len(resp.Hooks))
	for _, h := range resp.Hooks {
		out = append(out, h.Name)
	}
	return out
}
