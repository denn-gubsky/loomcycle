package http

import (
	"context"
	"net/http"
	"slices"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// TestMemberReadGate_AdmitsUsersAndOperators is the RFC BY gate contract: the
// user-facing read surfaces are gated member-read (runs:read), so a DELEGATED
// USER token — isolated OR tenant-mode — reaches them, as do tenant operators
// and admin. Keying the gate on substrate:tenant (the old /v1/_history posture)
// would lock BOTH user types out; keying on substrate:user would lock the
// tenant-mode user out (its token holds neither). A token that cannot read runs
// at all (channel-only) is correctly denied. Reverting /v1/_history to
// ScopeTenant must break the isolated + tenant-mode cases.
func TestMemberReadGate_AdmitsUsersAndOperators(t *testing.T) {
	gate := requiredScopeFor(http.MethodPost, "/v1/_history")
	if gate != auth.ScopeRunsRead {
		t.Fatalf("history gate = %q, want %q (member-read)", gate, auth.ScopeRunsRead)
	}
	if g := requiredScopeFor(http.MethodGet, "/v1/_runnable-agents"); g != auth.ScopeRunsRead {
		t.Fatalf("runnable-agents gate = %q, want %q (member-read)", g, auth.ScopeRunsRead)
	}

	isolated, err := auth.GrantableUserScopes("isolated")
	if err != nil {
		t.Fatalf("GrantableUserScopes(isolated): %v", err)
	}
	tenantMode, err := auth.GrantableUserScopes("tenant")
	if err != nil {
		t.Fatalf("GrantableUserScopes(tenant): %v", err)
	}

	for _, tc := range []struct {
		name   string
		scopes []string
		admit  bool
	}{
		{"isolated user (substrate:user)", isolated, true},
		{"tenant-mode user (runs+channel, no substrate:user)", tenantMode, true},
		{"tenant operator", []string{auth.ScopeTenant}, true},
		{"admin", []string{auth.ScopeAdmin}, true},
		// A token that can neither create nor read runs must not reach the
		// user-read surfaces — nothing here is its own.
		{"channel-only (no runs:read)", []string{auth.ScopeChannelRead}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := auth.HasScope(tc.scopes, gate); got != tc.admit {
				t.Errorf("HasScope(%v, %q) = %v, want %v", tc.scopes, gate, got, tc.admit)
			}
		})
	}
}

// TestHistoryScopeCap_ByPrincipalAuthority pins the RFC BY confinement that
// makes /v1/_history safe to open to a member-read token: a delegated USER (a
// member — neither tenant operator nor admin), in EITHER access mode, gets only
// [self, user] history scopes (its own chats). A tenant operator adds `tenant`;
// admin adds cross-tenant `global`. Removing the cap (reverting to the old
// [self, user, tenant] default) must break the two user cases.
func TestHistoryScopeCap_ByPrincipalAuthority(t *testing.T) {
	isolated, _ := auth.GrantableUserScopes("isolated")
	tenantMode, _ := auth.GrantableUserScopes("tenant")

	for _, tc := range []struct {
		name string
		p    auth.Principal
		want []string
	}{
		{"isolated user", auth.Principal{TenantID: "acme", Subject: "alice", Scopes: isolated}, []string{"self", "user"}},
		{"tenant-mode user", auth.Principal{TenantID: "acme", Subject: "bob", Scopes: tenantMode}, []string{"self", "user"}},
		{"tenant operator", auth.Principal{TenantID: "acme", Subject: "op", Scopes: []string{auth.ScopeTenant}}, []string{"self", "user", "tenant"}},
		{"admin", auth.Principal{TenantID: "acme", Subject: "root", Scopes: []string{auth.ScopeAdmin}}, []string{"self", "user", "tenant", "global"}},
		// Legacy holds ScopeAdmin, so it lands in the admin branch (unchanged).
		{"legacy", auth.Principal{TenantID: "default", Subject: "default", Scopes: []string{auth.ScopeAdmin}, Legacy: true}, []string{"self", "user", "tenant", "global"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := substrateAdminCtx(auth.WithPrincipal(context.Background(), tc.p))
			got := tools.HistoryPolicy(ctx).Scopes
			if !slices.Equal(got, tc.want) {
				t.Errorf("history scopes = %v, want %v", got, tc.want)
			}
		})
	}
}
