// Package lifecycle is the core's bookkeeping of workspaces (RFC GW §4.2,
// §8): opening one on a worker for a run, finding a run's own, running an op
// in one, and releasing them when the run ends, when the run asks, or when
// the sweeper finds a run that ended without the release.
//
// The pin is here. Every lookup is keyed on the tenant AND the owner run, so
// a run reaches only the workspaces it opened: a sibling, a child, a parent,
// another user's run of the same tenant get "not found", the same answer as
// for a name nobody uses.
package lifecycle

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/audit"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/workspace"
	"github.com/denn-gubsky/loomcycle/internal/workspace/dispatch"
	"github.com/denn-gubsky/loomcycle/internal/workspace/egress"
)

// Store is what the manager reads and writes: the workspaces table, and runs
// for the run-end release's pause check and a share's runs.
type Store interface {
	store.WorkspaceStore
	store.WorkspaceTemplateStore
	store.WorkspaceExposureStore
	GetRun(ctx context.Context, runID string) (store.Run, error)
	// GetRunByAgentID resolves the run a share names (RFC GW P8).
	GetRunByAgentID(ctx context.Context, agentID string) (store.Run, error)
}

// Limits are the operator's workspace bounds (config workspace:, resolved).
type Limits struct {
	MaxPerRun    int
	MaxPerTenant int // 0: unlimited
	// LostAfter is how long a worker may go unseen by every replica before
	// its workspaces are marked lost.
	LostAfter time.Duration
	// DiskQuotaBytes is each workspace's disk quota, sent to its worker at
	// open; zero is the worker's default (workspace.DefaultDiskQuotaBytes).
	DiskQuotaBytes int64
	// CopyMaxFiles and CopyMaxBytes bound a folder copy
	// (workspace.copy.max_files, max_bytes); zero is DefaultCopyMaxFiles and
	// DefaultCopyMaxBytes.
	CopyMaxFiles int
	CopyMaxBytes int64
	// CheckoutMaxDocuments and CheckoutMaxBytes bound File op=checkout of
	// Documents into a workspace (workspace.checkout); zero is
	// DefaultCheckoutMaxDocuments and DefaultCheckoutMaxBytes.
	CheckoutMaxDocuments int
	CheckoutMaxBytes     int64
	// MaxCheckpoints bounds a workspace's checkpoints
	// (workspace.checkpoint.max_per_workspace); zero is the worker's
	// default, workspace.DefaultMaxCheckpoints.
	MaxCheckpoints int
	// TemplateMaxBytes bounds one template's archive
	// (workspace.template.max_mb), MaxTemplates a tenant's templates
	// (workspace.template.max_per_tenant); zero is DefaultTemplateMaxBytes
	// and DefaultMaxTemplates.
	TemplateMaxBytes int64
	MaxTemplates     int
	// MaxTemplatesPerUser bounds one user's user templates
	// (workspace.template.max_per_user), so one member cannot fill the
	// tenant's bound; zero is DefaultMaxTemplatesPerUser.
	MaxTemplatesPerUser int
	// ConvertQuarantine is how long a file that crashed a worker's
	// conversion child is refused for its workspace
	// (conversion.quarantine_ttl_s); zero is DefaultConvertQuarantine.
	ConvertQuarantine time.Duration
}

// The folder-copy bounds of a Limits that names none: config
// workspace.copy's defaults.
const (
	DefaultCopyMaxFiles = 2000
	DefaultCopyMaxBytes = 1 << 30
	// The checkout bounds of a Limits that names none: config
	// workspace.checkout's defaults.
	DefaultCheckoutMaxDocuments = 500
	DefaultCheckoutMaxBytes     = 256 << 20
)

