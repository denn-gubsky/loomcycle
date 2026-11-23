package lifecycle

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/workspace/worker"
)

// RFC GW D35: a workspace opened over loomcycle's MCP server belongs to the
// session's token identity, its tenant and subject.

// Every session and token of one subject reaches the subject's workspace;
// no other subject of the tenant, no other tenant's same subject, and no run
// (not even a run of the same user) finds it, and the subject finds none of
// a run's.
func TestPrincipalOwner_OnlyTheSameTenantAndSubjectFindIt(t *testing.T) {
	f := newFixture(t, Limits{MaxPerRun: 4}, worker.Options{Capacity: 10})
	ctx := context.Background()
	alice := PrincipalOwner("acme", "alice", "otd_1")
	ws := f.open(t, alice, "work")
	if ws.OwnerKind != store.WorkspaceOwnerPrincipal || ws.OpenedToken != "otd_1" || ws.OwnerAgentID != "" {
		t.Fatalf("row = %+v", ws)
	}
	// Another token of the same subject: the same workspace.
	if r, err := f.m.Get(ctx, PrincipalOwner("acme", "alice", "otd_2"), "work"); err != nil || r.ID != ws.ID {
		t.Errorf("the same subject with another token: %+v, %v", r, err)
	}
	for name, o := range map[string]Owner{
		"another subject":           PrincipalOwner("acme", "bob", "otd_3"),
		"the subject, other tenant": PrincipalOwner("other", "alice", "otd_4"),
		"a run of the same user":    f.run(t, "alice"),
		// A run whose id is the principal's pin key, as a forged row could
		// carry: the kinds still differ.
		"a run keyed like it": {TenantID: "acme", UserID: "alice", RunID: alice.RunID},
	} {
		if _, err := f.m.Get(ctx, o, "work"); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s: err = %v, want not found", name, err)
		}
		if rows, err := f.m.List(ctx, o); err != nil || len(rows) != 0 {
			t.Errorf("%s: list = %d rows, %v; want none", name, len(rows), err)
		}
	}
	// And the subject does not reach a run's workspace of the same name.
	run := f.run(t, "alice")
	runWS := f.open(t, run, "theirs")
	if _, err := f.m.Get(ctx, alice, "theirs"); !errors.Is(err, ErrNotFound) {
		t.Errorf("the principal found the run's workspace %s: %v", runWS.ID, err)
	}
	// No pin without both halves of the identity.
	if _, err := f.m.Get(ctx, PrincipalOwner("acme", "", "otd_1"), "work"); !errors.Is(err, ErrNotFound) {
		t.Errorf("an empty subject: err = %v, want not found", err)
	}
}

// No run's end and no run sweep touches a principal's workspace: its pin key
// names no run row, which the run sweeper reads as an ended run.
func TestPrincipalOwner_RunEndsAndTheRunSweepLeaveIt(t *testing.T) {
	f := newFixture(t, Limits{MaxPerRun: 4}, worker.Options{Capacity: 10})
	ctx := context.Background()
	alice := PrincipalOwner("acme", "alice", "otd_1")
	ws := f.open(t, alice, "work")
	run := f.run(t, "alice")
	f.finish(t, run.RunID, store.RunCompleted)
	if err := f.m.ReleaseRun(ctx, run.RunID); err != nil {
		t.Fatal(err)
	}
	if err := f.m.ReleaseRun(ctx, alice.RunID); err != nil {
		t.Fatal(err)
	}
	if n, err := f.m.Sweep(ctx); err != nil || n != 0 {
		t.Fatalf("run sweep = %d, %v; want nothing", n, err)
	}
	if got, err := f.st.WorkspaceGet(ctx, "acme", ws.ID); err != nil || got.State != store.WorkspaceReady || !onDisk(f.h.Dir(ws.ID)) {
		t.Errorf("after run ends and the sweep: %+v, %v", got, err)
	}
}

// A principal's workspace cannot be shared, and reaches no share.
func TestPrincipalOwner_CannotShare(t *testing.T) {
	f := newFixture(t, Limits{MaxPerRun: 4}, worker.Options{Capacity: 10})
	ctx := context.Background()
	alice := PrincipalOwner("acme", "alice", "otd_1")
	f.open(t, alice, "work")
	peer := f.run(t, "alice")
	if _, err := f.m.Share(ctx, alice, "work", Peer{RunID: peer.RunID, AgentID: peer.AgentID}, store.WorkspaceShareRead); !errors.Is(err, ErrPrincipalOwner) {
		t.Errorf("share: err = %v, want ErrPrincipalOwner", err)
	}
	if got, err := f.m.SharedWith(ctx, alice); err != nil || len(got) != 0 {
		t.Errorf("shared with a principal: %v, %v", got, err)
	}
}

