package a2a

import (
	"context"
	"errors"
	"testing"

	a2asdk "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// TestTaskStore_CreateUpdateGetOCC exercises the in-memory SDK
// bookkeeping path: Create then an OCC-guarded Update, and a stale-
// version Update rejected with ErrConcurrentModification.
func TestTaskStore_CreateUpdateGetOCC(t *testing.T) {
	ts := NewTaskStore(&fakeRuns{byAgentID: map[string]store.Run{}})
	ctx := context.Background()
	task := &a2asdk.Task{ID: "t1", ContextID: "c1", Status: a2asdk.TaskStatus{State: a2asdk.TaskStateSubmitted}}

	v1, err := ts.Create(ctx, task)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := ts.Create(ctx, task); !errors.Is(err, taskstore.ErrTaskAlreadyExists) {
		t.Fatalf("re-create err = %v, want ErrTaskAlreadyExists", err)
	}

	v2, err := ts.Update(ctx, &taskstore.UpdateRequest{Task: task, PrevVersion: v1})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if !v2.After(v1) {
		t.Errorf("version did not advance: v1=%v v2=%v", v1, v2)
	}
	// Stale write rejected.
	if _, err := ts.Update(ctx, &taskstore.UpdateRequest{Task: task, PrevVersion: v1}); !errors.Is(err, taskstore.ErrConcurrentModification) {
		t.Fatalf("stale update err = %v, want ErrConcurrentModification", err)
	}

	got, err := ts.Get(ctx, "t1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Version != v2 {
		t.Errorf("get version = %v, want %v", got.Version, v2)
	}
}

// TestTaskStore_GetFallsThroughToRunTable verifies a Get miss in the
// in-memory map resolves the A2A Task.id as a loomcycle agent_id and
// synthesises a Task from the run row.
func TestTaskStore_GetFallsThroughToRunTable(t *testing.T) {
	ts := NewTaskStore(&fakeRuns{byAgentID: map[string]store.Run{
		"agent-7": {ID: "run-7", AgentID: "agent-7", SessionID: "sess-7", Status: store.RunCompleted, StopReason: "end_turn"},
	}})
	got, err := ts.Get(context.Background(), "agent-7")
	if err != nil {
		t.Fatalf("get fallthrough: %v", err)
	}
	if got.Task.ID != "agent-7" || got.Task.ContextID != "sess-7" {
		t.Errorf("task id/context = %q/%q, want agent-7/sess-7", got.Task.ID, got.Task.ContextID)
	}
	if got.Task.Status.State != a2asdk.TaskStateCompleted {
		t.Errorf("state = %q, want COMPLETED", got.Task.Status.State)
	}
	if got.Version != taskstore.TaskVersionMissing {
		t.Errorf("run-derived task should be version-missing, got %v", got.Version)
	}
}

// TestTaskStore_GetUnknownReturnsTaskNotFound verifies a miss in both
// the map and the run table surfaces a2a.ErrTaskNotFound (the SDK's
// sentinel), not the loomcycle ErrNotFound.
func TestTaskStore_GetUnknownReturnsTaskNotFound(t *testing.T) {
	ts := NewTaskStore(&fakeRuns{byAgentID: map[string]store.Run{}})
	if _, err := ts.Get(context.Background(), "nope"); !errors.Is(err, a2asdk.ErrTaskNotFound) {
		t.Fatalf("get unknown err = %v, want a2a.ErrTaskNotFound", err)
	}
}

// TestTaskStore_ListFiltersByContextAndStatus verifies List honours the
// ContextID and Status filters over the in-memory entries.
func TestTaskStore_ListFiltersByContextAndStatus(t *testing.T) {
	ts := NewTaskStore(&fakeRuns{byAgentID: map[string]store.Run{}})
	ctx := context.Background()
	_, _ = ts.Create(ctx, &a2asdk.Task{ID: "a", ContextID: "ctx-1", Status: a2asdk.TaskStatus{State: a2asdk.TaskStateWorking}})
	_, _ = ts.Create(ctx, &a2asdk.Task{ID: "b", ContextID: "ctx-2", Status: a2asdk.TaskStatus{State: a2asdk.TaskStateWorking}})
	_, _ = ts.Create(ctx, &a2asdk.Task{ID: "c", ContextID: "ctx-1", Status: a2asdk.TaskStatus{State: a2asdk.TaskStateCompleted}})

	resp, err := ts.List(ctx, &a2asdk.ListTasksRequest{ContextID: "ctx-1", Status: a2asdk.TaskStateWorking})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(resp.Tasks) != 1 || resp.Tasks[0].ID != "a" {
		t.Fatalf("filtered list = %#v, want exactly task a", resp.Tasks)
	}
}
