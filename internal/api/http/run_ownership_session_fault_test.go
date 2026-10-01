package http

import (
	"context"
	"errors"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/steer"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// sessionFaultStore fails every session read, as a store fault or a session
// deleted under a live run does.
type sessionFaultStore struct {
	store.Store
}

func (sessionFaultStore) GetSession(context.Context, string) (store.Session, error) {
	return store.Session{}, errors.New("store fault")
}

// liveSessionRun seeds alice's run in acme and registers it live on this
// replica, returning its run id and the steer queue its loop would drain.
func liveSessionRun(t *testing.T, srv *Server) (string, <-chan steer.Message) {
	t.Helper()
	runID := seedRunInTenant(t, srv.store, "acme", "alice", "a_alice")
	run, err := srv.store.GetRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	q, dereg := srv.steerReg.Register(steer.Entry{RunID: runID, SessionID: run.SessionID})
	t.Cleanup(dereg)
	return runID, q
}

// The local steer gate read the run's session and checked ownership only when
// the read succeeded, so a failed read let an isolated member steer another
// user's run.
func TestSteerRun_SessionReadFaultRefusesIsolatedMember(t *testing.T) {
	srv, cleanup := turnCancelFixture(t)
	defer cleanup()
	aliceRun, q := liveSessionRun(t, srv)
	healthy := srv.store
	srv.store = sessionFaultStore{Store: healthy}

	bob := tenantPrincipalCtx("acme", "bob", auth.ScopeUser)
	if delivered, err := srv.SteerRun(bob, aliceRun, "take over", "api"); !errors.Is(err, connector.ErrRunNotInFlight) || delivered {
		t.Fatalf("steer with an unreadable session = delivered %v, %v; want ErrRunNotInFlight", delivered, err)
	}
	select {
	case m := <-q:
		t.Fatalf("another user's run was steered: %q", m.Text)
	default:
	}

	srv.store = healthy
	alice := tenantPrincipalCtx("acme", "alice", auth.ScopeUser)
	if delivered, err := srv.SteerRun(alice, aliceRun, "carry on", "api"); err != nil || !delivered {
		t.Fatalf("owner steer = delivered %v, %v; want delivered", delivered, err)
	}
	if m := <-q; m.Text != "carry on" {
		t.Errorf("owner's steer delivered %q, want %q", m.Text, "carry on")
	}
}

// The local turn-cancel gate had the same shape: a failed session read skipped
// the ownership check and fired another user's armed turn.
func TestCancelTurn_SessionReadFaultRefusesIsolatedMember(t *testing.T) {
	srv, cleanup := turnCancelFixture(t)
	defer cleanup()
	aliceRun, _ := liveSessionRun(t, srv)
	turnCtx, cancelTurn := context.WithCancelCause(context.Background())
	defer cancelTurn(nil)
	srv.turnCancelReg.Arm(aliceRun, cancelTurn)
	healthy := srv.store
	srv.store = sessionFaultStore{Store: healthy}

	bob := tenantPrincipalCtx("acme", "bob", auth.ScopeUser)
	if stopped, _, err := srv.CancelTurn(bob, aliceRun, ""); !errors.Is(err, connector.ErrRunNotInFlight) || stopped {
		t.Fatalf("turn-cancel with an unreadable session = stopped %v, %v; want ErrRunNotInFlight", stopped, err)
	}
	if turnCtx.Err() != nil || !srv.turnCancelReg.IsArmed(aliceRun) {
		t.Fatal("another user's turn was cancelled")
	}

	srv.store = healthy
	alice := tenantPrincipalCtx("acme", "alice", auth.ScopeUser)
	if stopped, parked, err := srv.CancelTurn(alice, aliceRun, ""); err != nil || !stopped || !parked {
		t.Fatalf("owner turn-cancel = stopped %v parked %v, %v; want stopped and parked", stopped, parked, err)
	}
	if turnCtx.Err() == nil {
		t.Error("owner's turn-cancel did not fire the armed token")
	}
}
