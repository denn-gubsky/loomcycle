// Package dispatch is the core's side of the workspace workers (RFC GW §8):
// the WorkspaceService workers attach to on the worker endpoint, the registry
// of attached workers, each worker's pool of idle Op streams, placement, and
// Do, which runs one op on a worker.
//
// A worker authenticates with an operator token holding only workers:attach
// (auth.WorkerAttachment). Its id here is what its token grants (its tenant,
// or "shared") joined to the id it declares, so two tenants' workers never
// share an id, a rotated token's worker keeps its id, and an Op stream is
// accepted only when its own token grants that same id. The store's
// worker-keyed queries are not tenant-scoped; this keying is what makes them
// safe to use.
//
// The worker the runtime starts as its own child (workspace.mode local)
// attaches on a private Unix socket with a per-boot token instead
// (RegisterLocal); its id is "local:<tenant>/<id>".
package dispatch

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/safego"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/workerlink"
	"github.com/denn-gubsky/loomcycle/internal/workspace"
	"github.com/denn-gubsky/loomcycle/internal/workspace/workspacepb"
)

// Defaults for Options left zero, and bounds on what a worker declares.
const (
	DefaultHeartbeatTimeout = 15 * time.Second
	DefaultStreamWait       = 30 * time.Second
	DefaultMaxOutput        = 8 << 20
	// DefaultRecheckEvery is how often an attached worker's bearer is
	// resolved and decided again, so a revoked, rotated or re-bound token
	// stops serving within it rather than when the connection next drops.
	DefaultRecheckEvery = 60 * time.Second
	// maxDeclaredID bounds the id a worker declares.
	maxDeclaredID = 128
	// maxCapacity bounds the workspaces a worker may say it holds at most.
	maxCapacity = 10000
	// maxHeld bounds the held_workspaces list of a Hello.
	maxHeld = 10000
	// maxListed bounds the toolchains and labels of a Hello.
	maxListed = 64
	// lowWater is the idle streams under which the core asks a worker for
	// more; refill is how many it asks for at once.
	lowWater = 2
	refill   = 4
	// maxFreeRetries bounds the retries of an op whose stream went away
	// before anything of the op reached the worker.
	maxFreeRetries = 3
	// maxDetail bounds a worker's error_detail.
	maxDetail = 1024
)

// Resolver resolves a bearer to the principal it stands for, as the API's
// transports do; (_, false) for an unknown, expired or retired token.
type Resolver func(ctx context.Context, bearer string) (auth.Principal, bool)

// Store is what the dispatcher reads and writes of the workspaces table.
type Store interface {
	WorkspaceListByWorker(ctx context.Context, workerID string, states ...store.WorkspaceState) ([]store.WorkspaceRow, error)
	WorkspaceDelete(ctx context.Context, tenantID, id string) (bool, error)
	WorkspaceCountActiveByWorker(ctx context.Context, workerID string) (int, error)
	WorkspaceSetState(ctx context.Context, tenantID, id string, from, to store.WorkspaceState) (bool, error)
	// GetRun reads a lost workspace's owner run at reconciliation.
	GetRun(ctx context.Context, runID string) (store.Run, error)
	// WorkspaceTouchByWorker marks a worker's active rows as seen, so the
	// sweeper, on whichever replica, does not mark them lost.
	WorkspaceTouchByWorker(ctx context.Context, workerID string) (int, error)
}

// Options configure a Dispatcher.
type Options struct {
	// BuildVersion is this binary's version; a worker of another build is
	// admitted with a warning.
	BuildVersion string
	// HeartbeatTimeout drops a worker that has sent nothing for this long.
	HeartbeatTimeout time.Duration
	// StreamWait is the longest an op waits for one of its worker's idle
	// streams.
	StreamWait time.Duration
	// MaxOutput bounds the output of an op that names no bound of its own.
	MaxOutput int64
	// RecheckEvery is how often an attached worker's token is checked
	// again; zero is DefaultRecheckEvery. A test seam, not a setting.
	RecheckEvery time.Duration
	// LocalToken is the per-boot token of the worker the runtime starts as
	// its own child (workspace.mode local). It is accepted only on a stream
	// a RegisterLocal server serves, the runtime's private Unix socket, and
	// refused everywhere else; empty when there is no local worker.
	LocalToken string
	// LocalTenant is the one tenant the local worker serves
	// (workspace.local.tenant).
	LocalTenant string
	Logger      *slog.Logger
}

// Dispatcher is the workspace worker service and the registry behind it.
type Dispatcher struct {
	workspacepb.UnimplementedWorkspaceServiceServer

	opt Options
	log *slog.Logger
	// deps are the store and the token resolver, set by Activate.
	deps atomic.Pointer[deps]

	mu      sync.Mutex
	srv     *grpc.Server
	workers map[string]*worker
	ops     map[string]uint64
	deaths  uint64
	// outBytes counts the output bytes received, by op label.
	outBytes map[string]uint64
}

