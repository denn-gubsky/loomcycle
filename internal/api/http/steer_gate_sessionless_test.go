package http

import (
	"context"
	"errors"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/steer"
)

// liveSessionlessRun seeds alice's run in acme and registers it live on this
// replica with NO session on the entry — the shape whose ownership gate used
// to be skipped. Returns the run id and the steer queue its loop would drain.
func liveSessionlessRun(t *testing.T, srv *Server) (string, <-chan steer.Message) {
	t.Helper()
	runID := seedRunInTenant(t, srv.store, "acme", "alice", "a_alice")
	q, dereg := srv.steerReg.Register(steer.Entry{RunID: runID})
	t.Cleanup(dereg)
	return runID, q
}

// namedCtx is a caller the sessionless-gate tests run as.
type namedCtx struct {
	name string
	ctx  context.Context
}

// sessionlessGateCallers returns the callers that must be refused (another
// tenant; an isolated member of the run's tenant who does not own it) and
// those that must pass (the run's owner; a super-admin of another tenant).
func sessionlessGateCallers() (refused, admitted []namedCtx) {
	refused = []namedCtx{
		{"other tenant", tenantPrincipalCtx("globex", "mallory", auth.ScopeTenant)},
		{"isolated peer", tenantPrincipalCtx("acme", "bob", auth.ScopeUser)},
	}
	admitted = []namedCtx{
		{"owner", tenantPrincipalCtx("acme", "alice", auth.ScopeUser)},
		{"admin", tenantPrincipalCtx("globex", "root", auth.ScopeAdmin)},
	}
	return refused, admitted
}

// A live entry with no session skipped the ownership gate, so a token from any
// tenant could steer the run. It is gated on the run's own row instead.
func TestSteerRun_SessionlessEntryAdmitsOnlyTheRunsOwner(t *testing.T) {
	srv, cleanup := turnCancelFixture(t)
	defer cleanup()
	runID, q := liveSessionlessRun(t, srv)
	refused, admitted := sessionlessGateCallers()

	for _, c := range refused {
		name, ctx := c.name, c.ctx
		if delivered, err := srv.SteerRun(ctx, runID, "take over", "api"); !errors.Is(err, connector.ErrRunNotInFlight) || delivered {
			t.Errorf("%s steer = delivered %v, %v; want the opaque ErrRunNotInFlight", name, delivered, err)
		}
	}
	select {
	case m := <-q:
		t.Fatalf("a non-owner steered the run: %q", m.Text)
	default:
	}

	for _, c := range admitted {
		name, ctx := c.name, c.ctx
		if delivered, err := srv.SteerRun(ctx, runID, name, "api"); err != nil || !delivered {
			t.Fatalf("%s steer = delivered %v, %v; want delivered", name, delivered, err)
		}
		if m := <-q; m.Text != name {
			t.Errorf("%s steer delivered %q", name, m.Text)
		}
	}
}

// CancelTurn had the same skip: any caller could stop the run's armed turn.
func TestCancelTurn_SessionlessEntryAdmitsOnlyTheRunsOwner(t *testing.T) {
	srv, cleanup := turnCancelFixture(t)
	defer cleanup()
	runID, _ := liveSessionlessRun(t, srv)
	refused, admitted := sessionlessGateCallers()
	arm := func() context.Context {
		turnCtx, cancelTurn := context.WithCancelCause(context.Background())
		t.Cleanup(func() { cancelTurn(nil) })
		srv.turnCancelReg.Arm(runID, cancelTurn)
		return turnCtx
	}

	turnCtx := arm()
	for _, c := range refused {
		name, ctx := c.name, c.ctx
		if stopped, _, err := srv.CancelTurn(ctx, runID, ""); !errors.Is(err, connector.ErrRunNotInFlight) || stopped {
			t.Errorf("%s turn-cancel = stopped %v, %v; want the opaque ErrRunNotInFlight", name, stopped, err)
		}
	}
	if turnCtx.Err() != nil || !srv.turnCancelReg.IsArmed(runID) {
		t.Fatal("a non-owner cancelled the run's turn")
	}

	for _, c := range admitted {
		name, ctx := c.name, c.ctx
		if turnCtx.Err() != nil {
			turnCtx = arm() // the previous caller stopped the turn; start another
		}
		if stopped, parked, err := srv.CancelTurn(ctx, runID, ""); err != nil || !stopped || !parked {
			t.Fatalf("%s turn-cancel = stopped %v parked %v, %v; want stopped and parked", name, stopped, parked, err)
		}
		if turnCtx.Err() == nil {
			t.Errorf("%s turn-cancel did not fire the armed token", name)
		}
	}
}

// runForSteer is the gate retune and the run-config reads go through; it
// skipped the check the same way, and then read the row unscoped.
func TestRunForSteer_SessionlessEntryAdmitsOnlyTheRunsOwner(t *testing.T) {
	srv, cleanup := turnCancelFixture(t)
	defer cleanup()
	runID, _ := liveSessionlessRun(t, srv)
	refused, admitted := sessionlessGateCallers()

	for _, c := range refused {
		name, ctx := c.name, c.ctx
		if _, err := srv.runForSteer(ctx, runID); !errors.Is(err, connector.ErrRunNotInFlight) {
			t.Errorf("%s runForSteer = %v; want the opaque ErrRunNotInFlight", name, err)
		}
	}
	for _, c := range admitted {
		name, ctx := c.name, c.ctx
		if run, err := srv.runForSteer(ctx, runID); err != nil || run.ID != runID {
			t.Errorf("%s runForSteer = %q, %v; want the run", name, run.ID, err)
		}
	}
}

// A sessionless entry whose run cannot be read shows no owner, so even the
// admin is refused: nothing is left to gate on.
func TestSteerRun_SessionlessEntryWithoutARunRowIsRefused(t *testing.T) {
	srv, cleanup := turnCancelFixture(t)
	defer cleanup()
	q, dereg := srv.steerReg.Register(steer.Entry{RunID: "r_no_row"})
	defer dereg()

	if delivered, err := srv.SteerRun(tenantPrincipalCtx("globex", "root", auth.ScopeAdmin), "r_no_row", "hello", "api"); !errors.Is(err, connector.ErrRunNotInFlight) || delivered {
		t.Fatalf("steer of a rowless entry = delivered %v, %v; want ErrRunNotInFlight", delivered, err)
	}
	select {
	case m := <-q:
		t.Fatalf("a rowless entry was steered: %q", m.Text)
	default:
	}
}

// With no store there is nothing to show ownership from. No entry is
// registered without one in production, so refusing costs nothing there.
func TestSteerRun_NoStoreRefusesALiveEntry(t *testing.T) {
	srv, cleanup := turnCancelFixture(t)
	defer cleanup()
	runID, q := liveSessionlessRun(t, srv)
	srv.store = nil

	if delivered, err := srv.SteerRun(tenantPrincipalCtx("globex", "root", auth.ScopeAdmin), runID, "hello", "api"); !errors.Is(err, connector.ErrRunNotInFlight) || delivered {
		t.Fatalf("steer with no store = delivered %v, %v; want ErrRunNotInFlight", delivered, err)
	}
	select {
	case m := <-q:
		t.Fatalf("a run was steered with no store to gate on: %q", m.Text)
	default:
	}
}
