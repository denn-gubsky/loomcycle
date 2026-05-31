package a2a

import (
	"context"
	"errors"
	"testing"

	a2asdk "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// crossTenantRuns builds a fakeRuns where agent_id "task-b" resolves to a
// run whose owning session belongs to tenant "tenant-b". A request routed
// to "tenant-a" must not be able to reach it.
func crossTenantRuns() *fakeRuns {
	return &fakeRuns{
		byAgentID: map[string]store.Run{
			"task-b": {ID: "run-b", AgentID: "task-b", SessionID: "sess-b", Status: store.RunRunning},
		},
		bySession: map[string]store.Session{
			"sess-b": {ID: "sess-b", TenantID: "tenant-b"},
		},
	}
}

// TestTaskStore_GetRejectsCrossTenantAgentID_Fallback pins the HIGH
// cross-tenant tasks/get leak via the run-table fallback: a peer routed to
// tenant-a issues tasks/get with tenant-b's agent_id (a non-secret handle)
// and must get ErrTaskNotFound rather than tenant-b's run status/ErrorMsg.
//
// Regression-grade: on the unfixed Get (no tenant gate) this returns a
// synthesised Task for tenant-b's run.
func TestTaskStore_GetRejectsCrossTenantAgentID_Fallback(t *testing.T) {
	ts := NewTaskStore(crossTenantRuns())
	ctx := WithRoutedTenant(context.Background(), "tenant-a")
	if _, err := ts.Get(ctx, "task-b"); !errors.Is(err, a2asdk.ErrTaskNotFound) {
		t.Fatalf("cross-tenant get (fallback) err = %v, want a2a.ErrTaskNotFound", err)
	}
}

// TestTaskStore_GetRejectsCrossTenantAgentID_InMemory covers the same leak
// via the process-global in-memory map (one TaskStore fronts every tenant
// a host/path-routed server serves). Even with the task present in the
// map, a tenant-a request for tenant-b's task must be refused.
//
// Regression-grade: on the unfixed Get the in-memory hit returns first,
// before any tenant consideration.
func TestTaskStore_GetRejectsCrossTenantAgentID_InMemory(t *testing.T) {
	ts := NewTaskStore(crossTenantRuns())
	// Seed the in-memory map with tenant-b's task.
	if _, err := ts.Create(context.Background(), &a2asdk.Task{
		ID: "task-b", ContextID: "sess-b", Status: a2asdk.TaskStatus{State: a2asdk.TaskStateWorking},
	}); err != nil {
		t.Fatalf("seed create: %v", err)
	}
	ctx := WithRoutedTenant(context.Background(), "tenant-a")
	if _, err := ts.Get(ctx, "task-b"); !errors.Is(err, a2asdk.ErrTaskNotFound) {
		t.Fatalf("cross-tenant get (in-memory) err = %v, want a2a.ErrTaskNotFound", err)
	}
}

// TestTaskStore_GetAllowsSameTenant confirms the gate does not block the
// legitimate owner: a request routed to tenant-b resolves tenant-b's task.
func TestTaskStore_GetAllowsSameTenant(t *testing.T) {
	ts := NewTaskStore(crossTenantRuns())
	ctx := WithRoutedTenant(context.Background(), "tenant-b")
	got, err := ts.Get(ctx, "task-b")
	if err != nil {
		t.Fatalf("same-tenant get: %v", err)
	}
	if got.Task.ID != "task-b" {
		t.Errorf("task id = %q, want task-b", got.Task.ID)
	}
}

// TestTaskStore_GetSingleTenantModeUnchanged confirms that with no routing
// mode active (no WithRoutedTenant stamp), Get performs no tenant check and
// behaves exactly as before — the gate is purely additive for multi-tenant
// deployments.
func TestTaskStore_GetSingleTenantModeUnchanged(t *testing.T) {
	ts := NewTaskStore(crossTenantRuns())
	got, err := ts.Get(context.Background(), "task-b") // no routed tenant
	if err != nil {
		t.Fatalf("single-tenant get: %v", err)
	}
	if got.Task.ID != "task-b" {
		t.Errorf("task id = %q, want task-b", got.Task.ID)
	}
}

// TestExecutor_CancelRejectsCrossTenantTaskID pins the HIGH cross-tenant
// cancel: a peer routed to tenant-a sends tasks/cancel with tenant-b's
// agent_id and must be refused with ErrTaskNotFound BEFORE the destructive
// CancelRun fires.
//
// Regression-grade: on the unfixed Cancel, CancelRun is invoked with
// "task-b" and a CANCELED status is emitted.
func TestExecutor_CancelRejectsCrossTenantTaskID(t *testing.T) {
	fc := &fakeConnector{}
	ex := NewExecutor(&fakeRunner{}, fc, crossTenantRuns(), "qa-agent")

	ctx := WithRoutedTenant(context.Background(), "tenant-a")
	execCtx := &a2asrv.ExecutorContext{TaskID: "task-b", ContextID: "c"}
	_, errs := collect(ex.Cancel(ctx, execCtx))

	if len(errs) != 1 || !errors.Is(errs[0], a2asdk.ErrTaskNotFound) {
		t.Fatalf("cross-tenant cancel errs = %v, want exactly [a2a.ErrTaskNotFound]", errs)
	}
	if fc.cancelledAgentID != "" {
		t.Errorf("CancelRun was invoked for agent_id %q; cross-tenant cancel must not reach the connector", fc.cancelledAgentID)
	}
}

// TestExecutor_CancelAllowsSameTenant confirms the legitimate owner
// (routed to tenant-b) still cancels tenant-b's run and gets CANCELED.
func TestExecutor_CancelAllowsSameTenant(t *testing.T) {
	fc := &fakeConnector{}
	ex := NewExecutor(&fakeRunner{}, fc, crossTenantRuns(), "qa-agent")

	ctx := WithRoutedTenant(context.Background(), "tenant-b")
	execCtx := &a2asrv.ExecutorContext{TaskID: "task-b", ContextID: "c"}
	events, errs := collect(ex.Cancel(ctx, execCtx))
	if len(errs) != 0 {
		t.Fatalf("same-tenant cancel errs = %v, want none", errs)
	}
	if fc.cancelledAgentID != "task-b" {
		t.Errorf("cancelled agent_id = %q, want task-b", fc.cancelledAgentID)
	}
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1 (CANCELED)", len(events))
	}
	if su, ok := events[0].(*a2asdk.TaskStatusUpdateEvent); !ok || su.Status.State != a2asdk.TaskStateCanceled {
		t.Errorf("event = %#v, want CANCELED status", events[0])
	}
}