// worker is one attached worker, as its Hello described it.
type worker struct {
	id       string // the core's id: the token's identity and the declared id
	declared string
	serves   auth.WorkerAttach
	// local is the runtime's own child, attached on its private socket.
	local    bool
	engine   string
	tools    map[string]bool
	labels   []string
	capacity int
	// ctl queues control messages for the Attach stream's one sender.
	ctl chan *workspacepb.CoreControl
	// lost is closed when the worker detaches.
	lost chan struct{}
	// gone is the error an op in flight fails with once lost is closed:
	// ErrWorkerLost, or ErrWorkerRevoked when its token stopped holding.
	// Set under Dispatcher.mu before lost closes.
	gone *OpError

	// Under Dispatcher.mu.
	idle      []*opStream // oldest first
	waiters   []chan *opStream
	inFlight  int
	diedMidOp bool // an op's stream broke before its answer
	requested int  // streams asked for with OpenStreams, not yet arrived
	// pending are the releasing rows a Remove named, awaiting Removed.
	pending map[string]store.WorkspaceRow
	// streams are every Op stream of the worker, idle or busy, so a detach
	// can end them all.
	streams map[*opStream]struct{}
}

// opStream is one Op stream: idle until an op is handed to it.
type opStream struct {
	w *worker
	// ops is buffered 1, so handing an op never blocks. nil tells a
	// handler whose stream already ended that it was given back unused.
	ops    chan *op
	taken  bool
	ctx    context.Context
	cancel context.CancelFunc
}

type deps struct {
	store   Store
	resolve Resolver
}

// New builds a dispatcher. It answers every worker Unavailable until
// Activate.
func New(opt Options) *Dispatcher {
	if opt.RecheckEvery <= 0 {
		opt.RecheckEvery = DefaultRecheckEvery
	}
	if opt.HeartbeatTimeout <= 0 {
		opt.HeartbeatTimeout = DefaultHeartbeatTimeout
	}
	if opt.StreamWait <= 0 {
		opt.StreamWait = DefaultStreamWait
	}
	if opt.MaxOutput <= 0 {
		opt.MaxOutput = DefaultMaxOutput
	}
	log := opt.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Dispatcher{opt: opt, log: log.With("component", "workspace-dispatcher"),
		workers: map[string]*worker{}, ops: map[string]uint64{}}
}

// Activate gives the dispatcher the store and the way a worker's bearer is
// resolved. The runtime builds the worker endpoint before the store and the
// server that resolves tokens exist, so it activates the dispatcher once they
// do; until then an attach is answered Unavailable and the worker tries
// again.
func (d *Dispatcher) Activate(st Store, r Resolver) { d.deps.Store(&deps{store: st, resolve: r}) }

// store is the activated store; nil before Activate.
func (d *Dispatcher) store() Store {
	if dp := d.deps.Load(); dp != nil {
		return dp.store
	}
	return nil
}

// Register adds the workspace service to srv, the worker endpoint's server.
func (d *Dispatcher) Register(srv grpc.ServiceRegistrar) {
	workspacepb.RegisterWorkspaceServiceServer(srv, d)
}

// RegisterLocal adds the workspace service to srv, the server of the
// runtime's private socket, for the local worker alone: every stream it
// serves is authenticated against LocalToken, never the token resolver.
func (d *Dispatcher) RegisterLocal(srv grpc.ServiceRegistrar) {
	workspacepb.RegisterWorkspaceServiceServer(srv, localService{d: d})
}

// localKey marks the context of a stream the private socket serves. Only
// localService sets it, and only RegisterLocal registers localService, so a
// stream from the network endpoint can never carry it.
type localKey struct{}

func isLocal(ctx context.Context) bool { return ctx.Value(localKey{}) != nil }

// localService is the workspace service on the private socket: the
// dispatcher's own, each stream's context marked as local.
type localService struct {
	workspacepb.UnimplementedWorkspaceServiceServer
	d *Dispatcher
}

func (l localService) Attach(s workspacepb.WorkspaceService_AttachServer) error {
	return l.d.Attach(localAttach{s, context.WithValue(s.Context(), localKey{}, true)})
}

func (l localService) Op(s workspacepb.WorkspaceService_OpServer) error {
	return l.d.Op(localOp{s, context.WithValue(s.Context(), localKey{}, true)})
}

type localAttach struct {
	workspacepb.WorkspaceService_AttachServer
	ctx context.Context
}

func (s localAttach) Context() context.Context { return s.ctx }

type localOp struct {
	workspacepb.WorkspaceService_OpServer
	ctx context.Context
}

