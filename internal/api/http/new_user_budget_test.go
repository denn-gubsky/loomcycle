package http

import (
	"context"
	"net/http"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/limits"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// auth.lifecycle.new_user.budget is every new user's default: a user the
// password gate makes at invite redemption, or an operator makes with POST
// /v1/_users, gets it as its token limit just as an identity gate's does.

// withFloorBudget sets the deployment floor's new_user budget to a hard
// limit of 0 and gives the server a limits tracker.
func withFloorBudget(f *gateFixture) {
	f.srv.limits = limits.New(f.st) // as http.New builds it; the bare test server has none
	hard := int64(0)
	cfg := f.srv.cfg()
	floor := cfg.ResolvedAuth.NewUserFloor()
	floor.Budget = &config.AuthBudgetConfig{HardLimit: &hard}
	cfg.ResolvedAuth.Lifecycle = floor
}

func userBudgetRows(t *testing.T, st store.Store, tenant, subject string) []store.TokenLimitRow {
	t.Helper()
	rows, err := st.TokenLimitsAll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var out []store.TokenLimitRow
	for _, r := range rows {
		if r.TenantID == tenant && r.Scope == "user" && r.ScopeID == subject {
			out = append(out, r)
		}
	}
	return out
}

func TestPasswordGate_AnInvitedUserGetsTheNewUserBudget(t *testing.T) {
	f := newGateFixture(t)
	withFloorBudget(f)
	b := f.redeemed(t, "local", "nina@example.com", f.inviteCode(t, bAcmeOp, "local", `{}`))
	_, subject := b.me()
	rows := userBudgetRows(t, f.st, "acme", subject)
	if len(rows) != 1 || rows[0].HardLimit == nil || *rows[0].HardLimit != 0 {
		t.Fatalf("budget rows for the invited user = %+v, want the floor's hard limit 0", rows)
	}
	if dec := f.srv.LimitsTracker().Check("acme", subject); dec.Allowed {
		t.Fatal("the invited user's budget is stored but not enforced")
	}
}

func TestCreateUser_AnOperatorCreatedUserGetsTheNewUserBudget(t *testing.T) {
	f := newGateFixture(t)
	withFloorBudget(f)
	if code, body := f.call(t, bAcmeOp, "POST", "/v1/_users", `{"subject":"olga"}`); code != http.StatusCreated {
		t.Fatalf("create: %d %v", code, body)
	}
	rows := userBudgetRows(t, f.st, "acme", "olga")
	if len(rows) != 1 || rows[0].HardLimit == nil || *rows[0].HardLimit != 0 {
		t.Fatalf("budget rows for the created user = %+v, want the floor's hard limit 0", rows)
	}
}

// A create that conflicts with an existing user writes nothing: the budget
// is for a user coming into existence, not one an operator already runs.
func TestCreateUser_AConflictingCreateWritesNoBudget(t *testing.T) {
	f := newGateFixture(t)
	f.addUser(t, "acme", "pat", "isolated")
	withFloorBudget(f)
	if code, body := f.call(t, bAcmeOp, "POST", "/v1/_users", `{"subject":"pat"}`); code != http.StatusConflict {
		t.Fatalf("create over an existing user: %d %v", code, body)
	}
	if rows := userBudgetRows(t, f.st, "acme", "pat"); len(rows) != 0 {
		t.Fatalf("a conflicting create wrote a budget onto the existing user: %+v", rows)
	}
}