// Manager is the core's workspace bookkeeping over one dispatcher.
type Manager struct {
	disp   *dispatch.Dispatcher
	store  Store
	limits Limits
	log    *slog.Logger
	now    func() time.Time
	// quarantine is the files that crashed a workspace's conversion child.
	quarantine quarantine
	// checkouts are the records of File op=checkout (checkouts.go).
	checkouts checkouts
	// audit records shares and uses through them (SetAudit); nil records
	// nothing.
	audit audit.Sink
	// expose is workspace.expose (SetExpose): off until set.
	expose ExposeLimits
	// durable is workspace.durable (SetDurable): off until set.
	durable DurableLimits
	// surfaceClosed tells a run one of its pages went (SetSurfaceNotifier);
	// nil tells no one.
	surfaceClosed SurfaceNotifier
	// proxy and routes are the way out and the operator's routes by name
	// (SetEgress); a nil proxy refuses every request out.
	proxy  *egress.Proxy
	routes map[string]egress.Route
	// gitProxy sends git workspaces' requests (SetGitProxy); nil refuses
	// every one. gitTickets are the live tickets of git ops (git.go).
	gitProxy   *egress.GitProxy
	gitTickets gitTickets
	// principalEgress is workspace.mcp.egress (SetPrincipalEgress): the
	// routes a workspace opened over the MCP server may use.
	principalEgress []string
	// tenantRoutes reads the tenants' own routes (SetTenantRoutes); nil
	// when tenant routes are off. tenantHosts is
	// workspace.egress.tenant_hosts.
	tenantRoutes TenantRouteStore
	tenantHosts  []string
	// tenantTestPort is the one port other than 443 a tenant route may
	// name: set only by this package's tests, whose upstreams cannot listen
	// on 443.
	tenantTestPort string
	// templateDir is where template archives are kept (SetTemplateDir);
	// empty keeps none.
	templateDir string
}

// New builds a manager. A zero MaxPerRun is 1: a run that may open
// workspaces may open at least one.
func New(d *dispatch.Dispatcher, st Store, l Limits, log *slog.Logger) *Manager {
	if l.MaxPerRun <= 0 {
		l.MaxPerRun = 1
	}
	if l.CopyMaxFiles <= 0 {
		l.CopyMaxFiles = DefaultCopyMaxFiles
	}
	if l.CopyMaxBytes <= 0 {
		l.CopyMaxBytes = DefaultCopyMaxBytes
	}
	if l.CheckoutMaxDocuments <= 0 {
		l.CheckoutMaxDocuments = DefaultCheckoutMaxDocuments
	}
	if l.CheckoutMaxBytes <= 0 {
		l.CheckoutMaxBytes = DefaultCheckoutMaxBytes
	}
	if l.ConvertQuarantine <= 0 {
		l.ConvertQuarantine = DefaultConvertQuarantine
	}
	if log == nil {
		log = slog.Default()
	}
	return &Manager{disp: d, store: st, limits: l, log: log.With("component", "workspaces"), now: time.Now}
}

// Owner is who is asking: who a workspace is for and who may use it. A run
// (the usual), or the principal of a session of loomcycle's MCP server
// (PrincipalOwner, RFC GW D35).
type Owner struct {
	TenantID, UserID string
	RunID            string // the pin: the run's id, or a principal's pin key
	AgentID          string
	RootRunID        string
	// Kind is store.WorkspaceOwnerPrincipal for a principal; empty (or
	// store.WorkspaceOwnerRun) for a run.
	Kind string
	// TokenDefID is the token definition a principal signed in with: the
	// workspaces it opens are released when that token ends.
	TokenDefID string
}

// IsPrincipal reports whether the owner is an MCP session's principal.
func (o Owner) IsPrincipal() bool { return o.Kind == store.WorkspaceOwnerPrincipal }

// principalPinPrefix starts a principal's pin key. A run id starts "run_",
// so the two never meet in owner_run_id.
const principalPinPrefix = "principal:"

// PrincipalOwner is the owner of the workspaces a session of loomcycle's MCP
// server opens (RFC GW D35): the token identity, its tenant and subject, and
// no run. The pin key is a hash of the two, so every session and token of
// one subject reaches the same workspaces and no other subject's. tokenDefID
// is the token the session signed in with. Both tenant and subject are
// required: an empty one returns an owner with no pin, which every lookup
// refuses.
func PrincipalOwner(tenantID, subject, tokenDefID string) Owner {
	if tenantID == "" || subject == "" {
		return Owner{Kind: store.WorkspaceOwnerPrincipal}
	}
	sum := sha256.Sum256([]byte(tenantID + "\x00" + subject))
	return Owner{TenantID: tenantID, UserID: subject, RunID: principalPinPrefix + hex.EncodeToString(sum[:]),
		Kind: store.WorkspaceOwnerPrincipal, TokenDefID: tokenDefID}
}