func (s localOp) Context() context.Context { return s.ctx }

// Serve serves the workspace service alone on l, on a server of its own
// built as the runtime builds the endpoint's, until Stop: for tests and
// harnesses.
func (d *Dispatcher) Serve(l net.Listener) error {
	d.mu.Lock()
	if d.srv == nil {
		d.srv = grpc.NewServer(workerlink.ServerOptions()...)
		d.Register(d.srv)
	}
	srv := d.srv
	d.mu.Unlock()
	return srv.Serve(l)
}

// Stop ends every stream and the listener of a Serve.
func (d *Dispatcher) Stop() {
	d.mu.Lock()
	srv := d.srv
	d.mu.Unlock()
	if srv != nil {
		srv.Stop()
	}
}

// authenticate resolves the stream's bearer and decides whether it may
// attach a workspace worker. owner is what the token grants (grantKey), the
// prefix of every id its workers get.
func (d *Dispatcher) authenticate(ctx context.Context) (owner string, att auth.WorkerAttach, bearer string, err error) {
	md, _ := metadata.FromIncomingContext(ctx)
	vals := md.Get(workspace.MDAuthorization)
	const pfx = "Bearer "
	if len(vals) != 1 || len(vals[0]) <= len(pfx) || !strings.EqualFold(vals[0][:len(pfx)], pfx) {
		return "", att, "", status.Error(codes.Unauthenticated, "missing bearer token")
	}
	bearer = vals[0][len(pfx):]
	owner, att, err = d.authorize(ctx, bearer)
	return owner, att, bearer, err
}

// authorize resolves bearer and decides whether it may attach a workspace
// worker: at attach, on every Op stream, and again every RecheckEvery while
// a worker stays attached.
func (d *Dispatcher) authorize(ctx context.Context, bearer string) (owner string, att auth.WorkerAttach, err error) {
	dp := d.deps.Load()
	if dp == nil {
		return "", att, status.Error(codes.Unavailable, "the runtime is starting")
	}
	// The local worker's token is compared, in constant time, only on the
	// private socket, and the socket takes no other: a token that reached
	// the network could never attach there, and a network token, whatever
	// tenant it grants, could never take the local worker's id ("local:..."
	// is no grantKey) or its Op streams.
	local := d.opt.LocalToken != "" && auth.CompareBearer(bearer, d.opt.LocalToken)
	if isLocal(ctx) {
		if !local {
			return "", att, status.Error(codes.PermissionDenied, "the runtime's private worker socket accepts only its local worker's token")
		}
		att = auth.WorkerAttach{TenantID: d.opt.LocalTenant}
		return "local:" + d.opt.LocalTenant, att, nil
	}
	if local {
		return "", att, status.Error(codes.PermissionDenied, "the local worker's token is accepted only on the runtime's private socket")
	}
	p, ok := dp.resolve(ctx, bearer)
	if !ok {
		// The resolver answers an unknown token and a store it could not
		// read alike, so neither is final: Unavailable, which the worker
		// retries with its backoff. A token that never resolves is refused
		// at every attach and so never served; one that recovers attaches
		// afresh. Only a principal that did resolve is refused for good.
		return "", att, status.Error(codes.Unavailable, "the runtime could not verify this worker's token")
	}
	att, err = auth.WorkerAttachment(p)
	if err != nil {
		return "", att, status.Error(codes.PermissionDenied, err.Error())
	}
	return grantKey(att), att, nil
}

// stillAuthorized decides an attached worker's bearer again. It must still
// resolve, still be admitted by auth.WorkerAttachment, and still grant what
// it granted at attach.
func (d *Dispatcher) stillAuthorized(ctx context.Context, bearer, owner string) error {
	o, _, err := d.authorize(ctx, bearer)
	switch {
	case err != nil:
		return err
	case o != owner:
		return status.Error(codes.PermissionDenied, "the worker's token now serves other tenants")
	}
	return nil
}

// grantKey is what a worker token grants, the prefix of its workers' ids:
// "tenant:<id>" for a tenant-bound worker, "shared" for a shared one. It
// names the grant, not the token, so a rotated token's worker attaches under
// the id it had and its workspaces' rows still name it; the tenant in it
// keeps two tenants' workers from ever sharing an id.
func grantKey(att auth.WorkerAttach) string {
	if att.Shared {
		return "shared"
	}
	return "tenant:" + att.TenantID
}

