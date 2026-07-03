package limits

import "testing"

// TestResolveWrite_TenantConfinement locks the RFC AW limit-write authz shared
// by the HTTP /v1/_limits handler and the gRPC TokenLimit RPC: a full-authority
// caller may target any tenant + the operator scope; a scoped caller is confined
// to its own tenant and cannot touch the operator-global budget. Fails if the
// confinement rule regresses (a tenant operator writing a foreign/operator row).
func TestResolveWrite_TenantConfinement(t *testing.T) {
	cases := []struct {
		name                        string
		scope, wireTenant, scope_id string
		caller                      string
		all                         bool
		wantTenant, wantScopeID     string
		wantErr                     bool
		wantForbidden               bool
	}{
		// Full-authority (admin/legacy/open).
		{name: "admin operator", scope: "operator", caller: "ops", all: true, wantTenant: "", wantScopeID: ""},
		{name: "admin tenant any", scope: "tenant", wireTenant: "evil", caller: "ops", all: true, wantTenant: "evil"},
		{name: "admin user any", scope: "user", wireTenant: "evil", scope_id: "u1", caller: "ops", all: true, wantTenant: "evil", wantScopeID: "u1"},

		// Scoped substrate:tenant caller.
		{name: "scoped operator forbidden", scope: "operator", caller: "acme", all: false, wantErr: true, wantForbidden: true},
		{name: "scoped own tenant (no wire)", scope: "tenant", caller: "acme", all: false, wantTenant: "acme"},
		{name: "scoped own tenant (matching wire)", scope: "tenant", wireTenant: "acme", caller: "acme", all: false, wantTenant: "acme"},
		{name: "scoped cross-tenant forbidden", scope: "tenant", wireTenant: "evil", caller: "acme", all: false, wantErr: true, wantForbidden: true},
		{name: "scoped own user", scope: "user", scope_id: "u1", caller: "acme", all: false, wantTenant: "acme", wantScopeID: "u1"},
		{name: "scoped cross-tenant user forbidden", scope: "user", wireTenant: "evil", scope_id: "u1", caller: "acme", all: false, wantErr: true, wantForbidden: true},

		// Shape validation (bad_request, not forbidden).
		{name: "unknown scope", scope: "bogus", caller: "ops", all: true, wantErr: true, wantForbidden: false},
		{name: "tenant scope with scope_id", scope: "tenant", scope_id: "nope", caller: "ops", all: true, wantErr: true, wantForbidden: false},
		{name: "user scope missing scope_id", scope: "user", caller: "ops", all: true, wantErr: true, wantForbidden: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotTenant, gotScopeID, err := ResolveWrite(tc.scope, tc.wireTenant, tc.scope_id, tc.caller, tc.all)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got (%q,%q,nil)", gotTenant, gotScopeID)
				}
				if err.Forbidden != tc.wantForbidden {
					t.Fatalf("Forbidden=%v, want %v (msg %q)", err.Forbidden, tc.wantForbidden, err.Msg)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if gotTenant != tc.wantTenant || gotScopeID != tc.wantScopeID {
				t.Fatalf("got (%q,%q), want (%q,%q)", gotTenant, gotScopeID, tc.wantTenant, tc.wantScopeID)
			}
		})
	}
}
