// Package dispatch is the core's side of the conversion worker pool (RFC GO
// §5.4, §5.5): the WorkerService the workers attach to, the queue of their
// idle slots, and the Pool client that hands a conversion to one of them.
//
// Workers ask for work; the dispatcher never dials a worker. A worker with a
// free slot holds an open Work stream, so the pool's free capacity is exactly
// the idle streams the dispatcher holds: there is no health probe and no
// address list to keep in step.
package dispatch

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/denn-gubsky/loomcycle/internal/conversion/wire"
	"github.com/denn-gubsky/loomcycle/internal/conversion/workerpb"
	"github.com/denn-gubsky/loomcycle/internal/convert"
	"github.com/denn-gubsky/loomcycle/internal/safego"
)

// Defaults for Options left zero.
const (
	DefaultQueueWait        = 60 * time.Second
	DefaultQuarantineTTL    = time.Hour
	DefaultJobTimeout       = 60 * time.Second
	DefaultHeartbeatTimeout = 15 * time.Second
	// acceptWait bounds how long a worker may take to accept a job it was
	// handed. Nothing of the file has been sent by then, so a worker that
	// does not answer costs a retry, never a quarantine.
	acceptWait = 30 * time.Second
	// deadlineGrace is how long past a job's time bound the core waits for
	// the worker's own Done before it stops waiting.
	deadlineGrace  = 10 * time.Second
	maxWorkerSlots = 64
	maxWorkerID    = 128
)

// Options configure a Dispatcher.
type Options struct {
	// Token is the shared secret every worker presents (RFC GO D5). Empty
	// refuses every worker.
	Token string
	// BuildVersion is this binary's version; a worker of another build is
	// admitted with a warning (D7).
	BuildVersion string
	// QueueWait is the longest a job waits for an idle slot before it is
	// answered as a busy server.
	QueueWait time.Duration
	// QuarantineTTL is how long a file that crashed a worker is refused.
	QuarantineTTL time.Duration
	// JobTimeout bounds one job, counted from when a worker accepts it.
	JobTimeout time.Duration
	// KindTimeouts bound the jobs of a kind in place of JobTimeout: a helper
	// job runs under its helper's own setting (transcription.stt.timeout_s,
	// speech.timeout_s), which the worker is told with the job.
	KindTimeouts map[workerpb.JobKind]time.Duration
	// Scratch is the directory a converted recording is spooled to in the
	// core; empty is the system temp directory.
	Scratch string
	// HeartbeatTimeout drops a worker that has sent nothing for this long
	// (three missed heartbeats).
	HeartbeatTimeout time.Duration
	Logger           *slog.Logger
}

// Dispatcher is the worker endpoint and the pool behind it.
type Dispatcher struct {
	workerpb.UnimplementedWorkerServiceServer

	opt  Options
	log  *slog.Logger
	srv  *grpc.Server
	jobs atomic.Uint64 // job ids

	mu         sync.Mutex
	workers    map[string]*worker
	idle       []*slot // oldest idle first
	waiters    []*waiter
	inFlight   int
	quarantine map[string]time.Time
	outcomes   map[string]uint64
	deaths     uint64
	respawns   atomic.Uint64
}

type worker struct {
	id     string
	slots  int
	labels map[string]bool
	open   map[*slot]struct{}
}

// slot is one Work stream: an idle slot of an attached worker until a job
// is handed to it.
type slot struct {
	w *worker
	// jobs is buffered 1: handing a job never blocks. A nil job tells a
	// handler whose stream already ended that the slot taken from it was
	// given back unused.
	jobs   chan *job
	taken  bool
	ctx    context.Context
	cancel context.CancelFunc
}

type waiter struct {
	label string
	// ch receives the slot handed to this waiter, or nil when no attached
	// worker can run its job any more.
	ch chan *slot
}