// validDeclaredID reports whether a worker's own id is 1 to maxDeclaredID
// bytes of letters, digits, '.', '-' and '_'.
func validDeclaredID(id string) bool {
	if id == "" || len(id) > maxDeclaredID {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '.', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}

// ---- Attach ----

// Attach registers a worker for as long as the stream lives and it keeps
// heartbeating, and reconciles what it holds against the store.
func (d *Dispatcher) Attach(stream workspacepb.WorkspaceService_AttachServer) error {
	ctx := stream.Context()
	owner, att, bearer, err := d.authenticate(ctx)
	if err != nil {
		return err
	}
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	w, held, err := d.admit(owner, att, first.GetHello())
	if err != nil {
		return err
	}
	w.local = isLocal(ctx)
	d.mu.Lock()
	if _, dup := d.workers[w.id]; dup {
		d.mu.Unlock()
		return status.Errorf(codes.AlreadyExists, "a worker named %q is already attached", w.declared)
	}
	d.workers[w.id] = w
	d.mu.Unlock()
	log := d.log.With("worker_id", w.id)
	log.Info("workspace worker attached", "engine", w.engine, "capacity", w.capacity, "shared", att.Shared, "tenant", att.TenantID)
	died := false
	gone := ErrWorkerLost
	defer func() { d.detach(w, died, gone) }()

	if err := stream.Send(&workspacepb.CoreControl{Msg: &workspacepb.CoreControl_Welcome{Welcome: &workspacepb.Welcome{BuildVersion: d.opt.BuildVersion}}}); err != nil {
		return err
	}
	d.touch(ctx, w, log)
	if rm := d.reconcile(ctx, w, held, log); len(rm) > 0 {
		if err := stream.Send(&workspacepb.CoreControl{Msg: &workspacepb.CoreControl_Remove{Remove: &workspacepb.Remove{WorkspaceIds: rm}}}); err != nil {
			return err
		}
	}

	in := make(chan *workspacepb.WorkerMessage)
	errc := make(chan error, 1)
	safego.Go(ctx, "workspace.dispatch.attach", func() {
		for {
			m, err := stream.Recv()
			if err != nil {
				errc <- err
				return
			}
			select {
			case in <- m:
			case <-ctx.Done():
				return
			}
		}
	})
	t := time.NewTimer(d.opt.HeartbeatTimeout)
	defer t.Stop()
	recheck := time.NewTicker(d.opt.RecheckEvery)
	defer recheck.Stop()
	for {
		select {
		case <-recheck.C:
			// The bearer is kept only for the life of this stream, never
			// logged. A token that no longer resolves ends the attachment
			// with Unavailable (the worker retries, and attaches only once
			// it resolves again); one that is refused or now serves other
			// tenants ends it with PermissionDenied, which the worker reads
			// as final.
			if err := d.stillAuthorized(ctx, bearer, owner); err != nil {
				log.Warn("workspace worker's token no longer attaches it; detaching", "err", err)
				gone = ErrWorkerRevoked
				return err
			}
			d.touch(ctx, w, log)
		case m := <-in:
			t.Reset(d.opt.HeartbeatTimeout)
			if r := m.GetRemoved(); r != nil {
				d.confirmRemoved(ctx, w, r.GetWorkspaceIds(), log)
			}
		case c := <-w.ctl:
			if err := stream.Send(c); err != nil {
				return err
			}
		case err := <-errc:
			log.Info("workspace worker detached", "err", err)
			return nil
		case <-t.C:
			log.Warn("workspace worker missed its heartbeats; dropping it", "after", d.opt.HeartbeatTimeout)
			died = true
			return status.Error(codes.DeadlineExceeded, "missed heartbeats")
		case <-ctx.Done():
			return nil
		}
	}
}

// admit checks a worker's Hello and builds its registry entry. held are the
// valid workspace ids it reports on its disk.
func (d *Dispatcher) admit(owner string, att auth.WorkerAttach, h *workspacepb.Hello) (*worker, []string, error) {
	if h == nil {
		return nil, nil, status.Error(codes.InvalidArgument, "the first Attach frame must be Hello")
	}
	if h.GetProtocolVersion() != workspace.ProtocolVersion {
		return nil, nil, status.Errorf(codes.FailedPrecondition, "this runtime speaks workspace protocol %d, the worker %d: run the same loomcycle release on both", workspace.ProtocolVersion, h.GetProtocolVersion())
	}
	if !validDeclaredID(h.GetWorkerId()) {
		return nil, nil, status.Error(codes.InvalidArgument, "a worker id of 1 to 128 letters, digits, '.', '-' or '_' is required")
	}
	if workspace.EngineRank(h.GetEngine()) == 0 {
		return nil, nil, status.Errorf(codes.InvalidArgument, "unknown engine %q", h.GetEngine())
	}
	if !engineServes(att, h.GetEngine()) {
		// The principal resolved and is a shared worker's: a definite
		// refusal, which the worker reads as final.
		return nil, nil, status.Error(codes.PermissionDenied, "a shared worker must use an isolating engine; the host engine can serve one tenant only")
	}
	if len(h.GetToolchains()) > maxListed || len(h.GetLabels()) > maxListed || len(h.GetHeldWorkspaces()) > maxHeld {
		return nil, nil, status.Error(codes.InvalidArgument, "the Hello lists too many toolchains, labels or workspaces")
	}
	if b := h.GetBuildVersion(); b != d.opt.BuildVersion {
		d.log.Warn("workspace worker runs another build", "worker", h.GetWorkerId(), "worker_build", b, "build", d.opt.BuildVersion)
	}
	w := &worker{
		id: owner + "/" + h.GetWorkerId(), declared: h.GetWorkerId(), serves: att, engine: h.GetEngine(),
		tools: map[string]bool{}, labels: append([]string(nil), h.GetLabels()...),
		capacity: min(max(int(h.GetCapacity()), 1), maxCapacity),
		ctl:      make(chan *workspacepb.CoreControl, 16), lost: make(chan struct{}),
		pending: map[string]store.WorkspaceRow{}, streams: map[*opStream]struct{}{},
	}
	for _, t := range h.GetToolchains() {
		w.tools[t] = true
	}
	var held []string
	for _, id := range h.GetHeldWorkspaces() {
		// An id the worker could not have been sent is not ours to name in a
		// Remove; the worker keeps it.
		if workspace.ValidID(id) {
			held = append(held, id)
		}
	}
	return w, held, nil
}

// servesTenant reports whether w may be given a workspace or an op of
// tenant. It is the one place the decision is made: placement, Attached and
// Do all ask it.
//
// The empty tenant is what a run carries on an install with no
// authentication (open mode) when its request names none. Only the local
// worker, the runtime's own child on its private socket, configured for the
// empty tenant (workspace.local.tenant resolved to ""), serves it: such an
// install is a single user's, and its local worker is the one meant for it.
// A network worker never serves it, tenant-bound or shared: its token names
// a tenant, and an empty tenant reaching it would mean a run with no
// tenant's identity was handed to a machine some tenant's token attached.
func (w *worker) servesTenant(tenant string) bool {
	if tenant == "" {
		return w.local && !w.serves.Shared && w.serves.TenantID == ""
	}
	return w.serves.Serves(tenant)
}

// engineServes reports whether a worker on engine may hold workspaces for the
// tenants att grants. On the host engine every command runs as the worker's
// own user over the worker's whole disk, so the workspaces of every tenant it
// served would be open to each tenant's commands: a host worker serves one
// tenant only, and a shared worker needs an engine that isolates one
// workspace from another.
func engineServes(att auth.WorkerAttach, engine string) bool {
	return !att.Shared || workspace.EngineRank(engine) > workspace.EngineRank(workspace.EngineHost)
}

// reconcile compares what a worker holds with the store's rows for it and
// returns the ids to remove: those released while it was away (releasing)
// and those no row knows. A releasing row whose directory the worker no
// longer holds is deleted at once; one it holds waits for its Removed. A lost
// row whose directory came back is ready again while its owner run lives,
// and removed like a released one once that run has ended (recoverLost). A
// store that cannot be read removes nothing: a directory is never removed on
// a guess.
func (d *Dispatcher) reconcile(ctx context.Context, w *worker, held []string, log *slog.Logger) []string {
	rows, err := d.store().WorkspaceListByWorker(ctx, w.id)
	if err != nil {
		log.Warn("cannot read the worker's workspaces; reconciling at its next attach", "err", err)
		return nil
	}
	byID := make(map[string]store.WorkspaceRow, len(rows))
	for _, r := range rows {
		byID[r.ID] = r
	}
	onDisk := make(map[string]bool, len(held))
	var remove []string
	for _, id := range held {
		onDisk[id] = true
		r, known := byID[id]
		switch {
		case !known:
			remove = append(remove, id)
		case r.State == store.WorkspaceReleasing:
			d.markPending(w, r)
			remove = append(remove, id)
		case r.State == store.WorkspaceLost:
			if d.recoverLost(ctx, w, r, log) {
				remove = append(remove, id)
			}
		}
	}
	for _, r := range rows {
		if r.State == store.WorkspaceReleasing && !onDisk[r.ID] {
			d.deleteRow(ctx, r, log)
		}
	}
	if len(remove) > 0 {
		log.Info("removing workspaces released or unknown while the worker was away", "count", len(remove))
	}
	return remove
}

// touch records that the worker is attached here, on its active rows: a
// replica sees only its own attachments, so the sweeper's lost check reads
// this mark, which every replica the worker attaches to keeps fresh. It runs
// at attach and with each recheck (RecheckEvery); a fault waits for the next.
func (d *Dispatcher) touch(ctx context.Context, w *worker, log *slog.Logger) {
	if _, err := d.store().WorkspaceTouchByWorker(ctx, w.id); err != nil {
		log.Warn("cannot mark the worker's workspaces as seen", "err", err)
	}
}

func (d *Dispatcher) markPending(w *worker, r store.WorkspaceRow) {
	d.mu.Lock()
	w.pending[r.ID] = r
	d.mu.Unlock()
}

// recoverLost settles a lost row whose directory the worker came back with.
// A principal's row (opened over the MCP server) is ready again. Its owner
// run still live: the row is ready again (a compare-and-set from
// lost, so a sweeper that moved it meanwhile wins). Its owner run ended or
// gone: the directory is to be removed and the row deleted once the worker
// confirms, which recoverLost reports by returning true. A run that cannot
// be read changes nothing until the next attach.
func (d *Dispatcher) recoverLost(ctx context.Context, w *worker, r store.WorkspaceRow, log *slog.Logger) (remove bool) {
	if r.OwnerKind == store.WorkspaceOwnerPrincipal {
		// No run owns it (RFC GW D35): it comes back into service, and the
		// principal release pass decides when it ends, as for one never lost.
		// Read as a run, its pin key names no run row, and it would be removed.
		if ok, err := d.store().WorkspaceSetState(ctx, r.TenantID, r.ID, store.WorkspaceLost, store.WorkspaceReady); err != nil {
			log.Warn("cannot mark a returned workspace ready", "workspace_id", r.ID, "err", err)
		} else if ok {
			log.Info("a lost workspace came back with its worker", "workspace_id", r.ID)
		}
		return false
	}
	run, err := d.store().GetRun(ctx, r.OwnerRunID)
	var nf *store.ErrNotFound
	switch {
	case errors.As(err, &nf):
	case err != nil:
		log.Warn("cannot read a lost workspace's owner run; settling it at the worker's next attach", "workspace_id", r.ID, "err", err)
		return false
	case !store.IsTerminalRunStatus(run.Status):
		ok, err := d.store().WorkspaceSetState(ctx, r.TenantID, r.ID, store.WorkspaceLost, store.WorkspaceReady)
		if err != nil {
			log.Warn("cannot mark a returned workspace ready", "workspace_id", r.ID, "err", err)
		} else if ok {
			log.Info("a lost workspace came back with its worker", "workspace_id", r.ID)
		}
		return false
	}
	d.markPending(w, r)
	return true
}

// confirmRemoved deletes the rows a Remove named (released, or lost with
// their run ended) once the worker says their directories are gone. An id no
// Remove named is ignored.
func (d *Dispatcher) confirmRemoved(ctx context.Context, w *worker, ids []string, log *slog.Logger) {
	var rows []store.WorkspaceRow
	d.mu.Lock()
	for _, id := range ids {
		if r, ok := w.pending[id]; ok {
			rows = append(rows, r)
			delete(w.pending, id)
		}
	}
	d.mu.Unlock()
	for _, r := range rows {
		d.deleteRow(ctx, r, log)
	}
}

func (d *Dispatcher) deleteRow(ctx context.Context, r store.WorkspaceRow, log *slog.Logger) {
	if _, err := d.store().WorkspaceDelete(ctx, r.TenantID, r.ID); err != nil {
		log.Warn("cannot delete a released workspace's row; its worker's next attach tries again", "workspace_id", r.ID, "err", err)
	}
}

// detach removes a worker, ends every one of its streams and tells the ops
// waiting for one of them that it is gone; gone is what an op in flight
// fails with.
func (d *Dispatcher) detach(w *worker, died bool, gone *OpError) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.workers[w.id] == w {
		delete(d.workers, w.id)
	}
	if died || w.inFlight > 0 || w.diedMidOp {
		d.deaths++
	}
	w.gone = gone
	close(w.lost)
	// Every stream, busy ones too: an op in flight on a worker whose token
	// was revoked must not finish there.
	for s := range w.streams {
		s.cancel()
	}
	w.idle = nil
	for _, wt := range w.waiters {
		wt <- nil
	}
	w.waiters = nil
}

