package http

import (
	"errors"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/connector"
)

// A turn-cancel for a run that is not live on this replica is routed to the
// replica that owns it. The local path confines an isolated member through the
// session gate; the routed path checked only the tenant, so an isolated member
// could stop another user's turn whenever that run lived on another replica.
func TestCancelTurn_CrossReplicaIsolatedMemberCannotStopAnotherUsersTurn(t *testing.T) {
	srv, cleanup := turnCancelFixture(t)
	defer cleanup()
	aliceRun := seedRunInTenant(t, srv.store, "acme", "alice", "a_alice")
	bobRun := seedRunInTenant(t, srv.store, "acme", "bob", "a_bob")
	// Neither run is in this replica's steer registry, so every call takes the
	// cross-replica route.
	remote := &stubClusterCanceller{found: true}
	srv.turnCancelReg.SetClusterCanceller(remote)
	bob := tenantPrincipalCtx("acme", "bob", auth.ScopeUser)

	_, _, ghostErr := srv.CancelTurn(bob, "run_ghost", "")
	if !errors.Is(ghostErr, connector.ErrRunNotInFlight) {
		t.Fatalf("unknown run id: %v, want ErrRunNotInFlight", ghostErr)
	}
	if stopped, _, err := srv.CancelTurn(bob, aliceRun, ""); !errors.Is(err, connector.ErrRunNotInFlight) || stopped {
		t.Errorf("isolated member stopping another user's turn = stopped %v, %v; want the unknown-id %v", stopped, err, ghostErr)
	}
	if remote.calls != 0 {
		t.Fatalf("another user's turn-cancel reached the owning replica (%d call(s))", remote.calls)
	}

	if stopped, _, err := srv.CancelTurn(bob, bobRun, ""); err != nil || !stopped || remote.runID != bobRun {
		t.Errorf("isolated member stopping its own turn = stopped %v, %v (routed %q); want routed and stopped", stopped, err, remote.runID)
	}
	op := tenantPrincipalCtx("acme", "op", auth.ScopeTenant)
	if stopped, _, err := srv.CancelTurn(op, aliceRun, ""); err != nil || !stopped || remote.runID != aliceRun {
		t.Errorf("tenant operator stopping a member's turn = stopped %v, %v (routed %q); want routed and stopped", stopped, err, remote.runID)
	}
}
