package tools

import (
	"context"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
)

// The author's restriction is the more restrictive of the run's own bits and
// the live principal's — never the principal alone, which a resumed run does
// not have and a started draft has in a looser form.
func TestAuthorRestriction_MostRestrictiveOfRunAndPrincipalWins(t *testing.T) {
	// A granular token without the operator-key scope: restricted once the
	// gate is on. substrate:user alone makes it isolated.
	restricted := auth.Principal{TenantID: "acme", Subject: "alice", Scopes: []string{auth.ScopeRunsCreate, auth.ScopeUser}}
	// substrate:tenant implies the operator-key scope and is not isolated.
	loose := auth.Principal{TenantID: "acme", Subject: "bob", Scopes: []string{auth.ScopeTenant}}
	runBits := RunIdentityValue{TenantID: "acme", UserID: "alice", OperatorKeyRestricted: true, Isolated: true}

	cases := []struct {
		name           string
		ctx            context.Context
		gateOn         bool
		wantRestricted bool
		wantIsolated   bool
	}{
		{"open mode: no principal, no run", context.Background(), true, false, false},
		{"resumed run: run bits, no principal", WithRunIdentity(context.Background(), runBits), true, true, true},
		{"run bits survive a looser principal", auth.WithPrincipal(WithRunIdentity(context.Background(), runBits), loose), true, true, true},
		{"off-run restricted principal", auth.WithPrincipal(context.Background(), restricted), true, true, true},
		{"off-run loose principal", auth.WithPrincipal(context.Background(), loose), true, false, false},
		{"live run: principal and run agree", auth.WithPrincipal(WithRunIdentity(context.Background(), RunIdentityValue{TenantID: "acme"}), loose), true, false, false},
		{"gate off: principal is not restricted", auth.WithPrincipal(context.Background(), restricted), false, false, true},
		{"gate off: a recorded run bit still binds", WithRunIdentity(context.Background(), runBits), false, true, true},
	}
	for _, c := range cases {
		if got := AuthorOperatorKeyRestricted(c.ctx, c.gateOn); got != c.wantRestricted {
			t.Errorf("%s: AuthorOperatorKeyRestricted = %v, want %v", c.name, got, c.wantRestricted)
		}
		if got := AuthorIsolated(c.ctx); got != c.wantIsolated {
			t.Errorf("%s: AuthorIsolated = %v, want %v", c.name, got, c.wantIsolated)
		}
	}
}
