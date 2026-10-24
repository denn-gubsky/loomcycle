package http

import (
	"context"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// failSecondRetire lets the first token retire through and faults the next.
type failSecondRetire struct {
	store.Store
	calls int
}

func (f *failSecondRetire) OperatorTokenDefSetRetiredAt(ctx context.Context, defID string, at time.Time) error {
	f.calls++
	if f.calls > 1 {
		return errDiskFull
	}
	return f.Store.OperatorTokenDefSetRetiredAt(ctx, defID, at)
}

// TestRetireDelegatedUserTokens_FlushesTheCacheWhenItFaultsPartWay: a retire
// that faults after retiring one token must still flush the auth cache, or
// that token keeps authenticating from the cache while the row says retired.
func TestRetireDelegatedUserTokens_FlushesTheCacheWhenItFaultsPartWay(t *testing.T) {
	s, st := tokenAuthServer(t, "legacy")
	s.EnableTokenCache(time.Hour)
	ops := auth.Principal{TenantID: "acme", Subject: "ops", Scopes: []string{auth.ScopeTenant}}
	seedUser(t, st, "acme", "alice", "isolated", "active")
	a := mintFor(t, s, ops, "alice", "")
	b := mintFor(t, s, ops, "alice", "")
	ctx := context.Background()
	for _, tok := range []string{a.Token, b.Token} {
		if _, ok := s.resolvePrincipal(ctx, tok); !ok {
			t.Fatal("precondition: a minted token does not authenticate")
		}
	}
	s.store = &failSecondRetire{Store: st}

	retired, err := s.retireDelegatedUserTokens(ctx, "acme", "alice")
	if err == nil || len(retired) != 1 {
		t.Fatalf("retire = (%d rows, %v), want one retired and the fault", len(retired), err)
	}
	var tok string
	for _, m := range []mintUserTokenResponse{a, b} {
		if m.DefID == retired[0].DefID {
			tok = m.Token
		}
	}
	if _, ok := s.resolvePrincipal(ctx, tok); ok {
		t.Error("the retired token still authenticates from the cache")
	}
}
