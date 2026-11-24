// Package worker is `loomcycle worker --role workspace` (RFC GW §4.1, §8): a
// process that holds workspaces as directories under a root of its own,
// attaches to the core's worker endpoint, keeps a pool of idle Op streams
// open for the core to use, and runs the ops it is handed.
//
// It loads no config, opens no store and holds no provider key. An op names a
// workspace by id; the id is the directory's name, checked so it can never
// leave the root (workspace.ValidID). Which run or tenant a workspace belongs
// to is the core's business, never the worker's.
//
// It runs open, remove and status for the core's bookkeeping, and for the
// Workspace tool a command (exec, through an engine.Engine) and the file
// tools' operations on the workspace's directory (read, write, edit, glob,
// grep, through internal/fsops).
package worker

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/denn-gubsky/loomcycle/internal/convert"
	"github.com/denn-gubsky/loomcycle/internal/safego"
	"github.com/denn-gubsky/loomcycle/internal/workerlink"
	"github.com/denn-gubsky/loomcycle/internal/workspace"
	"github.com/denn-gubsky/loomcycle/internal/workspace/engine"
	"github.com/denn-gubsky/loomcycle/internal/workspace/kernel"
	"github.com/denn-gubsky/loomcycle/internal/workspace/workspacepb"
)

// Defaults and bounds.
const (
	DefaultCapacity    = 8
	DefaultIdleStreams = 16
	// MaxIdleStreams bounds the idle streams a worker keeps by itself (RFC
	// GW §8).
	MaxIdleStreams = 32
	// maxExtraStreams bounds the one-op streams OpenStreams adds at once.
	maxExtraStreams = 64
	// maxInput bounds an op's input: the arguments of a write or an edit,
	// whose 1 MiB file body may grow up to six times as JSON.
	maxInput       = 8 << 20
	heartbeatEvery = 5 * time.Second
	// endWait bounds how long a finished op's stream may take to close.
	endWait = 10 * time.Second
)

// Options configure a workspace worker.
type Options struct {
	// Target is the dispatchers to attach to (workerlink.ParseTargets).
	Target string
	// Token is an operator token holding only workers:attach.
	Token string
	// ID names the worker to the core; default the host name, which must not
	// change across restarts (defaultID).
	ID string
	// ParentPID, when set, ends the worker once its parent is gone: the
	// runtime sets it on the worker it starts as its child (workspace.mode
	// local).
	ParentPID    int
	BuildVersion string
	// Root is the directory the workspaces are made in.
	Root string
	// Engine runs commands: host (the default) or container.
	Engine string
	// Container configures the container engine. The worker fills its
	// user (its own uid:gid), worker key and commands.
	Container engine.ContainerOptions
	// HostEgress gives each workspace of a host worker a loopback port its
	// commands reach their routes at (LOOMCYCLE_WORKSPACE_HOST_EGRESS; RFC
	// GW P4 plan O13). Not a boundary: every process on the host can reach
	// the port (engine.Host).
	HostEgress bool
	// hostEgress is the handler a host workspace's port serves, set by
	// wireEgress.
	hostEgress func(workspace string) http.Handler
	// Capacity is the workspaces held at most; default DefaultCapacity.
	Capacity int
	// IdleStreams is the idle Op streams kept open; default 16, at most 32.
	IdleStreams int
	// Toolchains are reported to the core; nil detects python and node on
	// PATH (DetectToolchains).
	Toolchains []string
	// Commands, when not empty, are the programs an exec may start with
	// (LOOMCYCLE_WORKSPACE_COMMANDS); see engine.Host.Commands.
	Commands []string
	// Labels are the conversion labels (conversion/wire.Label*) of the
	// helpers this worker's conversion child has, reported at attach: what
	// a workspace's file can be converted to here (RFC GW D34). A worker
	// that converts nothing reports none.
	Labels []string
	// Convert runs the conversions of a workspace's files. nil starts the
	// conversion child ConvertChild describes; with neither, the worker
	// converts nothing and refuses the convert ops.
	Convert convert.Client
	// ConvertChild is the conversion child to start when Convert is nil.
	ConvertChild *ConvertChild
	// MeasureBudget bounds the walk that counts a workspace's bytes for its
	// quota; past it the count is an estimate. Zero is 10 s.
	MeasureBudget time.Duration
	// Observe, when set, is called with each op's name and arguments as
	// the op arrives. A test seam, not a setting: it is how a test sees
	// everything the core asked this worker.
	Observe  func(op string, args []byte)
	TLS      *tls.Config
	Insecure bool
	// Resolve and ResolveEvery are workerlink's.
	Resolve      func(ctx context.Context, host string) ([]string, error)
	ResolveEvery time.Duration
	Logger       *slog.Logger
}

