package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/workspace"
	"github.com/denn-gubsky/loomcycle/internal/workspace/dispatch"
)

// Durable workspaces (RFC GW §4.2, P12 plan §3.2). A durable workspace is
// kept under its tenant, its user and its name past the run that opened it.
// Its release parks it: the pin ends, what runs in it ends, its files stay.
// A later run of the same user opens it again by name and lifetime and is
// pinned to it; two runs never hold it at once, since the pin is a
// compare-and-set on the row. A parked row's owner key is no run id, so no
// run-keyed read reaches it until it is pinned again.

// DurableLimits is workspace.durable.
type DurableLimits struct {
	Enabled    bool
	MaxPerUser int
}

// The errors durable workspaces add.
var (
	// ErrDurableOff: workspace.durable is off.
	ErrDurableOff = errors.New("durable workspaces are off here (workspace.durable.enabled)")
	// ErrDurableNoUser: a run with no user keeps no durable workspace.
	ErrDurableNoUser = errors.New("a durable workspace is kept under its user, and this run has none")
	// ErrDurableLost: the durable workspace's worker is away.
	ErrDurableLost = errors.New("the durable workspace's worker is away; discard it with release {discard: true}, or try again when the worker is back")
	// ErrDurableGrant: the run's grant does not cover what the workspace was
	// opened with (its engine or its routes).
	ErrDurableGrant = errors.New("this agent's workspace grant does not cover the durable workspace")
	// ErrDurableOverMCP: no workspace opened over the MCP server is durable
	// (RFC GW P12 ruling Q3: it keeps D-P9-1's release rule).
	ErrDurableOverMCP = errors.New("durable workspaces are opened inside a run; over the MCP server a workspace ends by the session's release rule")
	// ErrDurableTemplate: a template is only for a new workspace.
	ErrDurableTemplate = errors.New("a template is for a new workspace; this durable workspace exists already (open it without template)")
)

// HeldError: another live run of the user holds the durable workspace.
type HeldError struct {
	Name, AgentID string
}

func (e *HeldError) Error() string {
	return fmt.Sprintf("the durable workspace %q is held by another run of your user (agent %s); it is free again when that run releases it or ends", e.Name, e.AgentID)
}

// SetDurable sets workspace.durable. Boot wiring only.
func (m *Manager) SetDurable(l DurableLimits) { m.durable = l }

// openDurable opens the user's durable workspace of req.Name: made new,
// returned when this run holds it, or pinned to this run when parked or
// held by a run that has ended.
func (m *Manager) openDurable(ctx context.Context, o Owner, req OpenRequest) (store.WorkspaceRow, error) {
	switch {
	case !m.durable.Enabled:
		return store.WorkspaceRow{}, ErrDurableOff
	case o.IsPrincipal():
		return store.WorkspaceRow{}, ErrDurableOverMCP
	case o.UserID == "":
		return store.WorkspaceRow{}, ErrDurableNoUser
	}
	// A few rounds: each loses only to another call that moved the row.
	for range 4 {
		r, err := m.store.WorkspaceGetDurable(ctx, o.TenantID, o.UserID, req.Name)
		var nf *store.ErrNotFound
		if errors.As(err, &nf) {
			row, err := m.openNewDurable(ctx, o, req)
			if errors.Is(err, errDurableRace) {
				continue
			}
			return row, err
		}
		if err != nil {
			return store.WorkspaceRow{}, err
		}
		switch {
		case r.OwnerKind == store.WorkspaceOwnerRun && r.OwnerRunID == o.RunID:
			if r.State != store.WorkspaceReady {
				return store.WorkspaceRow{}, fmt.Errorf("%w: %q is %s", ErrNotReady, req.Name, r.State)
			}
			return r, nil
		case r.State == store.WorkspaceLost:
			return store.WorkspaceRow{}, ErrDurableLost
		case r.State != store.WorkspaceReady:
			return store.WorkspaceRow{}, fmt.Errorf("%w: %q is %s", ErrNotReady, req.Name, r.State)
		case r.OwnerKind == store.WorkspaceOwnerRun:
			ended, err := m.ownerEnded(ctx, r.OwnerRunID)
			if err != nil {
				return store.WorkspaceRow{}, err
			}
			if !ended {
				return store.WorkspaceRow{}, &HeldError{Name: req.Name, AgentID: r.OwnerAgentID}
			}
			// The sweeper has not parked it yet: park it, then take it.
			if err := m.unpin(ctx, r); err != nil {
				return store.WorkspaceRow{}, err
			}
			continue
		case r.OwnerKind == store.WorkspaceOwnerParked:
			row, err := m.pin(ctx, o, req, r)
			if errors.Is(err, errDurableRace) {
				continue
			}
			return row, err
		default:
			return store.WorkspaceRow{}, ErrNotFound
		}
	}
	return store.WorkspaceRow{}, fmt.Errorf("%w: %q kept changing while it was opened; try again", ErrNotReady, req.Name)
}