// ---- Op ----

// Op holds one idle stream of an attached worker until an op is handed to
// it, runs that op on the stream, and ends.
func (d *Dispatcher) Op(stream workspacepb.WorkspaceService_OpServer) error {
	owner, _, _, err := d.authenticate(stream.Context())
	if err != nil {
		return err
	}
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	ready := first.GetReady()
	if ready == nil {
		return status.Error(codes.InvalidArgument, "the first Op frame must be Ready")
	}
	ctx, cancel := context.WithCancel(stream.Context())
	defer cancel()
	s := &opStream{ops: make(chan *op, 1), ctx: ctx, cancel: cancel}
	d.mu.Lock()
	// The id is built from what this stream's own token grants: a stream
	// naming a worker of another tenant (or a shared worker, from a tenant
	// token) finds nothing.
	w := d.workers[owner+"/"+ready.GetWorkerId()]
	if w == nil {
		d.mu.Unlock()
		return status.Error(codes.PermissionDenied, "no worker this token grants is attached under that id")
	}
	select {
	case <-w.lost:
		d.mu.Unlock()
		return status.Error(codes.PermissionDenied, "the worker is no longer attached")
	default:
	}
	s.w = w
	w.streams[s] = struct{}{}
	w.requested = max(w.requested-1, 0)
	d.offerLocked(s)
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		d.dropIdleLocked(s)
		delete(w.streams, s)
		d.mu.Unlock()
	}()

	select {
	case o := <-s.ops:
		if o == nil {
			return ctx.Err()
		}
		o.done <- d.serve(ctx, stream, w, o)
		return nil
	case <-ctx.Done():
		d.mu.Lock()
		taken := s.taken
		d.dropIdleLocked(s)
		d.mu.Unlock()
		if taken {
			// An op was handed to this stream as it went away: nothing of it
			// reached the worker, so it may run on another stream.
			if o := <-s.ops; o != nil {
				o.done <- result{retry: true}
			}
		}
		return ctx.Err()
	}
}

