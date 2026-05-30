package a2a

import (
	"context"
	"encoding/json"
	"sync"

	a2asdk "github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// InterruptResolver is the resume seam: the executor uses it to wake a
// run that parked on an Interruption.ask when a follow-up A2A
// message/send arrives for the same task. It mirrors the HTTP resolve
// endpoint's two-step shape (persist the answer, then notify the parked
// waiter) so A2A resume and HTTP resume converge on one mechanism.
//
// Injected (not the concrete Store + Bus) so the executor stays
// unit-testable: a test fake records the resolve call + flips a pending
// interrupt to resolved without a real bus.
type InterruptResolver interface {
	// PendingForRun returns the interrupt_id of the run's currently-
	// pending interruption, or ("", false) when the run has none. Used
	// to decide whether a same-taskId follow-up is a RESUME (pending
	// interrupt exists) or should start a fresh run (none).
	PendingForRun(ctx context.Context, runID string) (interruptID string, ok bool)
	// Resolve records answer against the interrupt and wakes the parked
	// run (Store.InterruptResolve + Bus.Notify). Returns an error when
	// the interrupt could not be resolved (already terminal, missing).
	Resolve(ctx context.Context, interruptID, answer string) error
}

// storeInterruptResolver is the production InterruptResolver backed by
// the run store + the channels notification bus. notify must be the same
// Bus the Interruption tool's blockWithHeartbeat waits on, keyed by
// "intr:<id>" — otherwise the parked run never wakes.
type storeInterruptResolver struct {
	store  InterruptStore
	notify func(busKey string)
}

// InterruptStore is the narrow store surface the resolver needs. Exported
// so the A2A server package (internal/api/a2a) can compose it into the
// store interface it requires of its caller. store.Store satisfies it.
type InterruptStore interface {
	InterruptListByRun(ctx context.Context, runID, statusFilter string) ([]store.InterruptRow, error)
	InterruptResolve(ctx context.Context, interruptID, answer, resolvedBy string, answerMeta json.RawMessage) error
}

// NewInterruptResolver builds the production resolver. notify is
// typically channelsBus.Notify; store is the loomcycle Store.
func NewInterruptResolver(st InterruptStore, notify func(busKey string)) InterruptResolver {
	return &storeInterruptResolver{store: st, notify: notify}
}

func (r *storeInterruptResolver) PendingForRun(ctx context.Context, runID string) (string, bool) {
	rows, err := r.store.InterruptListByRun(ctx, runID, store.InterruptStatusPending)
	if err != nil || len(rows) == 0 {
		return "", false
	}
	// A run parks on one interruption at a time (the Interruption tool
	// blocks the loop), so the first pending row is the one to resolve.
	return rows[0].InterruptID, true
}

func (r *storeInterruptResolver) Resolve(ctx context.Context, interruptID, answer string) error {
	// context.WithoutCancel: the resolve must complete even though the
	// triggering A2A request ctx may be torn down right after — same
	// posture as the HTTP resolve handler and the Interruption tool.
	if err := r.store.InterruptResolve(context.WithoutCancel(ctx), interruptID, answer, store.InterruptResolvedByAPI, nil); err != nil {
		return err
	}
	if r.notify != nil {
		r.notify("intr:" + interruptID)
	}
	return nil
}

// parkedRun is a run that emitted an interruption and is blocked on the
// bus awaiting input. The executor keeps it in parkRegistry so a
// same-task follow-up message can re-attach to the SAME run's event
// stream instead of starting a new one.
//
// A dedicated drain goroutine (started in Executor.Execute) owns the
// underlying run's OnEvent channel and forwards events to `out`. That
// goroutine stays alive across the park so the run can continue after
// resume without the events backing up — the first Execute returns to
// the A2A client but the run keeps running in the background.
type parkedRun struct {
	out  <-chan providers.Event // forwarded run events (post-park included)
	done <-chan struct{}        // closed when the run's RunOnce returns
	// runErrPtr is read only after done is closed (happens-before via
	// channel close), so no additional synchronisation is needed.
	runErrPtr *error
	agentID   string
	runID     string
	// cancel stops the detached background run. The run's lifetime is
	// deliberately decoupled from the per-request ctx (see startRun), so
	// this is the ONLY way to tear a parked run down before it completes —
	// used when a park can never be resumed (no bridge) or the client
	// abandons the stream at the park.
	cancel context.CancelFunc
}

// parkRegistry tracks runs parked on an interruption, keyed by A2A
// TaskID. Process-local: cross-replica resume is a later-slice concern
// (it would ride the shared run table + backplane bus), same boundary as
// TaskStore's in-memory map.
type parkRegistry struct {
	mu     sync.Mutex
	parked map[a2asdk.TaskID]*parkedRun
}

func newParkRegistry() *parkRegistry {
	return &parkRegistry{parked: make(map[a2asdk.TaskID]*parkedRun)}
}

func (r *parkRegistry) put(id a2asdk.TaskID, p *parkedRun) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.parked[id] = p
}

// take removes and returns the parked run for id, if any. Removing on
// take ensures a resume consumes the entry exactly once; if the resumed
// run parks again it re-registers a fresh entry.
func (r *parkRegistry) take(id a2asdk.TaskID) (*parkedRun, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.parked[id]
	if ok {
		delete(r.parked, id)
	}
	return p, ok
}
