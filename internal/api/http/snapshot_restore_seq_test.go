package http

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// A paused run restored onto an instance that has been running resumes on its
// whole transcript. Restore used to write the captured events under the
// source's seqs, which the target's own history already held, so they were
// dropped: the run came back with no pending turn and was flagged failed.
func TestRestoreSnapshot_PausedRunResumesOnItsTranscriptOnATargetWithHistory(t *testing.T) {
	srv, _, runID, envelope := restoreResumeServer(t)
	ctx := context.Background()
	sess, err := srv.store.CreateSession(ctx, "other", "resumer", "bob")
	if err != nil {
		t.Fatal(err)
	}
	other, err := srv.store.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_other", UserID: "bob", TenantID: "other"})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		b, _ := json.Marshal(map[string]int{"i": i})
		if err := srv.store.AppendEvent(ctx, other.ID, "text", b); err != nil {
			t.Fatal(err)
		}
	}

	res, err := srv.RestoreSnapshot(ctx, connector.RestoreSnapshotRequest{RawJSON: envelope})
	if err != nil {
		t.Fatalf("RestoreSnapshot: %v", err)
	}
	if res.Restored["transcript_events"] != 1 {
		t.Errorf("transcript_events restored = %d, want 1 (warnings %v)", res.Restored["transcript_events"], res.Warnings)
	}
	awaitRunCompleted(t, srv.store, runID)

	evs, err := srv.store.GetRunEventsSince(ctx, runID, 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	// The resume's own events sort after the restored user turn, not before it.
	if len(evs) < 2 || evs[0].Type != "user_input" {
		t.Fatalf("the resumed run's transcript does not open with its restored user turn: %+v", evs)
	}

	// Restoring the same snapshot again writes no second transcript.
	again, err := srv.RestoreSnapshot(ctx, connector.RestoreSnapshotRequest{RawJSON: envelope})
	if err != nil {
		t.Fatalf("second RestoreSnapshot: %v", err)
	}
	if again.Restored["transcript_events"] != 0 {
		t.Errorf("second restore transcript_events = %d, want 0", again.Restored["transcript_events"])
	}
	after, _ := srv.store.GetRunEventsSince(ctx, runID, 0, 1000)
	if len(after) != len(evs) {
		t.Errorf("a re-restore changed the run's transcript from %d to %d events", len(evs), len(after))
	}
}
