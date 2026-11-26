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
	WorkspaceKindGeneral = "general"
	// WorkspaceKindGit is a git working tree over a mirror (RFC GW §4.2,
	// P11); its row carries Git.
	WorkspaceKindGit           = "git"
	WorkspaceLifetimeEphemeral = "ephemeral"
	// WorkspaceLifetimeDurable (RFC GW §4.2, P12): kept under the tenant,
	// the user and the name past its run; unique on those three among
	// durable rows.
	WorkspaceLifetimeDurable = "durable"
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
	Shares []WorkspaceShare
	// OwnerKind is who holds the pin: WorkspaceOwnerRun (OwnerRunID is the
	// run) or WorkspaceOwnerPrincipal (RFC GW D35: a session of loomcycle's
	// MCP server; OwnerRunID is the principal's pin key, which no run id
	// can be). Empty on a write is a run.
	OwnerKind string
	// OpenedToken is the token definition that opened a principal's
	// workspace: its end releases it. Empty for a run's.
	OpenedToken string
	// LastUsedAt is when an op last reached a principal's workspace (its
	// idle release); the open stamps it. Not kept for a run's.
	LastUsedAt time.Time
	// Egress are the names of the routes the workspace may send requests
	// out through (RFC GW §6.3), fixed at open. Never nil on a row read
	// back: none is an empty list, and an empty list is no way out.
	Egress []string
	// Root is the name of the user's folder a personal worker offers, for a
	// workspace that is that folder (RFC GW §4.5, P13); empty for every
	// other workspace. A root workspace has a user and is not durable.
	Root string
	// Git is a git workspace's repository (RFC GW §4.2, P11): what the
	// supervisor is told of its remote and branch, never read back from the
	// workspace. nil for every other workspace.
	Git       *WorkspaceGit
	CreatedAt time.Time
	UpdatedAt time.Time
}

// WorkspaceGit is a git workspace's repository, as the core holds it. It
// names a credential, never holds one.
type WorkspaceGit struct {
	// URL is the repository: https, a host, no user, query or fragment.
	URL string `json:"url"`
	// Branch is the workspace's own branch, the one ref a push may move.
	Branch string `json:"branch"`
	// BaseRef is the ref the workspace was opened from; Base the commit it
	// resolved to in the mirror.
	BaseRef string `json:"base_ref,omitempty"`
	Base    string `json:"base,omitempty"`
	// DefaultBranch is the forge's default branch as the core read it from
	// the forge's own advertisement; "" until read.
	DefaultBranch string `json:"default_branch,omitempty"`
	// Credential is the credential reference the workspace's git ops use
	// ("$cred:<name>" or "$ghapp:<name>"); "" for none.
	Credential string `json:"credential,omitempty"`
	// Mirror is the worker's mirror key (gitrepo.MirrorKey of the tenant and
	// the URL).
	Mirror string `json:"mirror"`
}

// The owner kinds of a workspace (RFC GW D35, P12).
const (
	WorkspaceOwnerRun       = "run"
	WorkspaceOwnerPrincipal = "principal"
	// WorkspaceOwnerParked: a durable workspace no run holds (RFC GW P12
	// plan D-P12-1). Its OwnerRunID is WorkspaceParkedKey(id), which no run
	// id can be, so no run-keyed read reaches it. Set only by unpin.
	WorkspaceOwnerParked = "parked"
)

// WorkspaceParkedKey is a parked workspace's OwnerRunID.
func WorkspaceParkedKey(id string) string { return "parked:" + id }

// WorkspaceOwner is the run a durable workspace is pinned to.
type WorkspaceOwner struct {
	RunID, AgentID, RootRunID string
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
	RunID   string `json:"run_id"`
	AgentID string `json:"agent_id"`
	Access  string `json:"access"`
	// Connect lets the peer send HTTP requests to the ports the owner
	// exposed (RFC GW §5.2, D20). Absent on a share stored before it: none.
	Connect   bool      `json:"connect,omitempty"`
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
	// WorkspaceSetGit replaces a git workspace's repository record (the
	// forge's default branch, the base commit, as the core learns them) on
	// the tenant's row. It reports false when there is no such row.
	WorkspaceSetGit(ctx context.Context, tenantID, id string, git WorkspaceGit) (bool, error)
	// WorkspaceDelete removes the tenant's row and reports whether it was
	// there. It never touches the worker's disk.
	WorkspaceDelete(ctx context.Context, tenantID, id string) (bool, error)
	// WorkspaceCountActiveByWorker counts a worker's rows in
	// WorkspaceActiveStates, across tenants: its capacity in use.
	WorkspaceCountActiveByWorker(ctx context.Context, workerID string) (int, error)
	// WorkspaceCountActiveByTenant counts a tenant's rows in
	// WorkspaceActiveStates, across workers: what max_per_tenant bounds.
	WorkspaceCountActiveByTenant(ctx context.Context, tenantID string) (int, error)
	// WorkspaceSweepCandidates returns the run-owned rows, any state, whose
	// owner run is terminal and not paused or pausing, or whose owner run row
	// no longer exists: the sweeper's work list for runs that ended without
	// the run-end release. A paused run is parked, not ended, and keeps its
	// workspaces for its resume. A principal's row is never one: it has no
	// run (WorkspaceListPrincipalOwned is its pass).
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
	// WorkspaceListPrincipalOwned returns every principal-owned row, every
	// tenant and state, oldest first: the work list of the release rule for
	// workspaces opened over the MCP server (RFC GW D35).
	WorkspaceListPrincipalOwned(ctx context.Context) ([]WorkspaceRow, error)
	// WorkspaceTouchUsed sets the tenant's row's last_used_at to at.
	WorkspaceTouchUsed(ctx context.Context, tenantID, id string, at time.Time) error
	// WorkspaceGetDurable returns the tenant's durable row of user and
	// name, pinned or parked; *ErrNotFound when there is none.
	WorkspaceGetDurable(ctx context.Context, tenantID, userID, name string) (WorkspaceRow, error)
	// WorkspacePin pins a durable row to a run: a compare-and-set from the
	// OwnerRunID the caller read (the parked key, or a run that ended), so
	// two runs never both win. False when the row moved first, or is not a
	// durable row of the tenant; *ErrConflict when the run already holds a
	// workspace of that name.
	WorkspacePin(ctx context.Context, tenantID, id, fromOwnerRunID string, to WorkspaceOwner) (bool, error)
	// WorkspaceUnpin parks a durable row pinned to fromRunID: owner kind
	// parked, the parked key, no agent, no root and no shares. False when
	// the row moved first, or is not a pinned durable row of the tenant.
	WorkspaceUnpin(ctx context.Context, tenantID, id, fromRunID string) (bool, error)
}
