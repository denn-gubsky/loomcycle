package http

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// A walk's own run row carries the restriction bits its members run under,
// from the same source: a manual walk's caller, a subscription walk's
// promoter. Before, only the member rows did, and the walk's read as
// unrestricted.
func TestTeamWalkRun_RowCarriesTheCallersRestriction(t *testing.T) {
	restricted := auth.Principal{TenantID: "acme", Subject: "alice", Scopes: []string{auth.ScopeRunsCreate, auth.ScopeUser}}
	admin := auth.Principal{Subject: "root", Scopes: []string{auth.ScopeAdmin}}
	for name, walk := range map[string]func(h *promoterHarness){
		"a manual walk by a restricted caller": func(h *promoterHarness) {
			h.promote(tools.RunIdentityValue{AgentID: "a_root", TenantID: "acme"}, &admin, h.authorUnpromoted())
			if _, err := h.pub.PublishNow(context.Background(), "in", "acme", store.MemoryScopeTenant, "",
				json.RawMessage(`{"task":"write"}`), "_system", 0, 0); err != nil {
				t.Fatalf("publish: %v", err)
			}
			ctx := auth.WithPrincipal(tools.WithRunIdentity(context.Background(),
				tools.RunIdentityValue{AgentID: "a_alice", TenantID: "acme", UserID: "alice"}), restricted)
			// The walk's outcome is not under test, only the row it opened.
			if _, err := h.srv.TeamDef(ctx, json.RawMessage(`{"op":"run","name":"armed"}`)); err != nil {
				t.Fatalf("TeamDef run: %v", err)
			}
		},
		"a subscription walk of a confined promoter's team": func(h *promoterHarness) {
			h.promote(tools.RunIdentityValue{AgentID: "a_alice", TenantID: "acme", UserID: "alice"}, &restricted, h.authorUnpromoted())
			h.walkOnce()
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := newPromoterHarness(t)
			walk(h)
			row, err := h.st.GetRunByAgentID(context.Background(), teamWalkAgentPrefix+"armed")
			if err != nil {
				t.Fatalf("the walk's run row: %v", err)
			}
			if !row.OperatorKeyRestricted || !row.Isolated {
				t.Errorf("walk row %s: operator_key_restricted=%v isolated=%v, want both", row.ID, row.OperatorKeyRestricted, row.Isolated)
			}
		})
	}
}