// offerLocked hands an idle stream to the first op waiting for one of its
// worker's streams, or keeps it idle.
func (d *Dispatcher) offerLocked(s *opStream) {
	w := s.w
	if len(w.waiters) > 0 {
		wt := w.waiters[0]
		w.waiters = w.waiters[1:]
		s.taken = true
		wt <- s
		return
	}
	w.idle = append(w.idle, s)
}

func (d *Dispatcher) dropIdleLocked(s *opStream) {
	w := s.w
	for i, x := range w.idle {
		if x == s {
			w.idle = append(w.idle[:i], w.idle[i+1:]...)
			return
		}
	}
}

// askForStreamsLocked asks the worker for more streams when its idle ones
// run low, once until they arrive.
func (d *Dispatcher) askForStreamsLocked(w *worker) {
	if len(w.idle) >= lowWater || w.requested > 0 {
		return
	}
	select {
	case w.ctl <- &workspacepb.CoreControl{Msg: &workspacepb.CoreControl_OpenStreams{OpenStreams: &workspacepb.OpenStreams{N: refill}}}:
		w.requested = refill
	default:
		// The control queue is full; the next op asks again.
	}
}

// acquire takes w's oldest idle stream, waiting up to StreamWait.
func (d *Dispatcher) acquire(ctx context.Context, w *worker) (*opStream, error) {
	d.mu.Lock()
	select {
	case <-w.lost:
		gone := w.gone
		d.mu.Unlock()
		return nil, gone
	default:
	}
	if len(w.idle) > 0 {
		s := w.idle[0]
		w.idle = w.idle[1:]
		s.taken = true
		d.askForStreamsLocked(w)
		d.mu.Unlock()
		return s, nil
	}
	wt := make(chan *opStream, 1)
	w.waiters = append(w.waiters, wt)
	d.askForStreamsLocked(w)
	d.mu.Unlock()

	t := time.NewTimer(d.opt.StreamWait)
	defer t.Stop()
	var cause error
	select {
	case s := <-wt:
		if s == nil {
			d.mu.Lock()
			gone := w.gone
			d.mu.Unlock()
			return nil, gone
		}
		return s, nil
	case <-ctx.Done():
		cause = ctx.Err()
	case <-t.C:
		cause = &OpError{Class: workspace.ClassBusy, Detail: "the worker kept no stream free"}
	}
	d.mu.Lock()
	for i, x := range w.waiters {
		if x == wt {
			w.waiters = append(w.waiters[:i], w.waiters[i+1:]...)
			d.mu.Unlock()
			return nil, cause
		}
	}
	d.mu.Unlock()
	// A stream (or the news that the worker is gone) was handed over as the
	// wait ended: give a stream back rather than lose it.
	if s := <-wt; s != nil {
		d.mu.Lock()
		if s.ctx.Err() != nil {
			d.mu.Unlock()
			s.ops <- nil
		} else {
			s.taken = false
			d.offerLocked(s)
			d.mu.Unlock()
		}
	}
	return nil, cause
}