// ErrRefused is the core refusing this worker for good: a token that may not
// attach a worker, another protocol version.
var ErrRefused = workerlink.ErrRefused

// DetectToolchains reports python and node when each is found on PATH.
func DetectToolchains() []string {
	var out []string
	if _, err := exec.LookPath("python3"); err == nil {
		out = append(out, "python")
	} else if _, err := exec.LookPath("python"); err == nil {
		out = append(out, "python")
	}
	if _, err := exec.LookPath("node"); err == nil {
		out = append(out, "node")
	}
	return out
}

// Run serves the dispatchers until ctx ends, reconnecting with backoff.
func Run(ctx context.Context, opt Options) error {
	if opt.Token == "" {
		return errors.New("workspace worker: LOOMCYCLE_WORKER_TOKEN is not set")
	}
	if opt.Target == "" {
		return errors.New("workspace worker: no dispatcher address (--dispatcher)")
	}
	if opt.Root == "" {
		return errors.New("workspace worker: LOOMCYCLE_WORKSPACE_ROOT is not set")
	}
	if opt.Engine == "" {
		opt.Engine = workspace.EngineHost
	}
	if opt.Engine != workspace.EngineHost && opt.Engine != workspace.EngineContainer {
		return fmt.Errorf("workspace worker: engine %q is not one this worker runs (want host or container)", opt.Engine)
	}
	if opt.Capacity <= 0 {
		opt.Capacity = DefaultCapacity
	}
	if opt.IdleStreams <= 0 {
		opt.IdleStreams = DefaultIdleStreams
	}
	opt.IdleStreams = min(opt.IdleStreams, MaxIdleStreams)
	if opt.ID == "" {
		opt.ID = defaultID()
	}
	if opt.Logger == nil {
		opt.Logger = slog.Default()
	}
	root, err := filepath.Abs(opt.Root)
	if err != nil {
		return fmt.Errorf("workspace worker: the root: %w", err)
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return fmt.Errorf("workspace worker: making the root: %w", err)
	}
	opt.Root = root
	targets, err := workerlink.ParseTargets(opt.Target)
	if err != nil {
		return err
	}
	log := opt.Logger.With("component", "workspace-worker", "worker_id", opt.ID)
	if opt.ParentPID > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(ctx)
		defer cancel()
		safego.Go(ctx, "workspace.worker.parent", func() { workerlink.WatchParent(ctx, opt.ParentPID, cancel) })
	}
	// The session network's requests reach this worker's egress handler
	// (RFC GW §6.3); self is set below, before any session can start.
	var self *worker
	wireEgress(&opt, &self)
	eng, err := newEngine(ctx, &opt, log)
	if err != nil {
		return err
	}
	defer eng.Close()
	if opt.Convert == nil && opt.ConvertChild != nil {
		c, stop, err := startConverter(opt.BuildVersion, opt.ConvertChild, log)
		if err != nil {
			log.Warn("cannot start the conversion child; this worker converts no workspace file", "err", err)
		} else {
			defer stop()
			opt.Convert = c
		}
	}
	if opt.Convert == nil {
		opt.Labels = nil
	}
	// The checkpoint mechanism is the disk's, probed once and reported as a
	// label (RFC GW §5.4).
	mech := probeMechanism(root)
	opt.Labels = append(opt.Labels, workspace.CheckpointLabelPrefix+mech)
	log.Info("checkpoints are recorded by this mechanism", "mechanism", mech)
	w := &worker{opt: opt, log: log, engine: eng, mech: mech}
	self = w
	// The kernels die with the worker; the next cell after it comes back
	// says kernel_restarted (RFC GW §5.7). A host worker writes the drivers
	// it carries into its root; a session image carries its own copies
	// (Dockerfile.workspace), at a path only the session sees.
	kopt := kernel.Options{Engine: eng, DriverDir: filepath.Join(root, driversDir), MarkDir: root, Logger: log}
	if opt.Engine == workspace.EngineContainer {
		kopt.Drivers = &kernel.Drivers{Python: kernel.ImageDriversDir + "/python_driver.py", Node: kernel.ImageDriversDir + "/node_driver.js"}
	}
	w.kernels = kernel.NewManager(kopt)
	defer w.kernels.Close()
	// The servers die with the worker, before the engine closes.
	defer w.servers.Close()
	w.cleanWork()
	return workerlink.Serve(ctx, targets, workerlink.Options{
		TLS: opt.TLS, Insecure: opt.Insecure, Resolve: opt.Resolve, ResolveEvery: opt.ResolveEvery,
		Name: "workspace.worker", Logger: log,
	}, w.session)
}

