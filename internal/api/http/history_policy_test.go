package http

import (
	"context"
	"reflect"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/config"
)

// TestHistoryPolicyForAgent_AdminGatesGlobal pins the RFC BE security seam: the
// cross-tenant `global` scope survives policy resolution ONLY under an admin
// principal; every non-admin (tenant / open-mode / no-principal) case drops it,
// while the own-tenant scopes and the "any"->"global" alias are honored.
func TestHistoryPolicyForAgent_AdminGatesGlobal(t *testing.T) {
	s := &Server{}
	adminCtx := auth.WithPrincipal(context.Background(),
		auth.Principal{TenantID: "acme", Subject: "root", Scopes: []string{auth.ScopeAdmin}})
	tenantCtx := auth.WithPrincipal(context.Background(),
		auth.Principal{TenantID: "acme", Subject: "alice", Scopes: []string{auth.ScopeTenant}})
	noPrincipalCtx := context.Background()

	cases := []struct {
		name string
		ctx  context.Context
		yaml []string
		want []string
	}{
		{"admin keeps global", adminCtx, []string{"self", "user", "tenant", "global"}, []string{"self", "user", "tenant", "global"}},
		{"tenant drops global", tenantCtx, []string{"self", "user", "tenant", "global"}, []string{"self", "user", "tenant"}},
		{"no principal fails closed", noPrincipalCtx, []string{"self", "global"}, []string{"self"}},
		{"any alias -> global for admin", adminCtx, []string{"any"}, []string{"global"}},
		{"any alias dropped for tenant", tenantCtx, []string{"any"}, []string{}},
		{"dedup", adminCtx, []string{"self", "self", "any", "global"}, []string{"self", "global"}},
		{"empty stays empty", tenantCtx, nil, []string{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := s.historyPolicyForAgent(tc.ctx, config.AgentDef{HistoryScope: tc.yaml}).Scopes
			if len(got) == 0 && len(tc.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("scopes = %v, want %v", got, tc.want)
			}
		})
	}
}
