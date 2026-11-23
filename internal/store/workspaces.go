package store

import (
	"context"
	"time"
)

// WorkspaceState is where a workspace row is in its life (RFC GW §8).
type WorkspaceState string

const (
	// WorkspaceOpening: the row is written and the worker has not yet
	// confirmed the directory. It holds its place in the capacity counts, so
	// two opens racing on different replicas cannot both take the last slot.
	WorkspaceOpening WorkspaceState = "opening"
	// WorkspaceReady: the worker made the directory; ops may run.
	WorkspaceReady WorkspaceState = "ready"
	// WorkspaceReleasing: the owner ended or released it, and the worker has
	// not yet confirmed the removal (it may be away). The worker's next
	// attach removes the directory and then the row goes.
	WorkspaceReleasing WorkspaceState = "releasing"
	// WorkspaceLost: the worker holding it was gone longer than the
	// operator's lost_after_s. Its files are presumed gone.
	WorkspaceLost WorkspaceState = "lost"
)

// WorkspaceActiveStates are the states that count against a worker's and a
// tenant's capacity. A pin reserves its place until it is released (RFC GW
// §8 "Placement"), so releasing and lost rows do not count: a tenant whose
// worker went away must not be locked out by rows of runs that have ended.
var WorkspaceActiveStates = []WorkspaceState{WorkspaceOpening, WorkspaceReady}

// IsValid reports whether s is one of the four states.
func (s WorkspaceState) IsValid() bool {
	switch s {
	case WorkspaceOpening, WorkspaceReady, WorkspaceReleasing, WorkspaceLost:
		return true
	}
	return false
}

// The kind and lifetime P1 writes. Later phases add git workspaces (P11) and
// durable ones (P12); the store does not restrict the values, so those phases
// need no store change for them.
const (
	WorkspaceKindGeneral       = "general"
	WorkspaceLifetimeEphemeral = "ephemeral"
)

// WorkspaceRow is one workspace on one worker (RFC GW §4.2, §8). The row is
// what every replica routes an op by, and its OwnerRunID is the pin: only the
// run that opened the workspace may use it.
type WorkspaceRow struct {
	// ID names the workspace on the worker's disk. The caller mints it.
	ID       string
	TenantID string
	UserID   string
	// Name is the agent's name for it, unique within the owner run.
	Name     string
	WorkerID string
	Kind     string
	Lifetime string
	Engine   string
	// OwnerRunID is the run that opened it: the pin. Release and the sweeper
	// key on this run, not on the run tree (RFC GW P1 plan §2).
	OwnerRunID   string
	OwnerAgentID string
	// RootRunID is the owner's top-level run, kept for listing by tree; it
	// decides nothing.
	RootRunID string
	State     WorkspaceState
	// Shares are the runs the owner shared the workspace with (RFC GW §5.2),
	// in the order they were made. Never nil on a row read back.
	Shares    []WorkspaceShare
	CreatedAt time.Time
	UpdatedAt time.Time
}

// The accesses a share gives (RFC GW §5.2, D20).
const (
	WorkspaceShareRead      = "read"
	WorkspaceShareReadWrite = "read_write"
)

// WorkspaceShare is one share of a workspace: the run it is with, by its run
// id (which no later run reuses, so a share ends with that run) and the agent
// id it was named by, and what it may do.
type WorkspaceShare struct {
	RunID     string    `json:"run_id"`
	AgentID   string    `json:"agent_id"`
	Access    string    `json:"access"`
	CreatedAt time.Time `json:"created_at"`
}