// kind is the owner kind a row of o is written with.
func (o Owner) kind() string {
	if o.IsPrincipal() {
		return store.WorkspaceOwnerPrincipal
	}
	return store.WorkspaceOwnerRun
}

// The errors the manager answers with; the tool turns each into a class.
var (
	// ErrNotFound: no workspace of this name belongs to the run. Also the
	// answer for one that belongs to another run, so a run cannot learn
	// another's workspaces exist.
	ErrNotFound = errors.New("no workspace of that name belongs to this run")
	// ErrLimit: a max_per_run or max_per_tenant bound.
	ErrLimit = errors.New("workspace limit reached")
	// ErrInvalidName: a name the tool may not use.
	ErrInvalidName = errors.New("invalid workspace name")
	// ErrNotReady: the workspace is not ready (opening, releasing or lost).
	ErrNotReady = errors.New("the workspace is not ready")
)

// MaxNameLen bounds a workspace's name.
const MaxNameLen = 64

// ValidName reports whether name may name a workspace: 1 to MaxNameLen ASCII
// letters, digits, '.', '-' and '_', starting with a letter or a digit. A
// name is the agent's label and never reaches a path (the worker's
// directory is the id), but it is printed back to the model and to logs, so
// it is kept plain.
func ValidName(name string) bool {
	if name == "" || len(name) > MaxNameLen {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		alnum := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
		if !alnum && (i == 0 || (c != '.' && c != '-' && c != '_')) {
			return false
		}
	}
	return true
}

// newID mints a workspace id: what the worker's directory is named.
func newID() (string, error) {
	var b [12]byte
	if _, err := io.ReadFull(rand.Reader, b[:]); err != nil {
		return "", err
	}
	return "ws_" + hex.EncodeToString(b[:]), nil
}

// OpenRequest is what an open asks for.
type OpenRequest struct {
	Name string
	// Toolchains the worker must carry, all of them.
	Toolchains []string
	// Engine is the least isolating engine accepted; "" accepts any.
	Engine string
	// Egress are the routes the workspace may send requests out through,
	// recorded on its row (RFC GW §6.3). The caller has checked them
	// against the run's grant; none is no way out.
	Egress []string
	// Template, when set, is the template the workspace starts as a copy
	// of (RFC GW §5.5), found by FindTemplate. Toolchains are then its.
	Template *store.WorkspaceTemplateRow
	// Git, when set, opens a git workspace (RFC GW §4.2, P11): a working
	// tree over the tenant's mirror of the repository, on a worker with the
	// git label. The caller has run the run's checks (git_remote, the host
	// list, the access-control rules) and resolved the credential.
	Git *GitOpen
	// Lifetime is "" (ephemeral) or store.WorkspaceLifetimeDurable.
	Lifetime string
}

// Open opens a workspace named req.Name for the run. A ready workspace of
// that name the run already holds is returned as it is, so a repeated open
// is harmless. Otherwise it is placed on a worker, its row written as
// opening, the directory made, and the row moved to ready; on any failure
// what was made is removed and the row deleted.
func (m *Manager) Open(ctx context.Context, o Owner, req OpenRequest) (store.WorkspaceRow, error) {
	if !ValidName(req.Name) {
		return store.WorkspaceRow{}, fmt.Errorf("%w: 1 to %d letters, digits, '.', '-' or '_', starting with a letter or digit", ErrInvalidName, MaxNameLen)
	}
	switch req.Lifetime {
	case "", store.WorkspaceLifetimeEphemeral:
	case store.WorkspaceLifetimeDurable:
		return m.openDurable(ctx, o, req)
	default:
		return store.WorkspaceRow{}, fmt.Errorf("%w: lifetime %q (want ephemeral or durable)", ErrInvalidName, req.Lifetime)
	}
	if r, err := m.Get(ctx, o, req.Name); err == nil {
		// A repeat is harmless only when it asks for what the run holds: a
		// git open never gets a general workspace, or another repository's.
		if (req.Git != nil) != (r.Git != nil) || req.Git != nil && req.Git.URL != r.Git.URL {
			return store.WorkspaceRow{}, fmt.Errorf("%w: %q is another kind of workspace, or on another repository", ErrNameTaken, req.Name)
		}
		if r.State == store.WorkspaceReady {
			return r, nil
		}
		return store.WorkspaceRow{}, fmt.Errorf("%w: %q is %s", ErrNotReady, req.Name, r.State)
	} else if !errors.Is(err, ErrNotFound) {
		return store.WorkspaceRow{}, err
	}
	return m.openPlaced(ctx, o, req, store.WorkspaceLifetimeEphemeral)
}

