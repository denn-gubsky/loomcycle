package tools

import (
	"reflect"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
)

func TestOffRunEvaluationPolicy_TenantOperatorGetsEveryOpAndAUserOnlySubmitSelf(t *testing.T) {
	every := []string{"submit_self", "submit_descendants", "submit_any", "read_any"}
	self := []string{"submit_self"}
	for _, tc := range []struct {
		name string
		p    auth.Principal
		ok   bool
		want []string
	}{
		{"no principal (open mode, stdio)", auth.Principal{}, false, every},
		{"legacy token", auth.Principal{TenantID: "default", Subject: "default", Scopes: []string{auth.ScopeAdmin}, Legacy: true}, true, every},
		{"admin", auth.Principal{TenantID: "ops", Subject: "root", Scopes: []string{auth.ScopeAdmin}}, true, every},
		{"tenant operator", auth.Principal{TenantID: "acme", Subject: "alice", Scopes: []string{auth.ScopeTenant}}, true, every},
		{"isolated user", auth.Principal{TenantID: "acme", Subject: "bob", Scopes: []string{auth.ScopeUser}}, true, self},
		{"tenant member", auth.Principal{TenantID: "acme", Subject: "carol", Scopes: []string{auth.ScopeRunsCreate, auth.ScopeRunsRead}}, true, self},
		{"principal with no scopes", auth.Principal{TenantID: "acme", Subject: "dave"}, true, self},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := OffRunEvaluationPolicy(tc.p, tc.ok).Scopes; !reflect.DeepEqual(got, tc.want) {
				t.Errorf("scopes = %v, want %v", got, tc.want)
			}
		})
	}
}
