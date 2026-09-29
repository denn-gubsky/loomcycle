package storetest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// walkMember is the lineage a walk stamps on every member it spawns.
func walkMember(walkID string) *store.ParentContext {
	return &store.ParentContext{WalkID: walkID, State: "a", StateVisit: 1}
}

// pageAllByWalk reads every page of a walk's listing, failing on a page that
// breaks the (started_at, id) order across the page boundary.
func pageAllByWalk(t *testing.T, s store.Store, tenant, walkID string, limit int) ([]store.Run, int) {
	t.Helper()
	var (
		all    []store.Run
		cursor string
		pages  int
	)
	for {
		page, next, err := s.ListRunsByWalk(context.Background(), tenant, walkID, limit, cursor)
		if err != nil {
			t.Fatalf("ListRunsByWalk page %d: %v", pages, err)
		}
		pages++
		if len(page) > limit {
			t.Fatalf("page %d has %d rows, over the limit %d", pages, len(page), limit)
		}
		all = append(all, page...)
		if next == "" {
			return all, pages
		}
		if pages > 1000 {
			t.Fatalf("listing never ended")
		}
		cursor = next
	}
}

// runBefore is the listing's order: started_at, then id.
func runBefore(a, b store.Run) bool {
	if !a.StartedAt.Equal(b.StartedAt) {
		return a.StartedAt.Before(b.StartedAt)
	}
	return a.ID < b.ID
}

