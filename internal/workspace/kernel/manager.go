package kernel

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/safego"
	"github.com/denn-gubsky/loomcycle/internal/workspace/engine"
)

// DefaultStartWait bounds how long a driver may take to say it is ready.
const DefaultStartWait = 30 * time.Second

// markerSuffix names the file that records a kernel was started for a
// workspace: <id><markerSuffix><kind>, beside the workspaces in the worker's
// root. It outlives the kernel and the worker, so the first cell after a
// kernel died, for any reason, can say so (kernel_restarted).
const markerSuffix = ".kernel-"

// ErrUnavailable is a kernel the worker cannot start: no interpreter, no
// driver. Its message says why.
var ErrUnavailable = errors.New("kernel unavailable")

// Options configure a Manager.
type Options struct {
	Engine engine.Engine
	// Drivers locates the driver files. Nil writes the embedded ones into
	// DriverDir the first time a kernel starts (the host engine).
	Drivers *Drivers
	// DriverDir is where WriteDrivers puts them.
	DriverDir string
	// MarkDir holds the started-kernel markers: the worker's root.
	MarkDir   string
	StartWait time.Duration
	Logger    *slog.Logger
}

// Request is one cell.
type Request struct {
	Kind string
	Code string
	// Workspace is the workspace's id, for the engine's session.
	Workspace string
	// Dir is the workspace's directory, where the kernel starts.
	Dir     string
	Timeout time.Duration
	Grace   time.Duration
}

// Result is a cell's answer.
type Result struct {
	Output          string
	Value           *string
	Error           string
	Pictures        []string
	ExecutionCount  int
	Truncated       bool
	Interrupted     bool
	TimedOut        bool
	KernelKilled    bool
	KernelDied      bool
	KernelRestarted bool
}

// Manager holds a worker's kernels: at most one per kind per workspace, each
// behind a lock a cell holds while it runs, so cells of one kernel run one
// at a time and a second waits for the first.
type Manager struct {
	opt Options
	log *slog.Logger

	mu      sync.Mutex
	slots   map[slotKey]*slot
	drivers *Drivers
	closed  bool
}

type slotKey struct{ id, kind string }

type slot struct {
	// lock is held by the cell that runs, and by a reset.
	lock chan struct{}
	k    *proc
	// n counts the cells this kernel ran.
	n int
}

// NewManager makes a Manager.
func NewManager(opt Options) *Manager {
	if opt.StartWait <= 0 {
		opt.StartWait = DefaultStartWait
	}
	if opt.Logger == nil {
		opt.Logger = slog.Default()
	}
	return &Manager{opt: opt, log: opt.Logger, slots: map[slotKey]*slot{}, drivers: opt.Drivers}
}

func (m *Manager) slot(id, kind string) (*slot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, errors.New("the worker is stopping")
	}
	key := slotKey{id, kind}
	s := m.slots[key]
	if s == nil {
		s = &slot{lock: make(chan struct{}, 1)}
		m.slots[key] = s
	}
	return s, nil
}

