package http

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

func TestIssueGateToken_IsTheUsersShortLivedTaggedToken(t *testing.T) {
	m := config.AuthIdentityMap{Tenant: "acme", Provision: config.ProvisionAuto}
	m.NewUser = config.AuthNewUser{AccessMode: "isolated", Status: "active", TokenTTL: 2 * time.Hour}
	s, st, _ := identitySessionServer(t, m)
	ctx := context.Background()
	if err := st.UserCreate(ctx, store.UserRow{TenantID: "acme", Subject: "google:1", AccessMode: "tenant", Status: "active"}); err != nil {
		t.Fatal(err)
	}
	// An owner binding gives a SESSION operator power; an issued token
	// stays a member token.
	if _, err := st.AclBindingPut(ctx, ownerBinding("acme", "google:1", "test")); err != nil {
		t.Fatal(err)
	}
	before := time.Now()
	tok, row, err := s.issueGateToken(ctx, "acme", "google:1", "google", 24*time.Hour, auth.Restrictions{})
	if err != nil {
		t.Fatal(err)
	}
	if row.IssuedByGate != "google" || row.Name != "u-gate-google" {
		t.Fatalf("row = %+v", row)
	}
	if row.ExpiresAt.Before(before.Add(2*time.Hour-time.Minute)) || row.ExpiresAt.After(time.Now().Add(2*time.Hour)) {
		t.Fatalf("ttl not clamped to the gate's token_ttl: %v", row.ExpiresAt)
	}
	p, ok := s.resolvePrincipal(ctx, tok)
	if !ok || p.TenantID != "acme" || p.Subject != "google:1" {
		t.Fatalf("token resolves to %+v %v", p, ok)
	}
	if auth.HasScope(p.Scopes, auth.ScopeTenant) || auth.HasScope(p.Scopes, auth.ScopeAdmin) {
		t.Fatalf("issued token carries operator scopes: %v", p.Scopes)
	}
	if got, _ := st.OperatorTokenDefGet(ctx, row.DefID); !isDelegatedUserToken(got, "acme", "google:1") {
		t.Fatal("issued token is not retired with the user's delegated tokens")
	}
	// The operator's cap bounds it further; zero asks for the gate's cap.
	s.SetUserTokenMaxTTL(time.Hour)
	_, row, err = s.issueGateToken(ctx, "acme", "google:1", "google", 0, auth.Restrictions{})
	if err != nil || row.ExpiresAt.After(time.Now().Add(time.Hour)) || row.ExpiresAt.IsZero() {
		t.Fatalf("max ttl: %v %v", row.ExpiresAt, err)
	}
}

func TestIssueGateToken_RefusesInactiveUsersAndUnknownGates(t *testing.T) {
	s, st, _ := identitySessionServer(t, config.AuthIdentityMap{Tenant: "acme"})
	ctx := context.Background()
	if _, _, err := s.issueGateToken(ctx, "acme", "nobody", "google", 0, auth.Restrictions{}); !errors.Is(err, errGateTokenUser) {
		t.Fatalf("unknown user: %v", err)
	}
	for _, status := range []string{"disabled", store.UserStatusPending} {
		sub := "u-" + status
		if err := st.UserCreate(ctx, store.UserRow{TenantID: "acme", Subject: sub, AccessMode: "isolated", Status: status}); err != nil {
			t.Fatal(err)
		}
		if _, _, err := s.issueGateToken(ctx, "acme", sub, "google", 0, auth.Restrictions{}); !errors.Is(err, errGateTokenUser) {
			t.Fatalf("%s user: %v", status, err)
		}
	}
	if err := st.UserCreate(ctx, store.UserRow{TenantID: "acme", Subject: "ok", AccessMode: "isolated", Status: "active"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.issueGateToken(ctx, "acme", "ok", "nope", 0, auth.Restrictions{}); !errors.Is(err, errGateTokenGate) {
		t.Fatalf("unknown gate: %v", err)
	}
	empty := []string{}
	if _, _, err := s.issueGateToken(ctx, "acme", "ok", "google", 0, auth.Restrictions{Agents: empty}); err == nil {
		t.Fatal("an invalid restriction was accepted")
	}
}
