package lifecycle

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/workspace/worker"
)

// A user's erasure removes their durable workspaces, pinned or parked, rows
// and directories, and nothing of another user's or an ephemeral one
// (RFC GW P12 ruling Q2).
func TestEraseDurable_RemovesOnlyTheUsersDurableWorkspaces(t *testing.T) {
	f := newFixture(t, Limits{MaxPerRun: 4, LostAfter: time.Minute}, worker.Options{})
	f.m.SetDurable(DurableLimits{Enabled: true, MaxPerUser: 8})
	ctx := context.Background()
	alice, bob := f.run(t, "alice"), f.run(t, "bob")
	open := func(o Owner, name, lifetime string) store.WorkspaceRow {
		t.Helper()
		r, err := f.m.Open(ctx, o, OpenRequest{Name: name, Toolchains: []string{"python", "node"}, Lifetime: lifetime})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	pinned := open(alice, "one", store.WorkspaceLifetimeDurable)
	parked := open(alice, "two", store.WorkspaceLifetimeDurable)
	if err := f.m.Release(ctx, parked); err != nil {
		t.Fatal(err)
	}
	eph := open(alice, "tmp", "")
	bobs := open(bob, "one", store.WorkspaceLifetimeDurable)

	n, err := f.m.EraseDurable(ctx, "acme", "alice")
	if err != nil || n != 2 {
		t.Fatalf("erased %d, %v; want 2", n, err)
	}
	for _, r := range []store.WorkspaceRow{pinned, parked} {
		if _, err := f.st.WorkspaceGet(ctx, "acme", r.ID); err == nil {
			t.Errorf("%s's row survived", r.Name)
		}
		if _, err := os.Stat(f.h.Dir(r.ID)); !os.IsNotExist(err) {
			t.Errorf("%s's directory survived: %v", r.Name, err)
		}
	}
	for _, r := range []store.WorkspaceRow{eph, bobs} {
		if _, err := f.st.WorkspaceGet(ctx, "acme", r.ID); err != nil {
			t.Errorf("%s (%s) was removed: %v", r.Name, r.UserID, err)
		}
	}
}