// ---- placement and stats ----

// PlaceRequest is what a workspace needs of its worker.
type PlaceRequest struct {
	// TenantID is the tenant the workspace is for; a worker is placed only
	// when it serves that tenant.
	TenantID string
	// UserID is the user the workspace is for. Unused until personal
	// workers (RFC GW §4.5) exist.
	UserID string
	// Toolchains are the toolchains the worker must carry, all of them.
	Toolchains []string
	// Engine is the least isolating engine accepted; "" accepts any.
	Engine string
}

// Place returns the attached workers a workspace may be opened on, least
// loaded first: each serves the tenant, carries every toolchain asked, runs
// an engine at least as isolating as asked, and holds fewer active
// workspaces (counted from the store, so replicas agree) than its capacity.
// The caller opens on the first and moves to the next when one answers
// at_capacity. An error is an *OpError of class no_worker or at_capacity, or
// the store's.
func (d *Dispatcher) Place(ctx context.Context, req PlaceRequest) ([]string, error) {
	type cand struct {
		id        string
		used, cap int
	}
	var matched []cand
	d.mu.Lock()
	for _, w := range d.workers {
		// engineServes is checked at attach too; again here so a shared
		// host worker can never be handed a workspace, whatever let it in.
		if !w.servesTenant(req.TenantID) || !engineServes(w.serves, w.engine) || workspace.EngineRank(w.engine) < workspace.EngineRank(req.Engine) {
			continue
		}
		ok := true
		for _, t := range req.Toolchains {
			ok = ok && w.tools[t]
		}
		if ok {
			matched = append(matched, cand{id: w.id, cap: w.capacity})
		}
	}
	d.mu.Unlock()
	if len(matched) == 0 {
		detail := "no attached workspace worker serves this tenant with the toolchains and engine asked"
		if req.TenantID == "" {
			detail = "the run has no tenant, and only a local worker configured for the empty tenant (workspace.mode local, workspace.local.tenant unset, no authentication) serves one"
		}
		return nil, &OpError{Class: workspace.ClassNoWorker, Detail: detail}
	}
	var free []cand
	for _, c := range matched {
		n, err := d.store().WorkspaceCountActiveByWorker(ctx, c.id)
		if err != nil {
			return nil, fmt.Errorf("workspace placement: counting a worker's workspaces: %w", err)
		}
		if n < c.cap {
			c.used = n
			free = append(free, c)
		}
	}
	if len(free) == 0 {
		return nil, &OpError{Class: workspace.ClassAtCapacity, Detail: "every workspace worker that could take it is full"}
	}
	sort.Slice(free, func(i, j int) bool {
		// used/cap ascending, without division.
		li, lj := free[i].used*free[j].cap, free[j].used*free[i].cap
		if li != lj {
			return li < lj
		}
		return free[i].id < free[j].id
	})
	out := make([]string, len(free))
	for i, c := range free {
		out[i] = c.id
	}
	return out, nil
}

