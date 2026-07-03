package credential

import (
	"context"
	"testing"
)

func TestEngine_Substitute(t *testing.T) {
	e := newEngine(t, testKey(1))
	ctx := context.Background()
	if _, err := e.PutInline(ctx, Identity{TenantID: "t", Scope: "tenant", Name: "serper"}, "SK-tenant", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := e.PutInline(ctx, Identity{TenantID: "t", Scope: "user", ScopeID: "uA", Name: "telegram"}, "A-TOKEN", nil); err != nil {
		t.Fatal(err)
	}

	// Resolves a $cred: ref for the run's user; registers the value.
	var registered []string
	out, unresolved, err := e.Substitute(ctx, "t", "", "uA", "Bearer $cred:telegram", func(v string) { registered = append(registered, v) })
	if err != nil || len(unresolved) != 0 {
		t.Fatalf("Substitute: unresolved=%v err=%v", unresolved, err)
	}
	if out != "Bearer A-TOKEN" {
		t.Errorf("out = %q, want Bearer A-TOKEN", out)
	}
	if len(registered) != 1 || registered[0] != "A-TOKEN" {
		t.Errorf("register got %v, want [A-TOKEN]", registered)
	}

	// A different user with no override falls back to the tenant credential.
	if out, _, _ := e.Substitute(ctx, "t", "", "uB", "$cred:serper", nil); out != "SK-tenant" {
		t.Errorf("tenant fallback = %q, want SK-tenant", out)
	}

	// An unresolved ref is left literal and reported (caller drops the header).
	out, unresolved, _ = e.Substitute(ctx, "t", "", "", "$cred:absent", nil)
	if out != "$cred:absent" || len(unresolved) != 1 || unresolved[0] != "absent" {
		t.Errorf("unresolved handling = (%q, %v)", out, unresolved)
	}

	// No ref → passthrough, no work.
	if out, u, err := e.Substitute(ctx, "t", "", "", "plain text", nil); out != "plain text" || u != nil || err != nil {
		t.Errorf("passthrough = (%q, %v, %v)", out, u, err)
	}

	// Multiple refs in one string.
	if out, _, _ := e.Substitute(ctx, "t", "", "uA", "$cred:serper|$cred:telegram", nil); out != "SK-tenant|A-TOKEN" {
		t.Errorf("multi-ref = %q", out)
	}
}