// openPlaced places and makes a new workspace of lifetime for the run.
func (m *Manager) openPlaced(ctx context.Context, o Owner, req OpenRequest, lifetime string) (store.WorkspaceRow, error) {
	req.Lifetime = lifetime
	if req.Template != nil {
		if req.Template.TenantID != o.TenantID {
			return store.WorkspaceRow{}, ErrTemplateNotFound
		}
		// A template is refused on a worker without its toolchains
		// (RFC GW §5.5).
		req.Toolchains = req.Template.Toolchains
	}
	place := dispatch.PlaceRequest{TenantID: o.TenantID, UserID: o.UserID, Toolchains: req.Toolchains, Engine: req.Engine}
	if req.Git != nil {
		if req.Template != nil {
			return store.WorkspaceRow{}, fmt.Errorf("%w: a git workspace does not start from a template", ErrInvalidGitOpen)
		}
		if err := req.Git.check(); err != nil {
			return store.WorkspaceRow{}, err
		}
		place.Labels = []string{workspace.GitLabel}
	}
	cands, err := m.disp.Place(ctx, place)
	if err != nil {
		return store.WorkspaceRow{}, err
	}
	engines := map[string]string{}
	for _, w := range m.disp.Workers() {
		engines[w.ID] = w.Engine
	}
	var lastErr error
	for _, workerID := range cands {
		row, err := m.openOn(ctx, o, req, workerID, engines[workerID])
		if err == nil {
			return row, nil
		}
		if dispatch.ClassOf(err) != workspace.ClassAtCapacity {
			return store.WorkspaceRow{}, err
		}
		lastErr = err
	}
	return store.WorkspaceRow{}, lastErr
}

// openOn opens the workspace on one worker. The bounds are checked after the
// row is written, counting it: two opens racing on two replicas each see the
// other's row, so neither passes a bound both together would break (both may
// be refused, never both admitted).
func (m *Manager) openOn(ctx context.Context, o Owner, req OpenRequest, workerID, engine string) (store.WorkspaceRow, error) {
	if req.Git != nil {
		return m.makeOn(ctx, o, req.Name, req.Lifetime, req.Egress, workerID, engine, req.Git.row(o.TenantID), func(row *store.WorkspaceRow) error {
			return m.openGit(ctx, row, *req.Git)
		})
	}
	return m.makeOn(ctx, o, req.Name, req.Lifetime, req.Egress, workerID, engine, nil, func(row *store.WorkspaceRow) error {
		if req.Template != nil {
			return m.openFromTemplate(ctx, o, *req.Template, workerID, row.ID)
		}
		return m.disp.OpenWith(ctx, o.TenantID, workerID, row.ID, workspace.OpenArgs{DiskQuotaBytes: m.limits.DiskQuotaBytes})
	})
}