// Attached reports whether a worker of this id is attached to this replica
// and serves tenant.
func (d *Dispatcher) Attached(workerID, tenant string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	w := d.workers[workerID]
	return w != nil && w.servesTenant(tenant)
}

// WorkerInfo is one attached worker as its Hello described it.
type WorkerInfo struct {
	// ID is the core's id for it: what a workspace row's worker_id holds.
	ID string
	// Declared is the id the worker gave itself.
	Declared   string
	TenantID   string
	Shared     bool
	Engine     string
	Toolchains []string
	// Labels are the conversion labels the worker reported: the helpers
	// its conversion child has (RFC GW D34).
	Labels   []string
	Capacity int
}

// Workers lists the workers attached to this replica, by id.
func (d *Dispatcher) Workers() []WorkerInfo {
	d.mu.Lock()
	out := make([]WorkerInfo, 0, len(d.workers))
	for _, w := range d.workers {
		info := WorkerInfo{ID: w.id, Declared: w.declared, TenantID: w.serves.TenantID, Shared: w.serves.Shared, Engine: w.engine, Capacity: w.capacity,
			Labels: append([]string(nil), w.labels...)}
		for t := range w.tools {
			info.Toolchains = append(info.Toolchains, t)
		}
		sort.Strings(info.Toolchains)
		out = append(out, info)
	}
	d.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Stats reports the workers and ops for /metrics.
func (d *Dispatcher) Stats() workspace.Stats {
	d.mu.Lock()
	defer d.mu.Unlock()
	st := workspace.Stats{Workers: len(d.workers), Ops: make(map[string]uint64, len(d.ops)), Deaths: d.deaths,
		OutputBytes: make(map[string]uint64, len(d.outBytes))}
	for k, v := range d.ops {
		st.Ops[k] = v
	}
	for k, v := range d.outBytes {
		st.OutputBytes[k] = v
	}
	return st
}

// countOutput adds n bytes received for op.
func (d *Dispatcher) countOutput(op string, n int) {
	d.mu.Lock()
	if d.outBytes == nil {
		d.outBytes = map[string]uint64{}
	}
	d.outBytes[opLabel(op)] += uint64(n)
	d.mu.Unlock()
}

func (d *Dispatcher) count(opName, outcome string) {
	d.mu.Lock()
	d.ops[opName+"/"+outcome]++
	d.mu.Unlock()
}