// newEngine makes the engine opt names and fills opt.Toolchains when it is
// nil: on host, what the machine's PATH has; on container, what the session
// image has, found by starting one session under the same confinement. The
// container engine also removes the sessions a previous run of this worker
// left.
func newEngine(ctx context.Context, opt *Options, log *slog.Logger) (engine.Engine, error) {
	if opt.Engine == workspace.EngineHost {
		log.Warn("the host engine runs commands as this process's user, with no isolation; use it only on a trusted machine", "root", opt.Root)
		if opt.Toolchains == nil {
			opt.Toolchains = DetectToolchains()
		}
		return &engine.Host{Commands: opt.Commands, Egress: opt.hostEgress}, nil
	}
	co := opt.Container
	// The session runs as this process's user: files the worker writes and
	// files a command writes then have one owner. NewContainer refuses root.
	co.User = fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())
	co.WorkerKey = engine.WorkerKey(opt.ID, opt.Root)
	co.Commands = opt.Commands
	c, err := engine.NewContainer(co)
	if err != nil {
		return nil, fmt.Errorf("workspace worker: %w", err)
	}
	tc, err := c.Probe(ctx)
	if err != nil {
		return nil, fmt.Errorf("workspace worker: %w", err)
	}
	if opt.Toolchains == nil {
		opt.Toolchains = tc
	}
	if err := c.Reconcile(ctx); err != nil {
		return nil, fmt.Errorf("workspace worker: %w", err)
	}
	log.Info("commands run in a container per workspace", "image", co.Image, "runtime", co.Runtime, "toolchains", opt.Toolchains)
	return c, nil
}

// defaultID is the host's name. It must stay the same across restarts: the
// core keys a worker's workspace rows on it, and a worker that came back
// under a new id would have every directory it holds read as unknown and
// removed. Two workers on one host each need an --id of their own.
func defaultID() string {
	host, _ := os.Hostname()
	// A hostname may carry characters the core refuses in an id.
	out := []byte(host)
	for i, c := range out {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '.', c == '-', c == '_':
		default:
			out[i] = '-'
		}
	}
	if len(out) > 128 {
		out = out[:128]
	}
	if len(out) == 0 {
		return "workspace-worker"
	}
	return string(out)
}

type worker struct {
	opt    Options
	log    *slog.Logger
	engine engine.Engine
	// kernels are the workspaces' interpreters; nil runs none.
	kernels *kernel.Manager
	// servers are the workspaces' long-lived commands (RFC GW §5.3).
	servers servers
	// mu serialises open's capacity check with the directory it makes.
	mu sync.Mutex
	// quotas is each workspace's disk count and write lock.
	quotas quotas
	// extra counts the one-op streams OpenStreams opened and still open.
	extra atomic.Int32
	// links are the attached connections a session's request out is sent
	// to the core on (egress_client.go).
	links egressLinks
	// mech is the checkpoint mechanism probed at start (RFC GW §5.4);
	// empty copies.
	mech string
}

func (w *worker) outgoing(ctx context.Context) context.Context {
	return metadata.AppendToOutgoingContext(ctx, workspace.MDAuthorization, "Bearer "+w.opt.Token)
}

// held lists the workspace directories under the root.
func (w *worker) held() ([]string, error) {
	ents, err := os.ReadDir(w.opt.Root)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range ents {
		if e.IsDir() && workspace.ValidID(e.Name()) {
			out = append(out, e.Name())
		}
	}
	return out, nil
}

// refused reports whether a status from the core says retrying cannot help.
func refused(err error) bool {
	switch status.Code(err) {
	case codes.Unauthenticated, codes.PermissionDenied, codes.FailedPrecondition, codes.InvalidArgument:
		return true
	}
	return false
}