// New builds a dispatcher.
func New(opt Options) *Dispatcher {
	if opt.QueueWait <= 0 {
		opt.QueueWait = DefaultQueueWait
	}
	if opt.QuarantineTTL <= 0 {
		opt.QuarantineTTL = DefaultQuarantineTTL
	}
	if opt.JobTimeout <= 0 {
		opt.JobTimeout = DefaultJobTimeout
	}
	if opt.HeartbeatTimeout <= 0 {
		opt.HeartbeatTimeout = DefaultHeartbeatTimeout
	}
	log := opt.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Dispatcher{opt: opt, log: log.With("component", "conversion-dispatcher"),
		workers: map[string]*worker{}, quarantine: map[string]time.Time{}, outcomes: map[string]uint64{}}
}

// Serve serves the worker endpoint on l until Stop. It serves the two worker
// RPCs and nothing else.
func (d *Dispatcher) Serve(l net.Listener) error {
	d.mu.Lock()
	if d.srv == nil {
		d.srv = grpc.NewServer()
		workerpb.RegisterWorkerServiceServer(d.srv, d)
	}
	srv := d.srv
	d.mu.Unlock()
	return srv.Serve(l)
}

// Stop ends every worker stream and the listener.
func (d *Dispatcher) Stop() {
	d.mu.Lock()
	srv := d.srv
	d.mu.Unlock()
	if srv != nil {
		srv.Stop()
	}
}

// AddRespawn counts a worker the supervisor started again, for /metrics.
func (d *Dispatcher) AddRespawn() { d.respawns.Add(1) }

// authorize checks the worker's bearer in constant time.
func (d *Dispatcher) authorize(ctx context.Context) error {
	if d.opt.Token == "" {
		return status.Error(codes.Unauthenticated, "no worker token is configured")
	}
	md, _ := metadata.FromIncomingContext(ctx)
	got := ""
	if v := md.Get(wire.MDAuthorization); len(v) == 1 {
		got = v[0]
	}
	want := "Bearer " + d.opt.Token
	if subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
		return status.Error(codes.Unauthenticated, "bad worker token")
	}
	return nil
}

// ---- Attach ----

// Attach registers a worker for as long as the stream lives and it keeps
// heartbeating. When it ends, the worker's idle slots are dropped, and a job
// running on one of them fails as the worker's death.
func (d *Dispatcher) Attach(stream workerpb.WorkerService_AttachServer) error {
	if err := d.authorize(stream.Context()); err != nil {
		return err
	}
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	hello := first.GetHello()
	if hello == nil {
		return status.Error(codes.InvalidArgument, "the first Attach frame must be Hello")
	}
	if hello.GetProtocolVersion() != wire.ProtocolVersion {
		return status.Errorf(codes.FailedPrecondition, "this dispatcher speaks worker protocol %d, the worker %d: run the same loomcycle release on both", wire.ProtocolVersion, hello.GetProtocolVersion())
	}
	id := hello.GetWorkerId()
	if id == "" || len(id) > maxWorkerID {
		return status.Error(codes.InvalidArgument, "a worker id of 1 to 128 bytes is required")
	}
	if b := hello.GetBuildVersion(); b != d.opt.BuildVersion {
		d.log.Warn("conversion worker runs another build", "worker_id", id, "worker_build", b, "build", d.opt.BuildVersion)
	}
	w := &worker{id: id, slots: min(max(int(hello.GetSlots()), 1), maxWorkerSlots), labels: map[string]bool{}, open: map[*slot]struct{}{}}
	for _, l := range hello.GetLabels() {
		w.labels[l] = true
	}
	d.mu.Lock()
	if _, dup := d.workers[id]; dup {
		d.mu.Unlock()
		return status.Errorf(codes.AlreadyExists, "a worker named %q is already attached", id)
	}
	d.workers[id] = w
	d.mu.Unlock()
	d.log.Info("conversion worker attached", "worker_id", id, "slots", w.slots, "labels", hello.GetLabels())
	defer d.detach(w)

	if err := stream.Send(&workerpb.DispatcherControl{Msg: &workerpb.DispatcherControl_Welcome{Welcome: &workerpb.Welcome{BuildVersion: d.opt.BuildVersion}}}); err != nil {
		return err
	}
	beats := make(chan struct{}, 1)
	errc := make(chan error, 1)
	safego.Go(stream.Context(), "conversion.dispatch.attach", func() {
		for {
			if _, err := stream.Recv(); err != nil {
				errc <- err
				return
			}
			select {
			case beats <- struct{}{}:
			default:
			}
		}
	})
	t := time.NewTimer(d.opt.HeartbeatTimeout)
	defer t.Stop()
	for {
		select {
		case <-beats:
			t.Reset(d.opt.HeartbeatTimeout)
		case err := <-errc:
			d.log.Info("conversion worker detached", "worker_id", id, "err", err)
			return nil
		case <-t.C:
			d.log.Warn("conversion worker missed its heartbeats; dropping it", "worker_id", id, "after", d.opt.HeartbeatTimeout)
			d.mu.Lock()
			d.deaths++
			d.mu.Unlock()
			return status.Error(codes.DeadlineExceeded, "missed heartbeats")
		case <-stream.Context().Done():
			return nil
		}
	}
}

