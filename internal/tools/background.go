package tools

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
)

// The states a background child reports. Queued, running and held are
// outstanding; the rest are final. Idle is a resident child parked between
// turns.
const (
	ChildQueued    = "queued"
	ChildRunning   = "running"
	ChildHeld      = "held"
	ChildCompleted = "completed"
	ChildFailed    = "failed"
	ChildCancelled = "cancelled"
	ChildTimeout   = "timeout"
	ChildIdle      = "idle"
)

// errBackgroundClosed refuses a child started after its run stopped taking
// new ones: the run is ending, and nothing would collect the child.
var errBackgroundClosed = errors.New("this run is ending and starts no more background children")

// Background is one run's table of background children: the children its
// Agent spawn and parallel_spawn started in poll mode, which run on after the
// call that started them returns, and the resident children it opened, kept
// so that a poll or cancel naming one is recognised as this run's.
//
// A background child runs on a ctx that keeps the spawning call's values but
// not its cancellation — the call returns at once — and is cancelled instead
// when the run's lifetime ends or the parent cancels it. Its result is kept
// here until the parent reads it; nothing is ever dropped while the run lives.
//
// The loop makes one per run and carries it on the run's ctx (WithBackground).
// It is safe for concurrent use: children end on their own goroutines.
type Background struct {
	lifetime context.Context

	mu       sync.Mutex
	children []*bgChild // in the order they were started
	byID     map[string]*bgChild
	changed  chan struct{} // closed and replaced on every change
	closed   bool
}

type bgChild struct {
	ChildSpec
	state           string
	result          ChildResult
	cancel          context.CancelCauseFunc
	stop            func() bool // detaches the lifetime watch
	cancelRequested bool
	read            bool // a poll handed the parent its result
	noted           bool // a note told the parent it ended
}

// ChildSpec describes a child as it is started.
type ChildSpec struct {
	RunID string
	Agent string
	// Index is the child's place in its parallel_spawn; -1 for a spawn.
	Index   int
	BatchID string
	// Notify sends the parent a note when the child ends.
	Notify bool
	// CancelOnParentEnd cancels the child when the parent ends its turn, so
	// it never holds the run open.
	CancelOnParentEnd bool
	// Resident marks a child opened with Agent open: tracked for ownership
	// only, its state read from the resident registry when asked.
	Resident bool
}

// ChildResult is what a background child handed back when it ended.
type ChildResult struct {
	Output     string
	Error      string
	Structured map[string]any
	// Status is "timeout" for a child its timeout_ms stopped, as on a
	// parallel_spawn envelope row.
	Status string
}

// ChildView is a snapshot of one child.
type ChildView struct {
	ChildSpec
	State  string
	Result ChildResult
	Read   bool
}

// Ended reports whether the child is in a final state.
func (v ChildView) Ended() bool { return childEnded(v.State) }

func childEnded(state string) bool {
	switch state {
	case ChildCompleted, ChildFailed, ChildCancelled, ChildTimeout:
		return true
	}
	return false
}

// NewBackground returns a run's table. lifetime is the run's ctx: when it ends
// every background child is cancelled with its cause.
func NewBackground(lifetime context.Context) *Background {
	return &Background{lifetime: lifetime, byID: map[string]*bgChild{}, changed: make(chan struct{})}
}

type ctxKeyBackground struct{}

// WithBackground carries a run's table on ctx. nil clears an inherited one: a
// run that cannot host background children must not file its children in an
// ancestor's table.
func WithBackground(ctx context.Context, b *Background) context.Context {
	return context.WithValue(ctx, ctxKeyBackground{}, b)
}

// BackgroundOf is the calling run's table, or nil when it has none.
func BackgroundOf(ctx context.Context) *Background {
	b, _ := ctx.Value(ctxKeyBackground{}).(*Background)
	return b
}

// broadcastLocked wakes everything watching the table. Callers hold mu.
func (b *Background) broadcastLocked() {
	close(b.changed)
	b.changed = make(chan struct{})
}

// Start files a background child as queued and returns the ctx it runs on:
// ctx's values, cancelled when the run's lifetime ends or Cancel names it.
func (b *Background) Start(ctx context.Context, spec ChildSpec) (context.Context, error) {
	cctx, cancel := context.WithCancelCause(context.WithoutCancel(ctx))
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		cancel(errBackgroundClosed)
		return nil, errBackgroundClosed
	}
	c := &bgChild{ChildSpec: spec, state: ChildQueued, cancel: cancel}
	c.stop = context.AfterFunc(b.lifetime, func() { cancel(context.Cause(b.lifetime)) })
	b.children = append(b.children, c)
	b.byID[spec.RunID] = c
	b.broadcastLocked()
	return cctx, nil
}

// AddResident records a resident child this run opened.
func (b *Background) AddResident(runID, agent string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.byID[runID]; ok {
		return
	}
	c := &bgChild{ChildSpec: ChildSpec{RunID: runID, Agent: agent, Index: -1, Resident: true}}
	b.children = append(b.children, c)
	b.byID[runID] = c
}

// SetState moves an outstanding child between queued, running and held. A
// child that has ended keeps its final state.
func (b *Background) SetState(runID, state string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	c, ok := b.byID[runID]
	if !ok || c.Resident || childEnded(c.state) || c.state == state {
		return
	}
	c.state = state
	b.broadcastLocked()
}

