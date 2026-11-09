package http

import (
	"context"
	"errors"
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

// userBudgetCount is how many user-scope budget rows acme has.
func userBudgetCount(t *testing.T, st store.Store) int {
	t.Helper()
	rows, err := st.TokenLimitsAll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, r := range rows {
		if r.TenantID == "acme" && r.Scope == "user" {
			n++
		}
	}
	return n
}

// createLosesTheRace is a store whose UserCreate answers as if another
// create of the same subject won between the handler's check and its write.
type createLosesTheRace struct{ store.Store }

func (createLosesTheRace) UserCreate(_ context.Context, row store.UserRow) error {
	return &store.ErrConflict{Kind: "user", ID: row.Subject}
}

func (createLosesTheRace) UserCreateBudgeted(_ context.Context, row store.UserRow, _ *store.AclBindingRow, _ store.TokenLimitRow) error {
	return &store.ErrConflict{Kind: "user", ID: row.Subject}
}

// A create the store refuses (here, one that lost a race to another create
// of the subject) leaves no budget row behind.
func TestCreateUser_ARefusedCreateLeavesNoBudgetRow(t *testing.T) {
	f := newGateFixture(t)
	withFloorBudget(f)
	f.srv.store = createLosesTheRace{f.st}
	if code, body := f.call(t, bAcmeOp, "POST", "/v1/_users", `{"subject":"racer"}`); code != http.StatusConflict {
		t.Fatalf("precondition: the create was not refused: %d %v", code, body)
	}
	if rows := userBudgetRows(t, f.st, "acme", "racer"); len(rows) != 0 {
		t.Fatalf("a refused create left a budget row: %+v", rows)
	}
}

// A redemption the store refuses (its login is taken) leaves no budget row.
func TestPasswordGate_ARefusedRedemptionLeavesNoBudgetRow(t *testing.T) {
	f := newGateFixture(t)
	f.redeemed(t, "local", "vera@example.com", f.inviteCode(t, bAcmeOp, "local", `{}`))
	withFloorBudget(f)
	before := userBudgetCount(t, f.st)
	b := f.browser(t)
	code, body := b.do("POST", "/v1/_auth/invite", jsonBody(t, map[string]string{
		"code": f.inviteCode(t, bAcmeOp, "local", `{}`), "login": "vera@example.com", "password": goodPassword}))
	if code != http.StatusConflict {
		t.Fatalf("precondition: the taken login was not refused: %d %v", code, body)
	}
	if after := userBudgetCount(t, f.st); after != before {
		t.Fatalf("a refused redemption left %d budget row(s)", after-before)
	}
}

// budgetWritesFail is a store whose separate budget write and user delete
// both fault: a create that wrote the budget after the user, and undid the
// user when that failed, would leave the user uncapped.
type budgetWritesFail struct{ store.Store }

func (budgetWritesFail) SnapshotRestoreTokenLimit(context.Context, store.TokenLimitRow) (bool, error) {
	return false, errors.New("injected: budget write failed")
}

func (budgetWritesFail) UserDelete(context.Context, string, string) (bool, error) {
	return false, errors.New("injected: user delete failed")
}

// A user POST /v1/_users makes never exists without the new-user budget,
// whatever fails around it.
func TestCreateUser_NeverLeavesAUserWithoutItsBudget(t *testing.T) {
	f := newGateFixture(t)
	withFloorBudget(f)
	f.srv.store = budgetWritesFail{f.st}
	code, _ := f.call(t, bAcmeOp, "POST", "/v1/_users", `{"subject":"capped"}`)
	_, uerr := f.st.UserGet(context.Background(), "acme", "capped")
	rows := userBudgetRows(t, f.st, "acme", "capped")
	if uerr == nil && len(rows) == 0 {
		t.Fatalf("the user exists without its budget (create answered %d)", code)
	}
}

// redeemOverPresetLimit is a store whose redemption finds a limit already
// stored for the new subject: the cache must then hold that one.
type redeemOverPresetLimit struct {
	store.Store
	preset int64
}

func (w redeemOverPresetLimit) AuthInviteRedeem(ctx context.Context, red store.AuthInviteRedemption) error {
	if red.User != nil {
		if err := w.Store.TokenLimitPut(ctx, store.TokenLimitRow{TenantID: red.User.TenantID, Scope: "user",
			ScopeID: red.User.Subject, HardLimit: &w.preset}); err != nil {
			return err
		}
	}
	return w.Store.AuthInviteRedeem(ctx, red)
}

// After a redemption the limits cache holds the stored budget, not the
// floor's: a limit already there is the one in force.
func TestPasswordGate_TheCacheHoldsTheStoredBudgetAfterARedemption(t *testing.T) {
	f := newGateFixture(t)
	withFloorBudget(f) // hard 0: every run refused
	f.srv.store = redeemOverPresetLimit{Store: f.st, preset: 1_000_000}
	b := f.redeemed(t, "local", "will@example.com", f.inviteCode(t, bAcmeOp, "local", `{}`))
	_, subject := b.me()
	if dec := f.srv.LimitsTracker().Check("acme", subject); !dec.Allowed {
		t.Fatal("the cache holds the floor's budget, not the one stored for the user")
	}
}
