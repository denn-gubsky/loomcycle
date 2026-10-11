package storetest

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// A run's listing by parent is its direct children, each exactly once and in
// (started_at, id) order across pages, whatever their status — and nothing
// else: not the run itself, not a grandchild, not another run's child.
func testListChildRunsPagesEveryChildOnceInOrder(t *testing.T, s store.Store) {
	ctx := context.Background()
	sess, err := s.CreateSession(ctx, "t", "lead", "alice")
	if err != nil {
		t.Fatal(err)
	}
	child := func(parent, agent string) store.Run {
		t.Helper()
		r, err := s.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: agent, ParentRunID: parent, UserID: "alice", TenantID: "t"})
		if err != nil {
			t.Fatalf("CreateRun %s: %v", agent, err)
		}
		return r
	}
	parent := child("", "lead")
	other := child("", "other")
	want := map[string]bool{}
	const children = 250
	var first store.Run
	for i := 0; i < children; i++ {
		r := child(parent.ID, fmt.Sprintf("c_%d", i))
		want[r.ID] = true
		if i == 0 {
			first = r
		}
	}
	// One child has ended: a listing by parent is not a listing of what runs.
	if err := s.FinishRun(ctx, first.ID, store.RunCompleted, "", store.Usage{}, ""); err != nil {
		t.Fatal(err)
	}
	child(first.ID, "grandchild")
	child(other.ID, "others_child")

	var (
		got    []store.Run
		cursor string
		pages  int
	)
	for {
		page, next, err := s.ListChildRuns(ctx, "", parent.ID, 100, cursor)
		if err != nil {
			t.Fatalf("ListChildRuns page %d: %v", pages, err)
		}
		pages++
		if len(page) > 100 {
			t.Fatalf("page %d has %d rows, over the limit 100", pages, len(page))
		}
		got = append(got, page...)
		if next == "" || pages > 10 {
			break
		}
		cursor = next
	}
	if pages != 3 {
		t.Errorf("250 children at 100 per page took %d pages, want 3", pages)
	}
	seen := map[string]bool{}
	ended := 0
	for i, r := range got {
		if !want[r.ID] {
			t.Errorf("listed %s (agent %s), which is not a child of %s", r.ID, r.AgentID, parent.ID)
		}
		if seen[r.ID] {
			t.Errorf("listed %s twice", r.ID)
		}
		seen[r.ID] = true
		if r.Status == store.RunCompleted {
			ended++
		}
		if i > 0 && !runBefore(got[i-1], r) {
			t.Errorf("row %d (%s) does not follow row %d (%s)", i, r.ID, i-1, got[i-1].ID)
		}
	}
	if len(seen) != children || ended != 1 {
		t.Errorf("listed %d distinct children, %d of them ended; want %d and 1", len(seen), ended, children)
	}

	if none, next, err := s.ListChildRuns(ctx, "", "", 10, ""); err != nil || len(none) != 0 || next != "" {
		t.Errorf("an empty parent listed %v (next %q, err %v)", runIDs(none), next, err)
	}
	if none, _, err := s.ListChildRuns(ctx, "", "r_no_such_run", 10, ""); err != nil || len(none) != 0 {
		t.Errorf("an unknown parent listed %v (err %v)", runIDs(none), err)
	}
}

// The tenant filter is in the query, and a cursor this store did not issue is
// refused.
func testListChildRunsTenantFilterAndCursor(t *testing.T, s store.Store) {
	ctx := context.Background()
	sessA, _ := s.CreateSession(ctx, "tenant-a", "lead", "alice")
	sessB, _ := s.CreateSession(ctx, "tenant-b", "a", "bob")
	parent, err := s.CreateRun(ctx, sessA.ID, store.RunIdentity{AgentID: "lead", UserID: "alice", TenantID: "tenant-a"})
	if err != nil {
		t.Fatal(err)
	}
	mine, _ := s.CreateRun(ctx, sessA.ID, store.RunIdentity{AgentID: "c_a", ParentRunID: parent.ID, UserID: "alice", TenantID: "tenant-a"})
	theirs, _ := s.CreateRun(ctx, sessB.ID, store.RunIdentity{AgentID: "c_b", ParentRunID: parent.ID, UserID: "bob", TenantID: "tenant-b"})

	got, next, err := s.ListChildRuns(ctx, "tenant-a", parent.ID, 10, "")
	if err != nil || next != "" {
		t.Fatalf("ListChildRuns(tenant-a) = next %q, err %v", next, err)
	}
	if ids := runIDs(got); len(ids) != 1 || ids[0] != mine.ID {
		t.Errorf("tenant-a listing = %v, want %s", ids, mine.ID)
	}
	all, _, _ := s.ListChildRuns(ctx, "", parent.ID, 10, "")
	if ids := runIDs(all); len(ids) != 2 || !containsAll(ids, mine.ID, theirs.ID) {
		t.Errorf("all-tenants listing = %v, want %s and %s", ids, mine.ID, theirs.ID)
	}
	for _, c := range []string{"not-a-cursor", "run_"} {
		if _, _, err := s.ListChildRuns(ctx, "", parent.ID, 10, c); !errors.Is(err, store.ErrInvalidRunCursor) {
			t.Errorf("cursor %q: err = %v, want ErrInvalidRunCursor", c, err)
		}
	}
}
