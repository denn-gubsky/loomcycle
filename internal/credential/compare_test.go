package credential

import (
	"context"
	"errors"
	"testing"
)

// ResolveToCompare reads the tenant level of one tenant only, and opens a
// credential restricted to no URL at all — which every resolver for a
// destination refuses — because a value only compared is sent nowhere.
// HasToCompare answers the same question without opening anything.
func TestResolveToCompare_ReadsTheTenantLevelWhateverTheRestriction(t *testing.T) {
	e := newEngine(t, testKey(1))
	ctx := context.Background()
	nowhere := &[]string{}
	put := func(id Identity, value string) {
		t.Helper()
		if _, err := e.PutInline(ctx, id, value, nil, nowhere, nil); err != nil {
			t.Fatal(err)
		}
	}
	put(Identity{TenantID: "acme", Scope: "tenant", Name: "hook"}, "tenant-secret")
	put(Identity{TenantID: "acme", Scope: "user", ScopeID: "u1", Name: "user-only"}, "user-secret")
	put(Identity{TenantID: "acme", Scope: "agent", ScopeID: "a1", Name: "agent-only"}, "agent-secret")

	v, found, err := e.ResolveToCompare(ctx, "acme", "hook")
	if err != nil || !found || v != "tenant-secret" {
		t.Fatalf("the tenant's restricted credential: %q %v %v", v, found, err)
	}
	if _, _, err := e.Resolve(ctx, "acme", "", "", "hook"); !errors.Is(err, ErrRestricted) {
		t.Fatalf("Resolve of the same credential: %v, want ErrRestricted (it may be sent nowhere)", err)
	}
	for _, tc := range []struct{ tenant, name string }{
		{"other", "hook"},     // another tenant's
		{"acme", "user-only"}, // a user's, not the tenant's
		{"acme", "agent-only"},
		{"acme", "absent"},
	} {
		if _, found, err := e.ResolveToCompare(ctx, tc.tenant, tc.name); found || err != nil {
			t.Errorf("ResolveToCompare(%s, %s): found=%v err=%v, want not found", tc.tenant, tc.name, found, err)
		}
		if has, err := e.HasToCompare(ctx, tc.tenant, tc.name); has || err != nil {
			t.Errorf("HasToCompare(%s, %s): %v %v, want false", tc.tenant, tc.name, has, err)
		}
	}
	if has, err := e.HasToCompare(ctx, "acme", "hook"); !has || err != nil {
		t.Errorf("HasToCompare(acme, hook): %v %v, want true", has, err)
	}

	// No key: nothing can be opened, and nothing is reported found.
	off := newEngine(t, "")
	if _, found, err := off.ResolveToCompare(ctx, "acme", "hook"); found || err != nil {
		t.Errorf("an engine with no key: found=%v err=%v", found, err)
	}
}