// A walk's listing is its own run plus every member, each exactly once and in
// (started_at, id) order across pages — and nothing else: not another walk's
// members, not a run that only shares the caller's lineage.
func testListRunsByWalkPagesEveryMemberOnceInOrder(t *testing.T, s store.Store) {
	ctx := context.Background()
	sess, err := s.CreateSession(ctx, "t", "team:w", "alice")
	if err != nil {
		t.Fatal(err)
	}
	walk, err := s.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "team:w", UserID: "alice", TenantID: "t"})
	if err != nil {
		t.Fatal(err)
	}
	other, err := s.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "team:w", UserID: "alice", TenantID: "t"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{walk.ID: true}
	const members = 250
	for i := 0; i < members; i++ {
		r, err := s.CreateRun(ctx, sess.ID, store.RunIdentity{
			AgentID: fmt.Sprintf("m_%d", i), UserID: "alice", TenantID: "t", ParentContext: walkMember(walk.ID),
		})
		if err != nil {
			t.Fatalf("CreateRun member %d: %v", i, err)
		}
		want[r.ID] = true
	}
	// Noise: a member of another walk, and a run whose lineage names the walk
	// as its caller-side root without being a member of it.
	if _, err := s.CreateRun(ctx, sess.ID, store.RunIdentity{
		AgentID: "m_other", UserID: "alice", TenantID: "t", ParentContext: walkMember(other.ID),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateRun(ctx, sess.ID, store.RunIdentity{
		AgentID: "m_root", UserID: "alice", TenantID: "t",
		ParentContext: &store.ParentContext{RootAgentRunID: walk.ID},
	}); err != nil {
		t.Fatal(err)
	}

	got, pages := pageAllByWalk(t, s, "", walk.ID, 100)
	if pages != 3 {
		t.Errorf("251 runs at 100 per page took %d pages, want 3", pages)
	}
	seen := map[string]bool{}
	for i, r := range got {
		if !want[r.ID] {
			t.Errorf("listed %s (agent %s), which is not in walk %s", r.ID, r.AgentID, walk.ID)
		}
		if seen[r.ID] {
			t.Errorf("listed %s twice", r.ID)
		}
		seen[r.ID] = true
		if i > 0 && !runBefore(got[i-1], r) {
			t.Errorf("row %d (%s @ %v) does not follow row %d (%s @ %v)",
				i, r.ID, r.StartedAt, i-1, got[i-1].ID, got[i-1].StartedAt)
		}
	}
	if len(seen) != len(want) {
		t.Errorf("listed %d distinct runs, want %d (the walk + %d members)", len(seen), len(want), members)
	}
	if !seen[walk.ID] {
		t.Errorf("the walk's own run %s is not listed", walk.ID)
	}
}

// The tenant filter is in the query: a row in another tenant claiming the walk
// is not listed under the walk's tenant, and "" lists every tenant.
func testListRunsByWalkTenantFilter(t *testing.T, s store.Store) {
	ctx := context.Background()
	sessA, _ := s.CreateSession(ctx, "tenant-a", "team:w", "alice")
	sessB, _ := s.CreateSession(ctx, "tenant-b", "a", "bob")
	walk, err := s.CreateRun(ctx, sessA.ID, store.RunIdentity{AgentID: "team:w", UserID: "alice", TenantID: "tenant-a"})
	if err != nil {
		t.Fatal(err)
	}
	mine, _ := s.CreateRun(ctx, sessA.ID, store.RunIdentity{
		AgentID: "m_a", UserID: "alice", TenantID: "tenant-a", ParentContext: walkMember(walk.ID),
	})
	theirs, _ := s.CreateRun(ctx, sessB.ID, store.RunIdentity{
		AgentID: "m_b", UserID: "bob", TenantID: "tenant-b", ParentContext: walkMember(walk.ID),
	})

	got, next, err := s.ListRunsByWalk(ctx, "tenant-a", walk.ID, 10, "")
	if err != nil || next != "" {
		t.Fatalf("ListRunsByWalk(tenant-a) = next %q, err %v", next, err)
	}
	if ids := runIDs(got); len(ids) != 2 || !containsAll(ids, walk.ID, mine.ID) {
		t.Errorf("tenant-a listing = %v, want %s and %s", ids, walk.ID, mine.ID)
	}
	all, _, _ := s.ListRunsByWalk(ctx, "", walk.ID, 10, "")
	if ids := runIDs(all); len(ids) != 3 || !containsAll(ids, walk.ID, mine.ID, theirs.ID) {
		t.Errorf("all-tenants listing = %v, want %s, %s and %s", ids, walk.ID, mine.ID, theirs.ID)
	}
	if none, _, _ := s.ListRunsByWalk(ctx, "tenant-b", "r_no_such_walk", 10, ""); len(none) != 0 {
		t.Errorf("an unknown walk listed %v", runIDs(none))
	}
}

// Every writer of a runs row writes the walk column from the same lineage it
// writes to parent_context: a configured run and a snapshot-restored run are
// listed like a created one, and each listed member's lineage names the walk.
func testListRunsByWalkEveryWriterStampsTheColumn(t *testing.T, s store.Store) {
	ctx := context.Background()
	sess, _ := s.CreateSession(ctx, "t", "team:w", "alice")
	walk, err := s.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "team:w", UserID: "alice", TenantID: "t"})
	if err != nil {
		t.Fatal(err)
	}
	created, err := s.CreateRun(ctx, sess.ID, store.RunIdentity{
		AgentID: "m_created", UserID: "alice", TenantID: "t", ParentContext: walkMember(walk.ID),
	})
	if err != nil {
		t.Fatal(err)
	}
	configured, err := s.CreateConfiguredRun(ctx, sess.ID, store.RunIdentity{
		AgentID: "m_configured", UserID: "alice", TenantID: "t", ParentContext: walkMember(walk.ID),
	}, json.RawMessage(`{"segments":[{"role":"user"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	restored := store.Run{
		ID: "r_restored_walk_member", SessionID: sess.ID, Status: store.RunCompleted,
		StartedAt: time.Now().Add(time.Hour).UTC().Truncate(time.Microsecond),
		AgentID:   "m_restored", UserID: "alice", ParentContext: walkMember(walk.ID),
	}
	if ok, err := s.SnapshotRestoreRun(ctx, restored); err != nil || !ok {
		t.Fatalf("SnapshotRestoreRun = %v, %v", ok, err)
	}

	got, _, err := s.ListRunsByWalk(ctx, "", walk.ID, 10, "")
	if err != nil {
		t.Fatal(err)
	}
	listed := map[string]store.Run{}
	for _, r := range got {
		listed[r.ID] = r
	}
	for name, id := range map[string]string{
		"CreateRun": created.ID, "CreateConfiguredRun": configured.ID, "SnapshotRestoreRun": restored.ID,
	} {
		r, ok := listed[id]
		if !ok {
			t.Errorf("a %s member (%s) is not listed", name, id)
			continue
		}
		if r.ParentContext == nil || r.ParentContext.WalkID != walk.ID {
			t.Errorf("the %s member's parent_context = %+v, want walk %s", name, r.ParentContext, walk.ID)
		}
	}
	if len(got) != 4 {
		t.Errorf("listed %v, want the walk and three members", runIDs(got))
	}
}

// A cursor the store did not issue is refused, not read as "from the start".
func testListRunsByWalkRefusesForeignCursor(t *testing.T, s store.Store) {
	for _, c := range []string{"garbage", "run_zz", "run_0000000000000000_", "cur_0", "run_0000000000000001_a;b"} {
		if _, _, err := s.ListRunsByWalk(context.Background(), "", "r_w", 10, c); !errors.Is(err, store.ErrInvalidRunCursor) {
			t.Errorf("cursor %q: err = %v, want ErrInvalidRunCursor", c, err)
		}
	}
}

func containsAll(ids []string, want ...string) bool {
	have := map[string]bool{}
	for _, id := range ids {
		have[id] = true
	}
	for _, w := range want {
		if !have[w] {
			return false
		}
	}
	return true
}

func runIDs(rs []store.Run) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.ID
	}
	return out
}
