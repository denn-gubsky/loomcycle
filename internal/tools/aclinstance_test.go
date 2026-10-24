package tools

import (
	"context"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/authz"
)

// The per-call checks are only as good as the set CallerACL hands them: a
// nil while a set is present turns every one of them off (fail open).
func TestCallerACL_ReturnsTheSetWheneverOneIsPresent(t *testing.T) {
	runSet := compiledSet(t, authz.Rule{Effect: authz.Deny, Actions: []string{"invoke"}, Resources: []string{"tool:Bash"}})
	principalSet := compiledSet(t, authz.Rule{Effect: authz.Deny, Actions: []string{"invoke"}, Resources: []string{"tool:Read"}})
	bg := context.Background()
	withPrincipal := auth.WithPrincipal(bg, auth.Principal{TenantID: "t", Subject: "u", Effective: principalSet})

	if got := CallerACL(WithRunPolicy(bg, RunPolicy{ACL: runSet})); got != runSet {
		t.Errorf("in a run: CallerACL = %p, want the run's set %p", got, runSet)
	}
	if got := CallerACL(withPrincipal); got != principalSet {
		t.Errorf("off a run: CallerACL = %p, want the principal's set %p", got, principalSet)
	}
	// Inside a run the originating principal's set (RunPolicy.ACL) decides,
	// even when the ctx also carries a principal.
	if got := CallerACL(WithRunPolicy(withPrincipal, RunPolicy{ACL: runSet})); got != runSet {
		t.Errorf("in a run with a principal: CallerACL = %p, want the run's set", got)
	}
	if got := CallerACL(bg); got != nil {
		t.Errorf("no set anywhere: CallerACL = %p, want nil", got)
	}
}