// The release rule (D-P9-1): idle, the lifetime bound and the end of the
// opening token each release it; a workspace within all three is kept, and
// a token that cannot be read keeps it.
func TestSweepPrincipals_ReleasesOnIdleLifetimeAndTokenEnd(t *testing.T) {
	f := newFixture(t, Limits{MaxPerRun: 4}, worker.Options{Capacity: 10})
	ctx := context.Background()
	base := time.Now()
	f.m.now = func() time.Time { return base }
	idle := f.open(t, PrincipalOwner("acme", "idle", "otd_ok"), "w")
	old := f.open(t, PrincipalOwner("acme", "old", "otd_ok"), "w")
	revoked := f.open(t, PrincipalOwner("acme", "revoked", "otd_gone"), "w")
	busy := f.open(t, PrincipalOwner("acme", "busy", "otd_ok"), "w")
	unreadable := f.open(t, PrincipalOwner("acme", "unreadable", "otd_err"), "w")
	runWS := f.open(t, f.run(t, "alice"), "w")

	// Fifty minutes on, every one but idle is used, so only idle is idle
	// and revoked can end only by its token.
	f.m.now = func() time.Time { return base.Add(50 * time.Minute) }
	for _, sub := range []string{"busy", "old", "revoked", "unreadable"} {
		if _, err := f.m.Get(ctx, PrincipalOwner("acme", sub, ""), "w"); err != nil {
			t.Fatal(err)
		}
	}
	f.m.now = func() time.Time { return base.Add(70 * time.Minute) }
	rule := PrincipalRule{Idle: time.Hour, MaxLifetime: 24 * time.Hour, TokenValid: func(_ context.Context, id string) (bool, error) {
		switch id {
		case "otd_gone":
			return false, nil
		case "otd_err":
			return false, errors.New("store down")
		}
		return true, nil
	}}
	n, err := f.m.SweepPrincipals(ctx, rule)
	if err != nil || n != 2 {
		t.Fatalf("sweep = %d, %v; want idle and revoked", n, err)
	}
	waitGone(t, f, idle.ID)
	waitGone(t, f, revoked.ID)
	for _, r := range []store.WorkspaceRow{busy, old, unreadable, runWS} {
		if !onDisk(f.h.Dir(r.ID)) {
			t.Errorf("workspace %s (%s) was released", r.ID, r.OwnerRunID)
		}
	}
	// A day and more after they opened: the lifetime bound ends the rest,
	// however recently used; a run's workspace is not the pass's.
	f.m.now = func() time.Time { return base.Add(25 * time.Hour) }
	for _, o := range []Owner{PrincipalOwner("acme", "busy", "otd_ok"), PrincipalOwner("acme", "old", "otd_ok")} {
		if _, err := f.m.Get(ctx, o, "w"); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := f.m.SweepPrincipals(ctx, rule); err != nil || n != 3 {
		t.Fatalf("second sweep = %d, %v; want busy, old and unreadable", n, err)
	}
	if !onDisk(f.h.Dir(runWS.ID)) {
		t.Error("the run's workspace was released by the principal pass")
	}
}

// A principal's workspace on a worker that was lost comes back into service
// with the worker: read as a run's, its pin key names no run, and the
// reconciliation would remove it.
func TestPrincipalOwner_ALostWorkspaceComesBackWithItsWorker(t *testing.T) {
	f := newFixture(t, Limits{MaxPerRun: 4, LostAfter: time.Minute}, worker.Options{Capacity: 10})
	ctx := context.Background()
	ws := f.open(t, PrincipalOwner("acme", "alice", "otd_1"), "work")
	f.h.StopWorker()
	f.m.now = func() time.Time { return time.Now().Add(time.Hour) }
	if n, err := f.m.MarkLost(ctx); err != nil || n != 1 {
		t.Fatalf("mark lost = %d, %v", n, err)
	}
	f.m.now = time.Now
	f.h.StartWorker()
	deadline := time.Now().Add(10 * time.Second)
	for {
		got, err := f.st.WorkspaceGet(ctx, "acme", ws.ID)
		if err == nil && got.State == store.WorkspaceReady && onDisk(f.h.Dir(ws.ID)) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("after the worker came back: %+v, %v, on disk %v", got, err, onDisk(f.h.Dir(ws.ID)))
		}
		time.Sleep(20 * time.Millisecond)
	}
}
