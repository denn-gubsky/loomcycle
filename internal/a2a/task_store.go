package a2a

import (
	"context"
	"errors"
	"sync"
	"time"

	a2asdk "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// RunReader is the read-only slice of store.Store the task store needs.
// Narrowed so tests inject a fake without the full Store surface, and so
// the bridge cannot accidentally mutate the run table (run rows are owned
// by the loop; A2A never writes them).
type RunReader interface {
	GetRun(ctx context.Context, runID string) (store.Run, error)
	GetRunByAgentID(ctx context.Context, agentID string) (store.Run, error)
	// GetSession resolves a run's owning session. Its TenantID is the
	// authoritative tenant for A2A cross-tenant scoping on the get/cancel
	// paths: the runs table carries NO tenant column — tenant lives on the
	// session (store.Session.TenantID) — so resolving the run's tenant
	// means a run→session hop. store.Store satisfies this.
	GetSession(ctx context.Context, sessionID string) (store.Session, error)
}

// TaskStore implements the SDK's taskstore.Store over loomcycle's run
// model. It serves two distinct read paths:
//
//   - In-flight SDK bookkeeping: the SDK creates a Task when a message
//     arrives, then Updates it as the executor emits events, using
//     optimistic concurrency control (PrevVersion). Those reads/writes
//     hit an in-memory map keyed by a2a.TaskID. This is where the SDK's
//     OCC machinery lives; backing it with the immutable run row would
//     be wrong (run rows have no version column and are loop-owned).
//
//   - Run-table fallback for Get: when a Task isn't in the in-memory
//     map (e.g. a tasks/get for a run this process didn't start, or
//     after the in-memory entry was evicted), Get resolves the A2A
//     Task.id as a loomcycle agent_id and synthesises a Task from the
//     run row's status. This is the addressable-handle bridge: an A2A
//     Task.id IS a loomcycle agent_id.
//
// The in-memory map is process-local and not durable; cross-replica
// task addressing is a later-slice concern (it rides on the run table,
// which IS shared). Lossy eviction is not implemented this slice — the
// map grows with active tasks and is expected to be bounded by the
// run concurrency semaphore upstream.
type TaskStore struct {
	runs RunReader

	mu       sync.RWMutex
	tasks    map[a2asdk.TaskID]*taskstore.StoredTask
	versions map[a2asdk.TaskID]taskstore.TaskVersion
}

var _ taskstore.Store = (*TaskStore)(nil)

// NewTaskStore builds a TaskStore backed by the given run reader.
func NewTaskStore(runs RunReader) *TaskStore {
	return &TaskStore{
		runs:     runs,
		tasks:    make(map[a2asdk.TaskID]*taskstore.StoredTask),
		versions: make(map[a2asdk.TaskID]taskstore.TaskVersion),
	}
}

// Create records a new task in the in-memory map. Returns
// taskstore.ErrTaskAlreadyExists if the id is already present.
func (s *TaskStore) Create(ctx context.Context, task *a2asdk.Task) (taskstore.TaskVersion, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.tasks[task.ID]; exists {
		return taskstore.TaskVersionMissing, taskstore.ErrTaskAlreadyExists
	}
	const firstVersion taskstore.TaskVersion = 1
	s.tasks[task.ID] = &taskstore.StoredTask{Task: task, Version: firstVersion}
	s.versions[task.ID] = firstVersion
	return firstVersion, nil
}

// Update applies an OCC-guarded update. Returns
// taskstore.ErrConcurrentModification when PrevVersion does not match
// the stored version, and a2a.ErrTaskNotFound when the task is unknown.
func (s *TaskStore) Update(ctx context.Context, req *taskstore.UpdateRequest) (taskstore.TaskVersion, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, exists := s.versions[req.Task.ID]
	if !exists {
		return taskstore.TaskVersionMissing, a2asdk.ErrTaskNotFound
	}
	if req.PrevVersion != cur {
		return taskstore.TaskVersionMissing, taskstore.ErrConcurrentModification
	}
	next := cur + 1
	s.tasks[req.Task.ID] = &taskstore.StoredTask{Task: req.Task, Version: next}
	s.versions[req.Task.ID] = next
	return next, nil
}

// Get returns the stored task. In-memory entries win; on a miss it
// falls through to the run table, treating the A2A Task.id as a
// loomcycle agent_id. Returns a2a.ErrTaskNotFound when neither has it.
func (s *TaskStore) Get(ctx context.Context, taskID a2asdk.TaskID) (*taskstore.StoredTask, error) {
	// Tenant gate FIRST, before either the in-memory hit or the run-table
	// fallback can return a task. The A2A Task.id IS a caller-supplied
	// loomcycle agent_id (a non-secret addressable handle), and the
	// in-memory map is process-global across every tenant a host/path-
	// routed server fronts — so without this a peer authenticated on
	// tenant-A's routed host could read tenant-B's task (status + the raw
	// ErrorMsg taskFromRun packs in). A no-op in single-tenant/none mode.
	if err := authorizeTaskTenant(ctx, s.runs, string(taskID)); err != nil {
		return nil, err
	}

	s.mu.RLock()
	stored, ok := s.tasks[taskID]
	s.mu.RUnlock()
	if ok {
		return stored, nil
	}

	run, err := s.runs.GetRunByAgentID(ctx, string(taskID))
	if err != nil {
		var nf *store.ErrNotFound
		if errors.As(err, &nf) {
			return nil, a2asdk.ErrTaskNotFound
		}
		return nil, err
	}
	task, ok := taskFromRun(run)
	if !ok {
		// Run exists but its status doesn't map to a known TaskState
		// (would be a bug — the four RunStatus values are all mapped).
		return nil, a2asdk.ErrTaskNotFound
	}
	// Version-missing: run-table-derived tasks aren't OCC-tracked
	// (they're a read snapshot of a loop-owned row, not an A2A task we
	// mutate).
	return &taskstore.StoredTask{Task: task, Version: taskstore.TaskVersionMissing}, nil
}

// List enumerates in-memory tasks. The run-table fallback is not
// listable (no efficient by-status scan keyed to A2A semantics this
// slice); callers that need run enumeration use the loomcycle ListRuns
// surface. Honours req.ContextID and req.Status filters when set.
func (s *TaskStore) List(ctx context.Context, req *a2asdk.ListTasksRequest) (*a2asdk.ListTasksResponse, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*a2asdk.Task, 0, len(s.tasks))
	for _, st := range s.tasks {
		if req != nil {
			if req.ContextID != "" && st.Task.ContextID != req.ContextID {
				continue
			}
			if req.Status != a2asdk.TaskStateUnspecified && st.Task.Status.State != req.Status {
				continue
			}
		}
		out = append(out, st.Task)
	}
	return &a2asdk.ListTasksResponse{
		Tasks:     out,
		TotalSize: len(out),
		PageSize:  len(out),
	}, nil
}

// taskFromRun synthesises an A2A Task snapshot from a loomcycle run row.
// The A2A Task.id is the run's agent_id (the addressable handle); the
// state comes from the shared statemap. Second return is false when the
// run's status is unmapped (a bug — caller surfaces ErrTaskNotFound).
func taskFromRun(run store.Run) (*a2asdk.Task, bool) {
	state, ok := TaskStateForRunStatus(run.Status)
	if !ok {
		return nil, false
	}
	now := time.Now()
	detail := run.StopReason
	if run.Status == store.RunFailed && run.ErrorMsg != "" {
		detail = run.ErrorMsg
	}
	var statusMsg *a2asdk.Message
	if detail != "" {
		statusMsg = a2asdk.NewMessage(a2asdk.MessageRoleAgent, a2asdk.NewTextPart(detail))
	}
	return &a2asdk.Task{
		ID:        a2asdk.TaskID(run.AgentID),
		ContextID: run.SessionID,
		Status: a2asdk.TaskStatus{
			State:     state,
			Message:   statusMsg,
			Timestamp: &now,
		},
	}, true
}