// WorkspaceStore persists workspace rows (RFC GW). Every read a request path
// makes takes the tenant and treats another tenant's row as absent. The
// worker-keyed reads do not: a worker's bookkeeping (capacity, what to remove
// at attach) is about its disk, whichever tenants it serves.
type WorkspaceStore interface {
	// WorkspaceCreate writes a new row, stamping both timestamps. An empty
	// State is opening, an empty Kind general and an empty Lifetime
	// ephemeral. A second row on the same (OwnerRunID, Name), or the same ID,
	// is refused with *ErrConflict.
	WorkspaceCreate(ctx context.Context, row WorkspaceRow) (WorkspaceRow, error)
	// WorkspaceGet returns the tenant's row with this id; *ErrNotFound when
	// there is none, or it is another tenant's.
	WorkspaceGet(ctx context.Context, tenantID, id string) (WorkspaceRow, error)
	// WorkspaceGetByName returns the owner run's workspace of this name in
	// the tenant; *ErrNotFound when there is none.
	WorkspaceGetByName(ctx context.Context, tenantID, ownerRunID, name string) (WorkspaceRow, error)
	// WorkspaceListByOwnerRun returns the run's workspaces in the tenant,
	// every state, ordered by name. Empty, not an error, when it has none.
	WorkspaceListByOwnerRun(ctx context.Context, tenantID, ownerRunID string) ([]WorkspaceRow, error)
	// WorkspaceListByWorker returns the rows placed on a worker, in the given
	// states (every state when none is given), oldest first. With
	// WorkspaceReleasing it is what the worker's next attach removes.
	WorkspaceListByWorker(ctx context.Context, workerID string, states ...WorkspaceState) ([]WorkspaceRow, error)
	// WorkspaceSetState moves the tenant's row from one state to another and
	// bumps updated_at. It reports false, and changes nothing, when the row
	// is absent or not in from — so of two replicas moving one row, one wins.
	WorkspaceSetState(ctx context.Context, tenantID, id string, from, to WorkspaceState) (bool, error)
	// WorkspaceDelete removes the tenant's row and reports whether it was
	// there. It never touches the worker's disk.
	WorkspaceDelete(ctx context.Context, tenantID, id string) (bool, error)
	// WorkspaceCountActiveByWorker counts a worker's rows in
	// WorkspaceActiveStates, across tenants: its capacity in use.
	WorkspaceCountActiveByWorker(ctx context.Context, workerID string) (int, error)
	// WorkspaceCountActiveByTenant counts a tenant's rows in
	// WorkspaceActiveStates, across workers: what max_per_tenant bounds.
	WorkspaceCountActiveByTenant(ctx context.Context, tenantID string) (int, error)
	// WorkspaceSweepCandidates returns the rows, any state, whose owner run
	// is terminal and not paused or pausing, or whose owner run row no longer
	// exists: the sweeper's work list for runs that ended without the run-end
	// release. A paused run is parked, not ended, and keeps its workspaces for
	// its resume.
	WorkspaceSweepCandidates(ctx context.Context) ([]WorkspaceRow, error)
	// WorkspaceTouchByWorker sets updated_at to now on the worker's rows in
	// WorkspaceActiveStates and returns how many. A replica the worker is
	// attached to calls it now and then, so a row a live worker holds never
	// looks abandoned to WorkspaceListStale, whichever replica asks.
	WorkspaceTouchByWorker(ctx context.Context, workerID string) (int, error)
	// WorkspaceListStale returns the rows, every tenant, in
	// WorkspaceActiveStates whose updated_at is before the given time, oldest
	// first: those of a worker no replica has seen since then.
	WorkspaceListStale(ctx context.Context, before time.Time) ([]WorkspaceRow, error)
	// WorkspaceSetShares replaces the tenant's row's shares with to, when
	// they are still from (compared as stored), and bumps updated_at. It
	// reports false, and changes nothing, when the row is absent or its
	// shares moved since the caller read them, so of two writers one wins.
	WorkspaceSetShares(ctx context.Context, tenantID, id string, from, to []WorkspaceShare) (bool, error)
	// WorkspaceListByOwnerAgent returns the tenant's rows of this name whose
	// owner run carried ownerAgentID, every state, oldest first: how a
	// share's peer names a workspace (name@agent_id). Several when runs
	// reused one agent id.
	WorkspaceListByOwnerAgent(ctx context.Context, tenantID, ownerAgentID, name string) ([]WorkspaceRow, error)
	// WorkspaceListSharedWith returns the tenant's rows whose shares name
	// the run, every state, ordered by name.
	WorkspaceListSharedWith(ctx context.Context, tenantID, runID string) ([]WorkspaceRow, error)
}
