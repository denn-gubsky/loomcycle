package snapshot

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// restoredRunRecorder records every run row a restore writes, then writes it.
type restoredRunRecorder struct {
	store.Store
	rows []store.Run
}

func (r *restoredRunRecorder) SnapshotRestoreRun(ctx context.Context, run store.Run) (bool, error) {
	r.rows = append(r.rows, run)
	return r.Store.SnapshotRestoreRun(ctx, run)
}

// A restored queued run is queued again on the replica that restores it, so
// its row is stamped with that replica: a restart of that replica before the
// run starts must find it among its own queued rows.
func TestRestore_StampsARestoredQueuedRunWithTheRestoringReplica(t *testing.T) {
	ctx := context.Background()
	src, closeSrc := newTestStore(t)
	defer closeSrc()
	sess, err := src.CreateSession(ctx, "", "a", "alice")
	if err != nil {
		t.Fatal(err)
	}
	queued, err := src.CreateQueuedRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_stamp", UserID: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	_, raw, err := Capture(ctx, src, CaptureOptions{})
	if err != nil {
		t.Fatal(err)
	}

	dst, closeDst := newTestStore(t)
	defer closeDst()
	rec := &restoredRunRecorder{Store: dst}
	if _, err := Restore(ctx, rec, raw, RestoreOptions{ReplicaID: "replica-restorer"}); err != nil {
		t.Fatal(err)
	}
	for _, row := range rec.rows {
		if row.ID == queued.ID {
			if row.ReplicaID != "replica-restorer" {
				t.Errorf("restored queued run's replica = %q, want the restoring replica", row.ReplicaID)
			}
			return
		}
	}
	t.Fatalf("the queued run was not restored (rows %d)", len(rec.rows))
}

// A restore takes a queued run's request only in the shape Capture writes: a
// hand-made envelope cannot bring in sealed secrets, store a no-trace run's
// prompt, or store bytes that are not a request.
func TestRestorableQueuedInput_RefusesWhatCaptureNeverWrites(t *testing.T) {
	withSecrets := json.RawMessage(`{"input":{"Agent":"r"},"secrets":{"key_id":"k","nonce":"n","ciphertext":"c"}}`)
	got, ok := restorableQueuedInput(QueuedRunEntry{QueuedInput: withSecrets})
	if !ok || strings.Contains(string(got), "ciphertext") || !strings.Contains(string(got), `"Agent":"r"`) {
		t.Errorf("an envelope with sealed secrets restored %s (%v), want the request without them", got, ok)
	}
	if _, ok := restorableQueuedInput(QueuedRunEntry{QueuedInput: json.RawMessage(`{"input":{}}`), Retention: store.RetentionNone}); ok {
		t.Error("a no-trace run's request was restored")
	}
	for _, bad := range []string{`"a string"`, `[1,2]`, `not json`} {
		if _, ok := restorableQueuedInput(QueuedRunEntry{QueuedInput: json.RawMessage(bad)}); ok {
			t.Errorf("request %s was restored, want refused", bad)
		}
	}
	if _, ok := restorableQueuedInput(QueuedRunEntry{}); ok {
		t.Error("an empty request was restored")
	}
}
