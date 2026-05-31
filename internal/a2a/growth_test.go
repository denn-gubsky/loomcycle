package a2a

import (
	"context"
	"fmt"
	"testing"
	"time"

	a2asdk "github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// TestTaskStore_Create_EvictsTerminalTasksAtCap pins the unbounded-growth
// fix: once the in-memory map hits maxInMemoryTasks, Create evicts terminal
// entries (still readable via the run-table fallback) instead of growing
// forever. Regression-grade: the pre-fix Create never deleted, so the map
// would be maxInMemoryTasks+1 here.
func TestTaskStore_Create_EvictsTerminalTasksAtCap(t *testing.T) {
	ts := NewTaskStore(&fakeRuns{byAgentID: map[string]store.Run{}})
	ctx := context.Background()
	for i := 0; i < maxInMemoryTasks; i++ {
		_, _ = ts.Create(ctx, &a2asdk.Task{
			ID:     a2asdk.TaskID(fmt.Sprintf("done-%d", i)),
			Status: a2asdk.TaskStatus{State: a2asdk.TaskStateCompleted},
		})
	}
	if _, err := ts.Create(ctx, &a2asdk.Task{ID: "fresh", Status: a2asdk.TaskStatus{State: a2asdk.TaskStateWorking}}); err != nil {
		t.Fatalf("create fresh: %v", err)
	}
	// All terminal entries evicted; only the fresh non-terminal remains.
	if got := len(ts.tasks); got != 1 {
		t.Fatalf("tasks map = %d after eviction, want 1 (terminals reclaimed)", got)
	}
	if len(ts.versions) != len(ts.tasks) {
		t.Errorf("versions map (%d) drifted from tasks map (%d)", len(ts.versions), len(ts.tasks))
	}
}

// TestTaskStore_Create_KeepsInflightTasks confirms non-terminal (in-flight)
// tasks are never evicted — dropping one would break the SDK's OCC for a
// live run — so the map may exceed the cap when nothing is evictable (the
// floor the run-concurrency semaphore bounds).
func TestTaskStore_Create_KeepsInflightTasks(t *testing.T) {
	ts := NewTaskStore(&fakeRuns{byAgentID: map[string]store.Run{}})
	ctx := context.Background()
	for i := 0; i < maxInMemoryTasks; i++ {
		_, _ = ts.Create(ctx, &a2asdk.Task{
			ID:     a2asdk.TaskID(fmt.Sprintf("live-%d", i)),
			Status: a2asdk.TaskStatus{State: a2asdk.TaskStateWorking},
		})
	}
	_, _ = ts.Create(ctx, &a2asdk.Task{ID: "one-more", Status: a2asdk.TaskStatus{State: a2asdk.TaskStateWorking}})
	if got := len(ts.tasks); got != maxInMemoryTasks+1 {
		t.Fatalf("in-flight tasks = %d, want %d (none evictable)", got, maxInMemoryTasks+1)
	}
}

// TestParkRegistry_ReapsAbandonedParksOnPut pins the abandoned-park leak
// fix: a park older than the TTL is reaped (and its detached background run
// canceled) when a new park is registered. Regression-grade: the pre-fix
// put never reaped, so the stale entry + its goroutine leaked.
func TestParkRegistry_ReapsAbandonedParksOnPut(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	nowFunc = func() time.Time { return base }
	defer func() { nowFunc = time.Now }()

	r := newParkRegistry()
	canceled := false
	r.put("abandoned", &parkedRun{cancel: func() { canceled = true }})

	// Advance past the TTL, then register a fresh park — the stale one is
	// reaped on this put.
	nowFunc = func() time.Time { return base.Add(2 * defaultParkTTL) }
	r.put("fresh", &parkedRun{cancel: func() {}})

	if _, ok := r.take("abandoned"); ok {
		t.Error("abandoned park was not reaped past the TTL")
	}
	if !canceled {
		t.Error("reaped park's cancel() was not called — its background run leaks")
	}
	if _, ok := r.take("fresh"); !ok {
		t.Error("fresh park missing after put")
	}
}

// TestParkRegistry_KeepsFreshParks confirms a within-TTL park is retained.
func TestParkRegistry_KeepsFreshParks(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	nowFunc = func() time.Time { return base }
	defer func() { nowFunc = time.Now }()

	r := newParkRegistry()
	r.put("a", &parkedRun{cancel: func() {}})
	nowFunc = func() time.Time { return base.Add(defaultParkTTL / 2) }
	r.put("b", &parkedRun{cancel: func() {}})

	if _, ok := r.take("a"); !ok {
		t.Error("within-TTL park 'a' was wrongly reaped")
	}
	if _, ok := r.take("b"); !ok {
		t.Error("park 'b' missing")
	}
}