// errDurableRace: another call moved the row first; read it again.
var errDurableRace = errors.New("durable workspace moved")

// ownerEnded reports whether a pin's run is gone, or ended and not paused
// (a paused run resumes into its workspaces).
func (m *Manager) ownerEnded(ctx context.Context, runID string) (bool, error) {
	run, err := m.store.GetRun(ctx, runID)
	var nf *store.ErrNotFound
	if errors.As(err, &nf) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if run.PauseState == store.PauseStatePaused || run.PauseState == store.PauseStatePausing {
		return false, nil
	}
	return store.IsTerminalRunStatus(run.Status), nil
}

// openNewDurable makes the user's durable workspace of req.Name, as an
// ephemeral one is made, within the user's durable bound.
func (m *Manager) openNewDurable(ctx context.Context, o Owner, req OpenRequest) (store.WorkspaceRow, error) {
	if _, err := m.Get(ctx, o, req.Name); err == nil {
		return store.WorkspaceRow{}, fmt.Errorf("%w: %q (an ephemeral workspace of this run)", ErrNameTaken, req.Name)
	}
	if err := m.checkDurableLimit(ctx, o); err != nil {
		return store.WorkspaceRow{}, err
	}
	row, err := m.openPlaced(ctx, o, req, store.WorkspaceLifetimeDurable)
	if err != nil {
		// Another call made the user's durable row of this name first.
		if errors.Is(err, ErrNotReady) {
			if _, gerr := m.store.WorkspaceGetDurable(ctx, o.TenantID, o.UserID, req.Name); gerr == nil {
				return store.WorkspaceRow{}, errDurableRace
			}
		}
		return store.WorkspaceRow{}, err
	}
	return row, nil
}

// checkDurableLimit refuses a user at workspace.durable.max_per_user.
func (m *Manager) checkDurableLimit(ctx context.Context, o Owner) error {
	limit := m.durable.MaxPerUser
	if limit <= 0 {
		return nil
	}
	n, err := m.store.WorkspaceCountDurableByUser(ctx, o.TenantID, o.UserID)
	if err != nil {
		return err
	}
	if n >= limit {
		return fmt.Errorf("%w: a user keeps at most %d durable workspaces (workspace.durable.max_per_user); discard one first", ErrLimit, limit)
	}
	return nil
}