// detach removes a worker and ends its Work streams. Waiters whose job no
// remaining worker can run are told so at once rather than left to time out.
func (d *Dispatcher) detach(w *worker) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.workers[w.id] == w {
		delete(d.workers, w.id)
	}
	for s := range w.open {
		s.cancel()
	}
	kept := d.waiters[:0]
	for _, wt := range d.waiters {
		if d.canRunLocked(wt.label) {
			kept = append(kept, wt)
			continue
		}
		wt.ch <- nil
	}
	d.waiters = kept
}

func (d *Dispatcher) canRunLocked(label string) bool {
	for _, w := range d.workers {
		if w.labels[label] {
			return true
		}
	}
	return false
}

// ---- Work ----

// Work holds one idle slot of an attached worker until a job is handed to it,
// runs that job on the stream, and ends.
func (d *Dispatcher) Work(stream workerpb.WorkerService_WorkServer) error {
	if err := d.authorize(stream.Context()); err != nil {
		return err
	}
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	ready := first.GetReady()
	if ready == nil {
		return status.Error(codes.InvalidArgument, "the first Work frame must be Ready")
	}
	ctx, cancel := context.WithCancel(stream.Context())
	defer cancel()
	s := &slot{jobs: make(chan *job, 1), ctx: ctx, cancel: cancel}
	d.mu.Lock()
	w := d.workers[ready.GetWorkerId()]
	if w == nil {
		d.mu.Unlock()
		return status.Error(codes.FailedPrecondition, "attach before opening a work slot")
	}
	s.w = w
	w.open[s] = struct{}{}
	d.offerLocked(s)
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		delete(w.open, s)
		d.dropIdleLocked(s)
		d.mu.Unlock()
	}()

	select {
	case j := <-s.jobs:
		if j == nil {
			return ctx.Err()
		}
		j.done <- d.serve(ctx, stream, s, j)
		return nil
	case <-ctx.Done():
		d.mu.Lock()
		taken := s.taken
		d.dropIdleLocked(s)
		d.mu.Unlock()
		if taken {
			// A job was handed to this slot as it went away: nothing of it
			// reached the worker, so it is retried elsewhere.
			if j := <-s.jobs; j != nil {
				j.done <- result{retry: true}
			}
		}
		return ctx.Err()
	}
}

// offerLocked hands an idle slot to the first waiter whose job it can run, or
// keeps it idle.
func (d *Dispatcher) offerLocked(s *slot) {
	for i, wt := range d.waiters {
		if s.w.labels[wt.label] {
			d.waiters = append(d.waiters[:i], d.waiters[i+1:]...)
			s.taken = true
			wt.ch <- s
			return
		}
	}
	d.idle = append(d.idle, s)
}

func (d *Dispatcher) dropIdleLocked(s *slot) {
	for i, x := range d.idle {
		if x == s {
			d.idle = append(d.idle[:i], d.idle[i+1:]...)
			return
		}
	}
}

// timeoutFor is the time bound of a job of kind.
func (d *Dispatcher) timeoutFor(kind workerpb.JobKind) time.Duration {
	if t := d.opt.KindTimeouts[kind]; t > 0 {
		return t
	}
	return d.opt.JobTimeout
}

// errNoSlot is the wait for an idle slot running out.
var errNoSlot = fmt.Errorf("%w: every conversion worker stayed busy", convert.ErrBusy)

