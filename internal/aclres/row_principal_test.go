package aclres

import (
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
)

// A named subject is evaluated in the class its user row gives it.
func TestRowPrincipal_TakesTheClassOfTheUserRow(t *testing.T) {
	for _, c := range []struct{ mode, want string }{{"isolated", "isolated"}, {"tenant", "member"}, {"", "member"}} {
		if got := ClassKey(RowPrincipal("acme", "alice", c.mode)); got != c.want {
			t.Errorf("access_mode %q: class %q, want %q", c.mode, got, c.want)
		}
	}
}

func TestIsOperatorClass_OnlyAnUnconfinedTenantOperatorOrAdmin(t *testing.T) {
	op := ClassKey(auth.Principal{Subject: "o", Scopes: []string{auth.ScopeTenant}})
	confined := op + runPlaneSuffix
	for key, want := range map[string]bool{"admin": true, op: true, confined: false, "member": false, "isolated": false, "authmanager": false, "": false} {
		if got := IsOperatorClass(key); got != want {
			t.Errorf("IsOperatorClass(%q) = %v, want %v", key, got, want)
		}
	}
}