// pin pins the parked row r to this run. The run's grant must cover what r
// was opened with, its worker must be attached here, and what ran in it
// before is ended first.
func (m *Manager) pin(ctx context.Context, o Owner, req OpenRequest, r store.WorkspaceRow) (store.WorkspaceRow, error) {
	if req.Template != nil {
		return store.WorkspaceRow{}, ErrDurableTemplate
	}
	if err := grantCovers(req.Engine, req.Egress, r); err != nil {
		return store.WorkspaceRow{}, err
	}
	if !m.disp.Attached(r.WorkerID, r.TenantID) {
		return store.WorkspaceRow{}, dispatch.ErrWorkerLost
	}
	// Nothing of the last run may still run in it (a park its worker never
	// got). A worker without the op answers unsupported_op: its session
	// then ends when it is next reaped.
	if err := m.park(ctx, r); err != nil && dispatch.ClassOf(err) != workspace.ClassUnsupportedOp {
		return store.WorkspaceRow{}, err
	}
	ok, err := m.store.WorkspacePin(ctx, r.TenantID, r.ID, r.OwnerRunID, store.WorkspaceOwner{RunID: o.RunID, AgentID: o.AgentID, RootRunID: o.RootRunID})
	var conflict *store.ErrConflict
	if errors.As(err, &conflict) {
		return store.WorkspaceRow{}, fmt.Errorf("%w: %q", ErrNameTaken, req.Name)
	}
	if err != nil {
		return store.WorkspaceRow{}, err
	}
	if !ok {
		return store.WorkspaceRow{}, errDurableRace
	}
	cur, err := m.store.WorkspaceGet(ctx, r.TenantID, r.ID)
	if err != nil {
		return store.WorkspaceRow{}, err
	}
	if err := m.checkLimits(ctx, o); err != nil {
		if _, uerr := m.store.WorkspaceUnpin(ctx, cur.TenantID, cur.ID, o.RunID); uerr != nil {
			m.log.Warn("cannot park a durable workspace a run may not hold", "workspace_id", cur.ID, "err", uerr)
		}
		return store.WorkspaceRow{}, err
	}
	return cur, nil
}

// grantCovers refuses a run whose grant does not cover what the durable
// workspace r was made with: its engine (a run held to a more isolating one
// takes none on a weaker engine) and its routes (each one the run's grant
// names). Pinning it and discarding it both ask it, so a run never takes,
// nor destroys, more than its own grant would have made.
func grantCovers(engine string, egress []string, r store.WorkspaceRow) error {
	// A git workspace (RFC GW P11) holds a repository and a credential
	// reference its ops resolve in the run's name. No grant yet says which
	// runs may take those over, so another run takes none: deny until P11
	// settles it.
	if r.Git != nil || r.Kind == store.WorkspaceKindGit {
		return fmt.Errorf("%w: it is a git workspace, which another run may not take over or discard yet", ErrDurableGrant)
	}
	if !engineAccepted(engine, r.Engine) {
		return fmt.Errorf("%w: it runs on the %s engine, and this agent accepts %s only", ErrDurableGrant, r.Engine, engine)
	}
	for _, route := range r.Egress {
		if !slices.Contains(egress, route) {
			return fmt.Errorf("%w: it may reach the network through the route %q, which this agent's grant does not name", ErrDurableGrant, route)
		}
	}
	return nil
}

// engineAccepted reports whether a workspace on engine meets a run's least
// isolating accepted engine (OpenRequest.Engine): "" accepts any.
func engineAccepted(required, engine string) bool {
	return required == "" || required == engine || (required == workspace.EngineHost && engine == workspace.EngineContainer)
}

