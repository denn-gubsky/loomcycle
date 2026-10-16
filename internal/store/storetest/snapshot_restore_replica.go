package storetest

import (
	"context"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// A queued run restored from a snapshot is queued again by the restoring
// replica, which is then the one holding its wait. If that replica restarts
// before the run starts, its boot restarts only the queued rows stamped with
// its own id, so the restore must keep the replica it is given. The restore
// insert used to omit the column, leaving such a row to no replica: in a
// cluster no boot ever listed it again. (A single-replica store has no
// replica column and lists every queued row; it passes trivially.)
func testSnapshotRestoreRunKeepsItsReplica(t *testing.T, s store.Store) {
	ctx := context.Background()
	sess := store.Session{ID: "sess_restored_queued_replica", Agent: "a", UserID: "alice"}
	if ok, err := s.SnapshotRestoreSession(ctx, sess); err != nil || !ok {
		t.Fatalf("SnapshotRestoreSession = %v, %v", ok, err)
	}
	restored := store.Run{
		ID: "r_restored_queued_replica", SessionID: sess.ID, UserID: "alice",
		AgentID: "a_restored_queued_replica", Status: store.RunQueued,
		StartedAt: time.Now().Add(-time.Minute), ReplicaID: "replica-restorer",
	}
	if ok, err := s.SnapshotRestoreRun(ctx, restored); err != nil || !ok {
		t.Fatalf("SnapshotRestoreRun = %v, %v", ok, err)
	}

	list, err := s.ListQueuedRuns(ctx, "replica-restorer", time.Now())
	if err != nil {
		t.Fatalf("ListQueuedRuns: %v", err)
	}
	for _, q := range list {
		if q.Run.ID == restored.ID {
			return
		}
	}
	t.Errorf("the restoring replica's boot does not list the queued run restored for it (listed %d)", len(list))
}
