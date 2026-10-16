package snapshot

import (
	"context"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// A paused run's no-user-scope bit survives a snapshot and a restore, as its
// confinement bits do: resume reads it off the row, so a restored run that
// lost it resolved $cred: in its user's own scope.
func TestRoundTrip_PreservesRunCredentialsNoUserScope(t *testing.T) {
	src, srcClose := newTestStore(t)
	defer srcClose()
	dst, dstClose := newTestStore(t)
	defer dstClose()
	ctx := context.Background()

	sess, _ := src.CreateSession(ctx, "acme", "qa", "alice")
	run, err := src.CreateRun(ctx, sess.ID, store.RunIdentity{
		AgentID: "a_hooked", UserID: "alice", TenantID: "acme", CredentialsNoUserScope: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := src.SetRunPauseState(ctx, run.ID, store.PauseStatePaused); err != nil {
		t.Fatal(err)
	}

	_, raw, err := Capture(ctx, src, CaptureOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Restore(ctx, dst, raw, RestoreOptions{}); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	got, err := dst.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("GetRun on dst: %v", err)
	}
	if !got.CredentialsNoUserScope {
		t.Error("the no-user-scope bit was lost across pause→snapshot→restore")
	}
}