// unpin parks r: its checkouts, pages and egress counts go as at a release,
// its shares end, its pin ends (a compare-and-set, so a row another call
// moved is left to it), and then what runs in it on the worker ends. The
// worker's park is best effort: a worker away, or one that fails it, is
// parked again by the next pin.
func (m *Manager) unpin(ctx context.Context, r store.WorkspaceRow) error {
	if r.OwnerKind != store.WorkspaceOwnerRun {
		return nil
	}
	// Ruling Q4: a checkout does not survive the pin.
	m.checkouts.forget(r.ID)
	m.dropExposures(ctx, r)
	m.forgetEgress(r.ID)
	ok, err := m.store.WorkspaceUnpin(ctx, r.TenantID, r.ID, r.OwnerRunID)
	if err != nil {
		return err
	}
	if !ok {
		cur, err := m.store.WorkspaceGet(ctx, r.TenantID, r.ID)
		var nf *store.ErrNotFound
		if errors.As(err, &nf) {
			return nil
		}
		if err != nil {
			return err
		}
		if cur.OwnerKind == store.WorkspaceOwnerParked || cur.OwnerRunID != r.OwnerRunID {
			return nil // parked already, or another run's now
		}
		return fmt.Errorf("workspace %s moved during its release", r.ID)
	}
	if m.disp.Attached(r.WorkerID, r.TenantID) {
		if err := m.park(ctx, r); err != nil {
			m.log.Warn("cannot end what runs in a parked workspace; its next pin ends it", "workspace_id", r.ID, "err", err)
		}
	}
	return nil
}

// park asks r's worker to end what runs in it and keep its files.
func (m *Manager) park(ctx context.Context, r store.WorkspaceRow) error {
	_, err := m.disp.Do(ctx, r.WorkerID, dispatch.Request{TenantID: r.TenantID, WorkspaceID: r.ID, Op: workspace.OpPark})
	return err
}

// ReleaseDiscard removes a workspace, durable or not, as an ephemeral one is
// released (RFC GW §5: release {discard: true}).
func (m *Manager) ReleaseDiscard(ctx context.Context, r store.WorkspaceRow) error {
	return m.remove(ctx, r)
}

// DiscardParked removes the user's durable workspace of name that no live
// run holds: a parked one, or one whose run has ended. One a live run holds
// is refused with *HeldError; none, ErrNotFound. engine and egress are the
// run's grant, which must cover what it was made with, as for a pin
// (ErrDurableGrant). The caller has checked the run's durable grant.
func (m *Manager) DiscardParked(ctx context.Context, o Owner, name, engine string, egress []string) error {
	if o.IsPrincipal() || o.UserID == "" || !ValidName(name) {
		return ErrNotFound
	}
	r, err := m.store.WorkspaceGetDurable(ctx, o.TenantID, o.UserID, name)
	var nf *store.ErrNotFound
	if errors.As(err, &nf) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	// The query asks for these already; a store that ever got it wrong
	// must not let one user discard another's.
	if r.TenantID != o.TenantID || r.UserID != o.UserID || r.Lifetime != store.WorkspaceLifetimeDurable {
		return ErrNotFound
	}
	if r.OwnerKind == store.WorkspaceOwnerRun && r.OwnerRunID != o.RunID {
		ended, err := m.ownerEnded(ctx, r.OwnerRunID)
		if err != nil {
			return err
		}
		if !ended {
			return &HeldError{Name: name, AgentID: r.OwnerAgentID}
		}
	}
	if err := grantCovers(engine, egress, r); err != nil {
		return err
	}
	return m.remove(ctx, r)
}

// EraseDurable removes every durable workspace of user in tenant, pinned or
// parked, as a discard does (RFC GW P12 ruling Q2: a user's erasure), and
// returns how many it removed. A worker away removes the directory at its
// next attach. It is the erasure's: no grant is asked, the operator's
// erasure is.
func (m *Manager) EraseDurable(ctx context.Context, tenantID, userID string) (int, error) {
	if tenantID == "" && userID == "" {
		return 0, nil
	}
	rows, err := m.store.WorkspaceListDurableByUser(ctx, tenantID, userID)
	if err != nil {
		return 0, err
	}
	n := 0
	var errs []error
	for _, r := range rows {
		// The query asks for these already; a store that ever answered
		// wider must not let one user's erasure reach another's.
		if r.TenantID != tenantID || r.UserID != userID || r.Lifetime != store.WorkspaceLifetimeDurable {
			continue
		}
		if err := m.remove(ctx, r); err != nil {
			errs = append(errs, err)
			continue
		}
		n++
	}
	return n, errors.Join(errs...)
}
