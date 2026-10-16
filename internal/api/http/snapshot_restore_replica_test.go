package http

import (
	"context"
	"testing"
)

// A restore on a cluster replica stamps the queued runs it restores with that
// replica, which queues them again: the options every restore call site
// passes must name it.
func TestSnapshotRestoreOptions_NameThisReplicaForRestoredQueuedRuns(t *testing.T) {
	srv, _ := newGatedBatchServer(t, 1)
	srv.replicaID = "replica-restorer"
	if got := srv.snapshotRestoreOptions(context.Background(), false).ReplicaID; got != "replica-restorer" {
		t.Errorf("restore options name replica %q, want this replica", got)
	}
}