// acquire takes the oldest idle slot that can run a job needing label,
// waiting up to QueueWait. Waiters are served first come, first served (D9).
func (d *Dispatcher) acquire(ctx context.Context, label string) (*slot, error) {
	d.mu.Lock()
	if !d.canRunLocked(label) {
		attached := len(d.workers) > 0
		d.mu.Unlock()
		if attached && wire.HelperLabel(label) {
			// Workers are up, and none has this helper's program: a setting
			// to fix on the workers, not a pool that is down.
			return nil, fmt.Errorf("%w: no attached conversion worker has the %s helper", convert.ErrNotConfigured, label)
		}
		return nil, convert.ErrNoWorkers
	}
	for i, s := range d.idle {
		if s.w.labels[label] {
			d.idle = append(d.idle[:i], d.idle[i+1:]...)
			s.taken = true
			d.mu.Unlock()
			return s, nil
		}
	}
	wt := &waiter{label: label, ch: make(chan *slot, 1)}
	d.waiters = append(d.waiters, wt)
	d.mu.Unlock()

	t := time.NewTimer(d.opt.QueueWait)
	defer t.Stop()
	var cause error
	select {
	case s := <-wt.ch:
		if s == nil {
			return nil, convert.ErrNoWorkers
		}
		return s, nil
	case <-ctx.Done():
		cause = ctx.Err()
	case <-t.C:
		cause = errNoSlot
	}
	d.mu.Lock()
	for i, x := range d.waiters {
		if x == wt {
			d.waiters = append(d.waiters[:i], d.waiters[i+1:]...)
			d.mu.Unlock()
			if errors.Is(cause, errNoSlot) {
				return nil, errNoSlot
			}
			return nil, fmt.Errorf("%w: %w", convert.ErrBusy, cause)
		}
	}
	d.mu.Unlock()
	// A slot (or the news that none is left) was handed over as the wait
	// ended: give a slot back rather than lose it.
	if s := <-wt.ch; s != nil {
		d.mu.Lock()
		if s.ctx.Err() != nil {
			// Its stream is gone; its handler may already be waiting for
			// the job it was promised.
			d.mu.Unlock()
			s.jobs <- nil
		} else {
			s.taken = false
			d.offerLocked(s)
			d.mu.Unlock()
		}
	}
	if errors.Is(cause, errNoSlot) {
		return nil, errNoSlot
	}
	return nil, fmt.Errorf("%w: %w", convert.ErrBusy, cause)
}

// ---- quarantine and stats ----

func (d *Dispatcher) quarantined(key string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	until, ok := d.quarantine[key]
	if !ok {
		return false
	}
	if time.Now().After(until) {
		delete(d.quarantine, key)
		return false
	}
	return true
}

func (d *Dispatcher) quarantineFor(key string) {
	d.mu.Lock()
	d.quarantine[key] = time.Now().Add(d.opt.QuarantineTTL)
	d.mu.Unlock()
}

func (d *Dispatcher) count(kind, outcome string, died bool) {
	d.mu.Lock()
	d.outcomes[kind+"/"+outcome]++
	if died {
		d.deaths++
	}
	d.mu.Unlock()
}

// Stats reports the pool for /metrics.
func (d *Dispatcher) Stats() convert.PoolStats {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := time.Now()
	for k, until := range d.quarantine {
		if now.After(until) {
			delete(d.quarantine, k)
		}
	}
	st := convert.PoolStats{Workers: len(d.workers), Idle: len(d.idle), InFlight: d.inFlight, Waiting: len(d.waiters),
		Quarantined: len(d.quarantine), Jobs: make(map[string]uint64, len(d.outcomes)), Deaths: d.deaths, Respawns: d.respawns.Load()}
	for _, w := range d.workers {
		st.Slots += w.slots
	}
	for k, v := range d.outcomes {
		st.Jobs[k] = v
	}
	return st
}

// ExtractionStats feeds the existing extraction gauges from the pool: its
// slots, the jobs running and the jobs waiting.
func (d *Dispatcher) ExtractionStats() (limit, inFlight, waiting int) {
	st := d.Stats()
	return st.Slots, st.InFlight, st.Waiting
}
