package storetest

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// A status filter of several values is their union, each run once; repeats and
// empty entries change nothing, and a filter of only "" is no filter — what a
// caller passing no status has always meant.
func testListActiveRunsByUserMatchesAnyListedStatus(t *testing.T, s store.Store) {
	ctx := context.Background()
	sess, err := s.CreateSession(ctx, "t", "a", "alice")
	if err != nil {
		t.Fatal(err)
	}
	byStatus := map[store.RunStatus]string{}
	for _, st := range []store.RunStatus{store.RunRunning, store.RunCompleted, store.RunFailed, store.RunCancelled} {
		r, err := s.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_" + string(st), UserID: "alice"})
		if err != nil {
			t.Fatal(err)
		}
		if st != store.RunRunning {
			if err := s.FinishRun(ctx, r.ID, st, "end_turn", store.Usage{}, ""); err != nil {
				t.Fatal(err)
			}
		}
		byStatus[st] = r.ID
	}

	for _, tc := range []struct {
		name   string
		filter []store.RunStatus
		want   []store.RunStatus
	}{
		{"two_statuses", []store.RunStatus{store.RunCompleted, store.RunFailed}, []store.RunStatus{store.RunCompleted, store.RunFailed}},
		{"one_status", []store.RunStatus{store.RunRunning}, []store.RunStatus{store.RunRunning}},
		{"repeated_status", []store.RunStatus{store.RunCancelled, store.RunCancelled}, []store.RunStatus{store.RunCancelled}},
		{"empty_entry_ignored", []store.RunStatus{"", store.RunFailed}, []store.RunStatus{store.RunFailed}},
		{"only_empty_is_no_filter", []store.RunStatus{""}, []store.RunStatus{store.RunRunning, store.RunCompleted, store.RunFailed, store.RunCancelled}},
		{"none_is_no_filter", nil, []store.RunStatus{store.RunRunning, store.RunCompleted, store.RunFailed, store.RunCancelled}},
		{"status_no_run_has", []store.RunStatus{store.RunQueued}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := s.ListActiveRunsByUser(ctx, "", "alice", tc.filter...)
			if err != nil {
				t.Fatal(err)
			}
			var want []string
			for _, st := range tc.want {
				want = append(want, byStatus[st])
			}
			gotIDs := runIDs(got)
			slices.Sort(gotIDs)
			slices.Sort(want)
			if !slices.Equal(gotIDs, want) {
				t.Errorf("filter %v listed %v, want %v", tc.filter, gotIDs, want)
			}
		})
	}
}

// ListRunsByUser pages a user's runs newest first, each exactly once across
// pages, with the status and tenant filters applied before the page bound — and
// a run that starts while the caller is paging lands above the cursor rather
// than shifting a row onto the next page to be read twice.
func testListRunsByUserPagesEveryRunOnceNewestFirst(t *testing.T, s store.Store) {
	ctx := context.Background()
	sess, err := s.CreateSession(ctx, "acme", "a", "alice")
	if err != nil {
		t.Fatal(err)
	}
	other, err := s.CreateSession(ctx, "globex", "a", "alice")
	if err != nil {
		t.Fatal(err)
	}
	var running []string
	for i := 0; i < 23; i++ {
		r, err := s.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: fmt.Sprintf("a_%d", i), UserID: "alice", TenantID: "acme"})
		if err != nil {
			t.Fatal(err)
		}
		// Every third run finished: the running filter has to skip rows inside
		// pages, not only at their edges.
		if i%3 == 0 {
			if err := s.FinishRun(ctx, r.ID, store.RunCompleted, "end_turn", store.Usage{}, ""); err != nil {
				t.Fatal(err)
			}
			continue
		}
		running = append(running, r.ID)
		// Another tenant's run for the same user id between each of ours.
		if _, err := s.CreateRun(ctx, other.ID, store.RunIdentity{AgentID: fmt.Sprintf("g_%d", i), UserID: "alice", TenantID: "globex"}); err != nil {
			t.Fatal(err)
		}
	}

	var (
		all    []store.Run
		cursor string
		pages  int
	)
	for {
		page, next, err := s.ListRunsByUser(ctx, "acme", "alice", []store.RunStatus{store.RunRunning}, 4, cursor)
		if err != nil {
			t.Fatalf("page %d: %v", pages, err)
		}
		pages++
		if len(page) > 4 {
			t.Fatalf("page %d has %d rows, over the limit 4", pages, len(page))
		}
		all = append(all, page...)
		if pages == 1 {
			// Starts mid-listing: newer than every row read so far.
			if _, err := s.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_late", UserID: "alice", TenantID: "acme"}); err != nil {
				t.Fatal(err)
			}
		}
		if next == "" {
			break
		}
		if pages > 100 {
			t.Fatal("listing never ended")
		}
		cursor = next
	}
	if want := (len(running) + 3) / 4; pages != want {
		t.Errorf("listed in %d pages, want %d", pages, want)
	}
	for i := 1; i < len(all); i++ {
		if !runBefore(all[i], all[i-1]) {
			t.Errorf("row %d (%s) is not older than row %d (%s)", i, all[i].ID, i-1, all[i-1].ID)
		}
	}
	got := runIDs(all)
	want := slices.Clone(running)
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("listed %v, want every running acme run once %v", got, want)
	}
	for _, r := range all {
		if r.TenantID != "acme" || r.Status != store.RunRunning {
			t.Errorf("listed a %s run in tenant %q: %s", r.Status, r.TenantID, r.ID)
		}
	}

	for _, c := range []string{"garbage", "run_zz", "msg_0000000000000001_x"} {
		if _, _, err := s.ListRunsByUser(ctx, "acme", "alice", nil, 4, c); !errors.Is(err, store.ErrInvalidRunCursor) {
			t.Errorf("cursor %q: err = %v, want ErrInvalidRunCursor", c, err)
		}
	}
	if got, next, err := s.ListRunsByUser(ctx, "", "", nil, 4, ""); err != nil || len(got) != 0 || next != "" {
		t.Errorf("empty userID: got %d rows, next %q, err %v; want nothing", len(got), next, err)
	}
}