// Finish records a child's end. Only the first call counts.
func (b *Background) Finish(runID, state string, res ChildResult) {
	b.mu.Lock()
	defer b.mu.Unlock()
	c, ok := b.byID[runID]
	if !ok || c.Resident || childEnded(c.state) {
		return
	}
	c.state, c.result = state, res
	c.stop()
	c.cancel(nil) // frees the ctx; the child's run has ended
	b.broadcastLocked()
}

// Cancel cancels an outstanding background child with cause. It reports
// whether runID is a background child of this run; one that has already ended
// is left as it ended.
func (b *Background) Cancel(runID string, cause error) bool {
	b.mu.Lock()
	c, ok := b.byID[runID]
	if !ok || c.Resident {
		b.mu.Unlock()
		return false
	}
	ended := childEnded(c.state)
	if !ended {
		c.cancelRequested = true
	}
	b.mu.Unlock()
	if !ended {
		c.cancel(cause)
	}
	return true
}

// EndTurn cancels the outstanding children started with CancelOnParentEnd:
// the parent has ended its turn, which is when they were to stop.
func (b *Background) EndTurn(cause error) {
	b.mu.Lock()
	var cancel []*bgChild
	for _, c := range b.children {
		if c.CancelOnParentEnd && !c.Resident && !childEnded(c.state) {
			c.cancelRequested = true
			cancel = append(cancel, c)
		}
	}
	b.mu.Unlock()
	for _, c := range cancel {
		c.cancel(cause)
	}
}

// Close stops the table taking new children and cancels every outstanding
// one with cause. The run calls it as it ends: a child it no longer waits for
// would run on with nobody to read it.
func (b *Background) Close(cause error) {
	b.mu.Lock()
	b.closed = true
	var cancel []*bgChild
	for _, c := range b.children {
		if !c.Resident && !childEnded(c.state) {
			cancel = append(cancel, c)
		}
	}
	b.mu.Unlock()
	for _, c := range cancel {
		c.cancel(cause)
	}
}

// Outstanding is the children that hold the run open — queued, running or
// held — and a channel closed at the table's next change. A child the parent's
// turn end is cancelling does not hold it: it was started not to.
func (b *Background) Outstanding() ([]ChildView, <-chan struct{}) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []ChildView
	for _, c := range b.children {
		if c.Resident || childEnded(c.state) || (c.CancelOnParentEnd && c.cancelRequested) {
			continue
		}
		out = append(out, c.viewLocked())
	}
	return out, b.changed
}

// Changed is a channel closed at the table's next change.
func (b *Background) Changed() <-chan struct{} {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.changed
}

func (c *bgChild) viewLocked() ChildView {
	return ChildView{ChildSpec: c.ChildSpec, State: c.state, Result: c.result, Read: c.read}
}

// Lookup is one child of this run.
func (b *Background) Lookup(runID string) (ChildView, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	c, ok := b.byID[runID]
	if !ok {
		return ChildView{}, false
	}
	return c.viewLocked(), true
}

// Select resolves what a poll names: the given ids (in that order), or every
// child of batchID, or — neither given — every background child whose result
// the parent has not read yet. unknown lists the ids and batch that are not
// this run's.
func (b *Background) Select(ids []string, batchID string) (out []ChildView, unknown []string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch {
	case len(ids) > 0:
		for _, id := range ids {
			if c, ok := b.byID[id]; ok {
				out = append(out, c.viewLocked())
			} else {
				unknown = append(unknown, id)
			}
		}
	case batchID != "":
		for _, c := range b.children {
			if c.BatchID == batchID {
				out = append(out, c.viewLocked())
			}
		}
		if len(out) == 0 {
			unknown = append(unknown, batchID)
		}
	default:
		for _, c := range b.children {
			if !c.Resident && !c.read {
				out = append(out, c.viewLocked())
			}
		}
	}
	return out, unknown
}

// MarkRead records that the parent has been handed these children's results.
func (b *Background) MarkRead(ids []string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, id := range ids {
		if c, ok := b.byID[id]; ok && childEnded(c.state) {
			c.read = true
		}
	}
}

// TakeNotes is the note for the children that ended since the last one —
// those started with Notify whose result the parent has not read — or "".
// Each child is noted once.
func (b *Background) TakeNotes() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var ended []*bgChild
	for _, c := range b.children {
		if c.Notify && !c.Resident && !c.noted && !c.read && childEnded(c.state) {
			c.noted = true
			ended = append(ended, c)
		}
	}
	switch len(ended) {
	case 0:
		return ""
	case 1:
		c := ended[0]
		return fmt.Sprintf("Background child %s (%s) finished: %s. Use Agent poll to read its result.", c.RunID, c.Agent, c.state)
	}
	return "Background children finished: " + listStates(ended) + ". Use Agent poll to read their results."
}

// WakeNote is the note a run woken from waiting on its children gets: the
// final state of each child it waited for, and of any other child that ended
// unread and was not yet noted.
func (b *Background) WakeNote(waitedFor []string) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var ended []*bgChild
	for _, c := range b.children {
		if c.Resident || !childEnded(c.state) {
			continue
		}
		if slices.Contains(waitedFor, c.RunID) || (!c.noted && !c.read) {
			c.noted = true
			ended = append(ended, c)
		}
	}
	return "Every background child you were waiting for has ended: " + listStates(ended) +
		". Use Agent poll to read their results."
}

func listStates(cs []*bgChild) string {
	parts := make([]string, len(cs))
	for i, c := range cs {
		parts[i] = fmt.Sprintf("%s (%s): %s", c.RunID, c.Agent, c.state)
	}
	return strings.Join(parts, "; ")
}