// makeOn makes a workspace on one worker: its row (with the egress routes it
// may use, RFC GW §6.3), the bounds, then place, which makes its directory
// there (an open, or a fork), then the row ready. git, when set, makes a git
// workspace's row; place may fill in what it learns of the repository.
func (m *Manager) makeOn(ctx context.Context, o Owner, name, lifetime string, egress []string, workerID, engine string, git *store.WorkspaceGit, place func(row *store.WorkspaceRow) error) (store.WorkspaceRow, error) {
	id, err := newID()
	if err != nil {
		return store.WorkspaceRow{}, fmt.Errorf("workspace id: %w", err)
	}
	seed := store.WorkspaceRow{
		ID: id, TenantID: o.TenantID, UserID: o.UserID, Name: name, WorkerID: workerID, Engine: engine, Lifetime: lifetime,
		OwnerRunID: o.RunID, OwnerAgentID: o.AgentID, RootRunID: o.RootRunID, State: store.WorkspaceOpening,
		OwnerKind: o.kind(), OpenedToken: o.TokenDefID,
		Egress: append([]string{}, egress...),
	}
	if git != nil {
		g := *git
		seed.Kind, seed.Git = store.WorkspaceKindGit, &g
	}
	row, err := m.store.WorkspaceCreate(ctx, seed)
	if err != nil {
		var conflict *store.ErrConflict
		if errors.As(err, &conflict) {
			return store.WorkspaceRow{}, fmt.Errorf("%w: %q is being opened by another call", ErrNotReady, name)
		}
		return store.WorkspaceRow{}, err
	}
	undo := func() { m.deleteRow(row) }
	if err := m.checkLimits(ctx, o); err != nil {
		undo()
		return store.WorkspaceRow{}, err
	}
	if lifetime == store.WorkspaceLifetimeDurable {
		// Counted after the insert, as the run's bound is: two creates
		// racing on two replicas cannot both pass it.
		if n, err := m.store.WorkspaceCountDurableByUser(ctx, o.TenantID, o.UserID); err != nil || (m.durable.MaxPerUser > 0 && n > m.durable.MaxPerUser) {
			undo()
			if err == nil {
				err = fmt.Errorf("%w: a user keeps at most %d durable workspaces (workspace.durable.max_per_user); discard one first", ErrLimit, m.durable.MaxPerUser)
			}
			return store.WorkspaceRow{}, err
		}
	}
	if err := place(&row); err != nil {
		// The directory may have been made before the error (a worker lost
		// mid-op): remove it if the worker is still here; if not, its next
		// attach finds a directory no row knows and removes it.
		if dispatch.ClassOf(err) != workspace.ClassAtCapacity {
			m.removeQuietly(row)
		}
		undo()
		return store.WorkspaceRow{}, err
	}
	ok, err := m.store.WorkspaceSetState(ctx, o.TenantID, id, store.WorkspaceOpening, store.WorkspaceReady)
	if err != nil || !ok {
		m.removeQuietly(row)
		undo()
		if err == nil {
			err = fmt.Errorf("%w: the workspace was released while it opened", ErrNotReady)
		}
		return store.WorkspaceRow{}, err
	}
	row.State = store.WorkspaceReady
	return row, nil
}

func (m *Manager) checkLimits(ctx context.Context, o Owner) error {
	rows, err := m.store.WorkspaceListByOwnerRun(ctx, o.TenantID, o.RunID)
	if err != nil {
		return err
	}
	active := 0
	for _, r := range rows {
		if r.State == store.WorkspaceOpening || r.State == store.WorkspaceReady {
			active++
		}
	}
	if active > m.limits.MaxPerRun {
		return fmt.Errorf("%w: a run holds at most %d workspaces (workspace.max_per_run); release one first", ErrLimit, m.limits.MaxPerRun)
	}
	if m.limits.MaxPerTenant > 0 {
		n, err := m.store.WorkspaceCountActiveByTenant(ctx, o.TenantID)
		if err != nil {
			return err
		}
		if n > m.limits.MaxPerTenant {
			return fmt.Errorf("%w: the tenant holds its %d workspaces (workspace.max_per_tenant)", ErrLimit, m.limits.MaxPerTenant)
		}
	}
	return nil
}

