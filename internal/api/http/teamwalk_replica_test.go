package http

import (
	"context"
	"sync"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// replicaCapturingStore records the replica each run row was created with.
// SQLite keeps no replica column, so the identity handed to the store is
// what a clustered backend would write.
type replicaCapturingStore struct {
	store.Store
	mu      sync.Mutex
	replica map[string]string
}

func (s *replicaCapturingStore) CreateRun(ctx context.Context, sessionID string, identity store.RunIdentity) (store.Run, error) {
	run, err := s.Store.CreateRun(ctx, sessionID, identity)
	if err == nil {
		s.mu.Lock()
		s.replica[run.ID] = identity.ReplicaID
		s.mu.Unlock()
	}
	return run, err
}

// A walk's row names the replica running it, as every other run's does: the
// cross-replica cancel and steer coordinators route a run by that column,
// and a row without it reads as un-routable.
func TestTeamWalkRun_TheRowCarriesTheReplicaRunningIt(t *testing.T) {
	h := newReviewHarness(t)
	st := &replicaCapturingStore{Store: h.srv.store, replica: map[string]string{}}
	h.srv.store = st
	h.srv.replicaID = "replica-a"

	_, runID, finish, err := h.srv.openTeamWalkRun(context.Background(), builtin.WalkRunSpec{Name: "triage"})
	if err != nil {
		t.Fatal(err)
	}
	finish(builtin.WalkEnd{FinalText: "done"})
	st.mu.Lock()
	defer st.mu.Unlock()
	if got, ok := st.replica[runID]; !ok || got != "replica-a" {
		t.Errorf("walk run %s created with replica %q (recorded %v), want %q", runID, got, ok, "replica-a")
	}
}