// session is one attachment: Hello, Welcome, heartbeats, the core's control
// messages, and the idle Op streams.
func (w *worker) session(parent context.Context, conn *grpc.ClientConn, log *slog.Logger) error {
	client := workspacepb.NewWorkspaceServiceClient(conn)
	ctx, cancel := context.WithCancel(w.outgoing(parent))
	defer cancel()
	held, err := w.held()
	if err != nil {
		return fmt.Errorf("listing the workspaces on disk: %w", err)
	}
	att, err := client.Attach(ctx)
	if err != nil {
		return err
	}
	if err := att.Send(&workspacepb.WorkerMessage{Msg: &workspacepb.WorkerMessage_Hello{Hello: &workspacepb.Hello{
		WorkerId: w.opt.ID, ProtocolVersion: workspace.ProtocolVersion, BuildVersion: w.opt.BuildVersion,
		Engine: w.opt.Engine, Toolchains: w.opt.Toolchains, Labels: w.opt.Labels,
		Capacity: uint32(w.opt.Capacity), HeldWorkspaces: held, IdleStreams: uint32(w.opt.IdleStreams),
	}}}); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	// A Send that met io.EOF means the core already ended the stream, a
	// refusal sent before it read the Hello; only Recv returns its status.
	ctl, err := att.Recv()
	if err != nil {
		if refused(err) {
			return fmt.Errorf("%w: %v", ErrRefused, status.Convert(err).Message())
		}
		return err
	}
	if b := ctl.GetWelcome().GetBuildVersion(); b != w.opt.BuildVersion {
		log.Warn("the dispatcher runs another build", "dispatcher_build", b, "build", w.opt.BuildVersion)
	}
	log.Info("attached to the dispatcher", "engine", w.opt.Engine, "capacity", w.opt.Capacity, "toolchains", w.opt.Toolchains, "held", len(held))
	// While attached, this connection can carry the sessions' requests out.
	defer w.links.add(client)()

	var wg sync.WaitGroup
	defer wg.Wait()
	defer cancel()
	for range w.opt.IdleStreams {
		wg.Add(1)
		safego.Go(ctx, "workspace.worker.stream", func() {
			defer wg.Done()
			for ctx.Err() == nil {
				if err := w.serveOne(ctx, client); err != nil && ctx.Err() == nil {
					log.Warn("an op stream ended", "err", err)
					select {
					case <-ctx.Done():
					case <-time.After(workerlink.BackoffMin):
					}
				}
			}
		})
	}

	// Control messages arrive on a goroutine of their own; every Send on the
	// Attach stream happens below, on this one.
	// It is in wg itself, so the Adds it makes for extra streams never race
	// the Wait above with the count at zero.
	removes := make(chan []string, 4)
	errc := make(chan error, 1)
	wg.Add(1)
	safego.Go(ctx, "workspace.worker.control", func() {
		defer wg.Done()
		for {
			m, err := att.Recv()
			if err != nil {
				errc <- err
				return
			}
			switch {
			case m.GetRemove() != nil:
				select {
				case removes <- m.GetRemove().GetWorkspaceIds():
				case <-ctx.Done():
					return
				}
			case m.GetOpenStreams() != nil:
				w.openExtra(ctx, client, &wg, int(m.GetOpenStreams().GetN()), log)
			}
		}
	})
	t := time.NewTicker(heartbeatEvery)
	defer t.Stop()
	for {
		select {
		case err := <-errc:
			return err
		case ids := <-removes:
			gone := w.removeAll(ids, log)
			if err := att.Send(&workspacepb.WorkerMessage{Msg: &workspacepb.WorkerMessage_Removed{Removed: &workspacepb.Removed{WorkspaceIds: gone}}}); err != nil {
				return err
			}
		case <-t.C:
			n, _ := w.held()
			if err := att.Send(&workspacepb.WorkerMessage{Msg: &workspacepb.WorkerMessage_Heartbeat{Heartbeat: &workspacepb.Heartbeat{Held: uint32(len(n))}}}); err != nil {
				return err
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// openExtra opens n streams that each carry one op, within maxExtraStreams.
func (w *worker) openExtra(ctx context.Context, client workspacepb.WorkspaceServiceClient, wg *sync.WaitGroup, n int, log *slog.Logger) {
	for range n {
		if w.extra.Add(1) > maxExtraStreams {
			w.extra.Add(-1)
			return
		}
		wg.Add(1)
		safego.Go(ctx, "workspace.worker.extra-stream", func() {
			defer wg.Done()
			defer w.extra.Add(-1)
			if err := w.serveOne(ctx, client); err != nil && ctx.Err() == nil {
				log.Warn("an extra op stream ended", "err", err)
			}
		})
	}
}

// removeAll removes the directories of ids the core named, and returns the
// ids now gone. An id that is not a workspace id is never touched.
func (w *worker) removeAll(ids []string, log *slog.Logger) []string {
	var gone []string
	for _, id := range ids {
		if !workspace.ValidID(id) {
			continue
		}
		w.kernels.Drop(id)
		w.servers.Drop(id)
		// The session first, so nothing still running writes into the
		// directory while it is removed.
		if err := w.engine.Forget(context.Background(), id); err != nil {
			log.Warn("cannot end the session of a workspace the dispatcher released", "workspace_id", id, "err", err)
			continue
		}
		if err := os.RemoveAll(filepath.Join(w.opt.Root, id)); err != nil {
			log.Warn("cannot remove a workspace the dispatcher released", "workspace_id", id, "err", err)
			continue
		}
		if err := w.dropCheckpoints(id); err != nil {
			log.Warn("cannot remove the checkpoints of a workspace the dispatcher released", "workspace_id", id, "err", err)
			continue
		}
		w.dropQuota(id)
		gone = append(gone, id)
	}
	if len(gone) > 0 {
		log.Info("removed workspaces released while away", "count", len(gone))
	}
	return gone
}