// Get returns the run's workspace of this name. The lookup is keyed on the
// tenant and the owner run, so it is the pin: another run's workspace of the
// same name is not found.
func (m *Manager) Get(ctx context.Context, o Owner, name string) (store.WorkspaceRow, error) {
	if o.RunID == "" || !ValidName(name) {
		return store.WorkspaceRow{}, ErrNotFound
	}
	r, err := m.store.WorkspaceGetByName(ctx, o.TenantID, o.RunID, name)
	var nf *store.ErrNotFound
	if errors.As(err, &nf) {
		return store.WorkspaceRow{}, ErrNotFound
	}
	if err != nil {
		return store.WorkspaceRow{}, err
	}
	// The query asks for both already; this keeps a store that ever got it
	// wrong from widening the pin. A run never reaches a principal's row,
	// nor a principal a run's, whatever their keys.
	if r.OwnerRunID != o.RunID || r.TenantID != o.TenantID || r.OwnerKind != o.kind() {
		return store.WorkspaceRow{}, ErrNotFound
	}
	if o.IsPrincipal() {
		// The idle release counts from here (D-P9-1). A failed stamp is
		// logged, not fatal: at worst the workspace is released early.
		now := m.now().UTC()
		if err := m.store.WorkspaceTouchUsed(ctx, r.TenantID, r.ID, now); err != nil {
			m.log.Warn("cannot record a workspace's use", "workspace_id", r.ID, "err", err)
		} else {
			r.LastUsedAt = now
		}
	}
	return r, nil
}

// List returns the run's own workspaces.
func (m *Manager) List(ctx context.Context, o Owner) ([]store.WorkspaceRow, error) {
	if o.RunID == "" {
		return []store.WorkspaceRow{}, nil
	}
	rows, err := m.store.WorkspaceListByOwnerRun(ctx, o.TenantID, o.RunID)
	if err != nil {
		return nil, err
	}
	out := rows[:0]
	for _, r := range rows {
		if r.OwnerKind == o.kind() {
			out = append(out, r)
		}
	}
	return out, nil
}

// Do runs one op in a ready workspace of the run.
func (m *Manager) Do(ctx context.Context, r store.WorkspaceRow, req dispatch.Request) (dispatch.Result, error) {
	if r.State != store.WorkspaceReady {
		return dispatch.Result{}, fmt.Errorf("%w: %q is %s", ErrNotReady, r.Name, r.State)
	}
	req.TenantID, req.WorkspaceID = r.TenantID, r.ID
	return m.disp.Do(ctx, r.WorkerID, req)
}

// Status reports whether the workspace's directory is on its worker.
func (m *Manager) Status(ctx context.Context, r store.WorkspaceRow) (bool, error) {
	return m.disp.Status(ctx, r.TenantID, r.WorkerID, r.ID)
}

// Release ends a workspace's pin: a durable one is parked, its files kept
// (RFC GW §4.2); any other is removed. The run-end release and the sweeper
// come here, so neither removes a durable workspace.
func (m *Manager) Release(ctx context.Context, r store.WorkspaceRow) error {
	if r.Lifetime == store.WorkspaceLifetimeDurable && r.State != store.WorkspaceReleasing {
		return m.unpin(ctx, r)
	}
	return m.remove(ctx, r)
}

// remove removes a workspace: its directory from the worker and then its
// row. A worker that is not attached here, or that fails the remove, leaves
// the row releasing, and the worker's next attach removes the directory and
// the row (dispatch's reconciliation).
func (m *Manager) remove(ctx context.Context, r store.WorkspaceRow) error {
	// Its checkouts go with it, whatever the release's outcome: no commit
	// can be asked of a workspace being released.
	m.checkouts.forget(r.ID)
	// Its exposures close first: the route already refuses a workspace that
	// is not ready, and the rows go with the workspace's in one transaction
	// at the end; dropping them now closes them on every replica at once,
	// even when the worker is away and the row stays releasing.
	m.dropExposures(ctx, r)
	// So are the proxy's counts for it: an id is never reused.
	m.forgetEgress(r.ID)
	if r.State != store.WorkspaceReleasing {
		ok, err := m.store.WorkspaceSetState(ctx, r.TenantID, r.ID, r.State, store.WorkspaceReleasing)
		if err != nil {
			return err
		}
		if !ok {
			// Another caller moved it first (a sweep, a concurrent release);
			// read it again and let that path finish.
			cur, err := m.store.WorkspaceGet(ctx, r.TenantID, r.ID)
			var nf *store.ErrNotFound
			if errors.As(err, &nf) {
				return nil
			}
			if err != nil {
				return err
			}
			if cur.State != store.WorkspaceReleasing {
				return fmt.Errorf("workspace %s moved to %s during its release", r.ID, cur.State)
			}
		}
	}
	if !m.disp.Attached(r.WorkerID, r.TenantID) {
		return nil
	}
	if err := m.disp.Remove(ctx, r.TenantID, r.WorkerID, r.ID); err != nil {
		m.log.Warn("cannot remove a released workspace; its worker's next attach removes it", "workspace_id", r.ID, "err", err)
		return nil
	}
	_, err := m.store.WorkspaceDelete(ctx, r.TenantID, r.ID)
	return err
}