func (s *slot) acquire(ctx context.Context) error {
	select {
	case s.lock <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *slot) release() { <-s.lock }

// Run runs one cell in workspace id's kernel of req.Kind, starting the
// kernel if none runs. When ctx ends before the cell does, Run returns ctx's
// error and finishes the cell in the background (interrupt, grace, kill),
// holding the kernel's lock until then.
func (m *Manager) Run(ctx context.Context, id string, req Request) (Result, error) {
	if !Valid(req.Kind) {
		return Result{}, fmt.Errorf("%w: no kernel %q (want python or node)", ErrUnavailable, req.Kind)
	}
	s, err := m.slot(id, req.Kind)
	if err != nil {
		return Result{}, err
	}
	if err := s.acquire(ctx); err != nil {
		return Result{}, err
	}
	held := true
	defer func() {
		if held {
			s.release()
		}
	}()
	var res Result
	if s.k == nil || !s.k.alive() {
		if s.k != nil {
			s.k.kill()
		}
		s.k, s.n = nil, 0
		restarted := m.marked(id, req.Kind)
		req.Workspace = id
		k, err := m.startKernel(ctx, req)
		if err != nil {
			return Result{}, err
		}
		if err := m.mark(id, req.Kind); err != nil {
			k.kill()
			return Result{}, fmt.Errorf("recording the kernel: %w", err)
		}
		s.k = k
		res.KernelRestarted = restarted
	}
	s.n++
	o, settle, err := s.k.run(ctx, cellRequest{code: req.Code, n: s.n, timeout: req.Timeout, grace: req.Grace, maxOutput: MaxResult})
	if settle != nil {
		held = false
		safego.Go(context.WithoutCancel(ctx), "workspace.kernel.settle", func() {
			defer s.release()
			if o := settle(); o.killed || o.died {
				s.k = nil
				m.log.Info("a kernel whose cell the core stopped waiting for was killed or died", "workspace_id", id, "kernel", req.Kind)
			}
		})
		return Result{}, err
	}
	if err != nil {
		return Result{}, err
	}
	res.ExecutionCount = s.n
	fill(&res, o)
	if o.killed || o.died {
		s.k = nil
	}
	return res, nil
}

// fill turns an outcome into a result, within the result's bounds.
func fill(res *Result, o outcome) {
	res.TimedOut = o.timedOut
	res.KernelKilled = o.killed
	res.KernelDied = o.died && !o.killed
	if d := o.done; d != nil {
		if d.Value != nil {
			v, cut := cutBytes(*d.Value, MaxValue)
			res.Value = &v
			res.Truncated = res.Truncated || cut
		}
		e, cut := cutBytes(d.Error, MaxError)
		res.Error = e
		res.Truncated = res.Truncated || cut
		res.Interrupted = d.Interrupted
		for _, p := range d.Pictures {
			if len(res.Pictures) < MaxPictures && pictureName.MatchString(p) {
				res.Pictures = append(res.Pictures, p)
			}
		}
		if d.Dropped > 0 {
			res.Truncated = true
		}
	}
	room := MaxResult - len(res.Error)
	if res.Value != nil {
		room -= len(*res.Value)
	}
	out, cut := cutBytes(o.output, max(room, 0))
	res.Output = out
	res.Truncated = res.Truncated || cut || o.dropped > 0
	switch {
	case o.killed:
		res.Error = strings.TrimSpace(res.Error + "\nthe cell did not stop when interrupted, so the kernel was killed; its variables are gone, and the next cell starts a new kernel")
	case o.died:
		res.Error = strings.TrimSpace(res.Error + "\nthe kernel exited during the cell; its variables are gone, and the next cell starts a new kernel")
	case o.timedOut && res.Error == "":
		res.Error = "the cell was interrupted at its timeout; the kernel's variables are kept"
	}
}

func cutBytes(s string, n int) (string, bool) {
	if len(s) <= n {
		return s, false
	}
	return cutUTF8(s, n), true
}

// Reset stops workspace id's kernel of kind, if one runs. The next cell
// starts a new kernel and is not told kernel_restarted: the agent asked.
func (m *Manager) Reset(ctx context.Context, id, kind string) (bool, error) {
	if !Valid(kind) {
		return false, fmt.Errorf("%w: no kernel %q (want python or node)", ErrUnavailable, kind)
	}
	s, err := m.slot(id, kind)
	if err != nil {
		return false, err
	}
	if err := s.acquire(ctx); err != nil {
		return false, err
	}
	defer s.release()
	stopped := false
	if s.k != nil {
		stopped = s.k.alive()
		s.k.kill()
		s.k, s.n = nil, 0
	}
	m.unmark(id, kind)
	return stopped, nil
}

// Drop kills workspace id's kernels and forgets them: the workspace is
// being removed, or put back to a checkpoint (RFC GW P6). A cell running in
// one ends as killed. It reports whether a kernel was alive. Drop, Close and
// Clean do nothing on a nil Manager, a worker that runs no kernels.
func (m *Manager) Drop(id string) bool {
	if m == nil {
		return false
	}
	m.mu.Lock()
	var gone []*slot
	for key, s := range m.slots {
		if key.id == id {
			gone = append(gone, s)
			delete(m.slots, key)
		}
	}
	m.mu.Unlock()
	alive := false
	for _, s := range gone {
		if k := s.k; k != nil {
			alive = alive || k.alive()
			k.kill()
		}
	}
	for _, kind := range []string{Python, Node} {
		m.unmark(id, kind)
	}
	return alive
}

// Close kills every kernel; the worker is stopping. The markers stay, so
// the next cell after a restart says kernel_restarted.
func (m *Manager) Close() {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.closed = true
	slots := m.slots
	m.slots = map[slotKey]*slot{}
	m.mu.Unlock()
	for _, s := range slots {
		if k := s.k; k != nil {
			k.kill()
		}
	}
}

// Clean removes the markers of workspaces not in keep: what a worker that
// was killed while removing one left.
func (m *Manager) Clean(keep map[string]bool) {
	if m == nil {
		return
	}
	ents, err := os.ReadDir(m.opt.MarkDir)
	if err != nil {
		return
	}
	for _, e := range ents {
		id, kind, ok := strings.Cut(e.Name(), markerSuffix)
		if ok && !e.IsDir() && Valid(kind) && !keep[id] {
			_ = os.Remove(filepath.Join(m.opt.MarkDir, e.Name()))
		}
	}
}

func (m *Manager) markerPath(id, kind string) string {
	return filepath.Join(m.opt.MarkDir, id+markerSuffix+kind)
}

func (m *Manager) marked(id, kind string) bool {
	_, err := os.Lstat(m.markerPath(id, kind))
	return err == nil
}

func (m *Manager) mark(id, kind string) error {
	return os.WriteFile(m.markerPath(id, kind), nil, 0o600)
}

func (m *Manager) unmark(id, kind string) { _ = os.Remove(m.markerPath(id, kind)) }

// startKernel starts the driver of req.Kind in req.Dir.
func (m *Manager) startKernel(ctx context.Context, req Request) (*proc, error) {
	d, err := m.driverPaths()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	var sr engine.StartRequest
	switch req.Kind {
	case Python:
		sr = engine.StartRequest{Argv: []string{pythonFor(req.Dir), "-I", d.Python}, Env: []string{"MPLBACKEND=Agg"}}
	case Node:
		sr = engine.StartRequest{Argv: []string{"node", d.Node}}
	}
	sr.Workspace, sr.Root, sr.Dir = req.Workspace, req.Dir, req.Dir
	k, err := start(ctx, m.opt.Engine, sr, m.opt.StartWait)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("%w: %s: %v", ErrUnavailable, req.Kind, err)
	}
	return k, nil
}

// venvPython is the workspace's own interpreter, relative to the workspace:
// the kernel starts in the workspace's root, so the same path reads on the
// host and in a session, where the workspace is mounted elsewhere.
const venvPython = "./.venv/bin/python"

// pythonFor is the interpreter a Python kernel runs: the workspace's own
// virtual environment when it has one, so what the agent installed there is
// importable (RFC GW §4.4), else python3 from PATH. Lstat, not Stat: a venv
// made in a session links to the session's python3, a path the worker's own
// disk may not have.
func pythonFor(dir string) string {
	if _, err := os.Lstat(filepath.Join(dir, ".venv", "bin", "python")); err == nil {
		return venvPython
	}
	return "python3"
}

func (m *Manager) driverPaths() (Drivers, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.drivers != nil {
		return *m.drivers, nil
	}
	if m.opt.DriverDir == "" {
		return Drivers{}, errors.New("this worker carries no kernel drivers")
	}
	d, err := WriteDrivers(m.opt.DriverDir)
	if err != nil {
		return Drivers{}, err
	}
	m.drivers = &d
	return d, nil
}