// ReleaseRun releases every workspace runID holds, unless the run is paused
// or pausing (parked to be resumed, here or elsewhere, into its
// workspaces). Called when a run ends, keyed on the run's own id.
func (m *Manager) ReleaseRun(ctx context.Context, runID string) error {
	if runID == "" {
		return nil
	}
	run, err := m.store.GetRun(ctx, runID)
	var nf *store.ErrNotFound
	switch {
	case errors.As(err, &nf):
		return nil
	case err != nil:
		return err
	case run.PauseState == store.PauseStatePaused || run.PauseState == store.PauseStatePausing:
		return nil
	}
	rows, err := m.store.WorkspaceListByOwnerRun(ctx, run.TenantID, runID)
	if err != nil {
		return err
	}
	var errs []error
	for _, r := range rows {
		errs = append(errs, m.Release(ctx, r))
	}
	return errors.Join(errs...)
}

// Sweep releases the workspaces of runs that ended without the run-end
// release (a core killed mid-run, a release that failed), and of runs whose
// row is gone. It returns how many it released.
func (m *Manager) Sweep(ctx context.Context) (int, error) {
	rows, err := m.store.WorkspaceSweepCandidates(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, r := range rows {
		if r.State == store.WorkspaceReleasing && !m.disp.Attached(r.WorkerID, r.TenantID) {
			continue // the worker's next attach finishes it
		}
		if err := m.Release(ctx, r); err != nil {
			m.log.Warn("sweep: cannot release a workspace", "workspace_id", r.ID, "err", err)
			continue
		}
		n++
	}
	return n, nil
}

// minLostAfter keeps the lost bound well above how often an attached
// worker's rows are touched (dispatch.DefaultRecheckEvery, 60 s): a smaller
// lost_after_s would mark a live worker's workspaces lost between touches.
const minLostAfter = 3 * dispatch.DefaultRecheckEvery

// MarkLost marks lost the active workspaces whose worker no replica has seen
// for LostAfter: their rows were not touched since. A worker attached to
// this replica is skipped whatever its rows say (its next touch is due). A
// lost workspace whose worker comes back is put back in service at attach if
// its run still lives.
func (m *Manager) MarkLost(ctx context.Context) (int, error) {
	after := max(m.limits.LostAfter, minLostAfter)
	rows, err := m.store.WorkspaceListStale(ctx, m.now().Add(-after))
	if err != nil {
		return 0, err
	}
	n := 0
	for _, r := range rows {
		if m.disp.Attached(r.WorkerID, r.TenantID) {
			continue
		}
		ok, err := m.store.WorkspaceSetState(ctx, r.TenantID, r.ID, r.State, store.WorkspaceLost)
		if err != nil {
			m.log.Warn("cannot mark a workspace lost", "workspace_id", r.ID, "err", err)
			continue
		}
		if ok {
			n++
		}
	}
	if n > 0 {
		m.log.Warn("workspaces marked lost: their worker has not been seen", "count", n, "after", after)
	}
	return n, nil
}

func (m *Manager) removeQuietly(r store.WorkspaceRow) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := m.disp.Remove(ctx, r.TenantID, r.WorkerID, r.ID); err != nil {
		m.log.Warn("cannot remove a workspace that failed to open; its worker's next attach removes it", "workspace_id", r.ID, "err", err)
	}
}

func (m *Manager) deleteRow(r store.WorkspaceRow) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := m.store.WorkspaceDelete(ctx, r.TenantID, r.ID); err != nil {
		m.log.Warn("cannot delete a workspace's row", "workspace_id", r.ID, "err", err)
	}
}
