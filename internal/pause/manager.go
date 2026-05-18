package pause

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// DefaultPauseTimeout is the wait-for-non-idempotent-tools cap when the
// operator omits timeout_ms from POST /v1/runtime/pause. 30 s matches
// the longest reasonable non-idempotent tool call (HTTP POST + MCP
// chained call) and keeps the operator's pause-then-snapshot cycle
// completing inside a minute on the worst common case.
const DefaultPauseTimeout = 30 * time.Second

// MaxPauseTimeout caps what an operator can request. Without an upper
// bound, an operator typo (300000 vs 30000) leaves the runtime in
// StatePausing for 5 minutes, during which new runs all see 503. The
// 5-minute ceiling is generous — most tools complete in under 30 s.
const MaxPauseTimeout = 5 * time.Minute

// ErrAlreadyPausing is returned when Pause is called while the manager
// is already in StatePausing or StatePaused. Idempotent for the caller
// (the operator can retry POST /v1/runtime/pause without worrying
// about state) but the second call returns this distinguished error so
// HTTP handlers can return 409 rather than 200.
var ErrAlreadyPausing = errors.New("pause: runtime is already pausing or paused")

// ErrNotPaused is returned when Resume is called while the manager is
// in StateRunning. Symmetric with ErrAlreadyPausing.
var ErrNotPaused = errors.New("pause: runtime is not paused")

// PauseResult is the payload POST /v1/runtime/pause returns. The
// duration the wind-down took and the count of force-cancelled tool
// calls are the operator-visible quality signals: a long duration
// suggests slow tools; a non-zero force-cancelled count suggests
// timeout was too short (or tools mis-categorised as non-idempotent).
type PauseResult struct {
	State               string   `json:"state"`
	DurationMs          int64    `json:"duration_ms"`
	ForceCancelledCount int      `json:"force_cancelled_count"`
	PausedRunsCount     int      `json:"paused_runs_count"`
	Warnings            []string `json:"warnings,omitempty"`
}

// ResumeResult is the payload POST /v1/runtime/resume returns. After
// resume, the manager re-marks each previously-paused run as running
// in the store; the actual loop re-entry happens when the runner's
// goroutine for that run picks up the broadcast.
type ResumeResult struct {
	State            string   `json:"state"`
	ResumedRunsCount int      `json:"resumed_runs_count"`
	Warnings         []string `json:"warnings,omitempty"`
}

// StateSnapshot is the payload GET /v1/runtime/state returns. Cheap to
// compute — atomic load on the state, single SELECT for paused runs.
type StateSnapshot struct {
	State           string `json:"state"`
	PausedRunsCount int    `json:"paused_runs_count"`
}

// Manager is the single per-server pause/resume coordinator. Owns the
// process-local atomic state, the broadcast channel the loop watches
// at iteration boundaries, and the in-flight-tool tracking used by
// ToolCtx to apply per-tool cancel timeouts.
//
// Concurrency: state load + PauseCh read are lock-free (atomic + chan
// receive). Pause / Resume serialise on `mu` to keep transitions
// linear. The in-flight tool registry uses a sync.Map so per-tool
// registration during dispatch doesn't contend with state transitions.
type Manager struct {
	state atomic.Int32

	// mu serialises Pause/Resume so the state transition + DB
	// checkpoint + broadcast-channel swap are a single critical
	// section.
	mu sync.Mutex

	// pauseCh is closed when pause is declared, signalling the
	// boundary check in loop.Run. On resume, a fresh channel is
	// allocated so the next pause has a clean signal.
	pauseCh chan struct{}

	// store is used to persist runs.pause_state and to list paused
	// runs on resume. Manager treats the store as the source of
	// truth — process-local atomic is the fast path; DB is the
	// recovery / multi-replica-future path.
	store store.Store

	// activeTools tracks in-flight tool calls so Pause can iterate
	// them and apply per-category cancel policy. Key: a per-call
	// id (UUID-ish); value: *toolEntry.
	activeTools sync.Map

	// timeout is the configured wait-for-non-idempotent-tools cap.
	// Per-pause-call value overrides this if the operator supplies
	// timeout_ms; this is the default used when omitted.
	defaultTimeout time.Duration
}

// toolEntry holds the per-tool-call cancellation handle. Manager-
// owned; created in ToolCtx, removed when the tool's goroutine exits.
type toolEntry struct {
	cancel   context.CancelFunc
	category ToolCategory
	toolName string
	id       string
}

// NewManager constructs a Manager wired to the given store. The
// initial state is StateRunning; no pause is in flight. timeout is the
// default wait-for-non-idempotent-tools cap when POST /v1/runtime/pause
// omits timeout_ms; pass 0 to use DefaultPauseTimeout.
func NewManager(s store.Store, timeout time.Duration) *Manager {
	if timeout <= 0 {
		timeout = DefaultPauseTimeout
	}
	if timeout > MaxPauseTimeout {
		timeout = MaxPauseTimeout
	}
	m := &Manager{
		store:          s,
		pauseCh:        make(chan struct{}),
		defaultTimeout: timeout,
	}
	m.state.Store(int32(StateRunning))
	return m
}

// State returns the current RuntimeState. Lock-free atomic read; safe
// to call from any goroutine including the loop's hot path.
func (m *Manager) State() RuntimeState {
	if m == nil {
		return StateRunning
	}
	return loadState(&m.state)
}

// PauseCh returns the channel the loop's iteration-boundary check
// selects on. The channel is closed when pause is declared; old
// callers holding the channel see the close. After resume, a new
// channel is allocated; old captures see the OLD channel which
// remains closed (loops that started before resume see the pause
// signal). Always returns a non-nil channel.
//
// The receive operation `<-m.PauseCh()` is the hot-path check; it
// returns immediately (the channel is closed) when pause is declared
// and blocks otherwise. The loop wraps it in a non-blocking select
// (`case <-pauseCh: ... default: ...`) so unrelated runs aren't
// suspended.
func (m *Manager) PauseCh() <-chan struct{} {
	if m == nil {
		return nil // nil channel blocks forever; safe for old callers
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.pauseCh
}

// ToolCtx derives a per-tool-call context with the right cancellation
// policy for the current runtime state. Called from inside the loop's
// executePendingTools fan-out, ONCE per pending tool, before the
// tool's Execute() is invoked.
//
// When state is StateRunning: returns the parent ctx unchanged + a
// no-op cleanup; tool execution proceeds normally.
//
// When state is StatePausing / StatePaused: the tool's category
// (CategoryForInput) drives the policy:
//
//   - Idempotent: parent ctx is wrapped with WithCancel and the
//     cancel is invoked IMMEDIATELY. The tool's Execute sees a
//     cancelled ctx; the dispatcher returns IsError=true; the loop's
//     iteration boundary records pause_state='paused'.
//   - Non-idempotent / External: parent ctx is wrapped with
//     WithTimeout(timeout). Tool completes naturally if it's fast
//     enough; force-cancel at deadline. Count of force-cancels is
//     surfaced in PauseResult.
//
// Cleanup() must be called when the tool's goroutine exits, whether
// by completion or by force-cancel. Manager tracks the active entry
// until Cleanup runs.
//
// The toolID parameter is the loop-issued tool_use_id; it's the key
// in the activeTools registry so Pause can iterate.
func (m *Manager) ToolCtx(parent context.Context, toolID, toolName string, input json.RawMessage) (ctx context.Context, cleanup func()) {
	if m == nil {
		return parent, func() {}
	}
	state := m.State()
	cat := CategoryForInput(toolName, input)

	switch state {
	case StateRunning:
		// Track the entry so a pause that arrives mid-flight can
		// find it. Use WithCancel so a transition from running →
		// pausing can call cancel for idempotent tools.
		c, cancel := context.WithCancel(parent)
		id := toolID
		if id == "" {
			id = fmt.Sprintf("tool-%d-%p", time.Now().UnixNano(), &cancel)
		}
		entry := &toolEntry{
			cancel:   cancel,
			category: cat,
			toolName: toolName,
			id:       id,
		}
		m.activeTools.Store(id, entry)
		return c, func() {
			m.activeTools.Delete(id)
			cancel()
		}

	case StatePausing, StatePaused:
		// Pause is already declared. New tool dispatch within an
		// already-pausing run is unusual but handle defensively:
		// idempotent → cancel immediately; non-idempotent → apply
		// the timeout right away.
		if cat == CategoryIdempotent {
			c, cancel := context.WithCancel(parent)
			cancel() // immediate
			return c, func() {}
		}
		// Non-idempotent / external: register the entry so the
		// drain loop / post-drain sweep in Pause can find and
		// force-cancel it at the deadline. Without this Store,
		// late-registering tools would escape the pause barrier.
		c, cancel := context.WithTimeout(parent, m.defaultTimeout)
		id := toolID
		if id == "" {
			id = fmt.Sprintf("tool-%d-%p", time.Now().UnixNano(), &cancel)
		}
		entry := &toolEntry{
			cancel:   cancel,
			category: cat,
			toolName: toolName,
			id:       id,
		}
		m.activeTools.Store(id, entry)
		return c, func() {
			m.activeTools.Delete(id)
			cancel()
		}

	default:
		return parent, func() {}
	}
}

// Pause transitions the manager from StateRunning → StatePausing →
// StatePaused. Closes the pause broadcast channel (waking the loop's
// boundary check), iterates in-flight tools applying category policy,
// then waits for all activeTools entries to drain or for the deadline.
//
// Returns ErrAlreadyPausing when the manager is not in StateRunning.
//
// timeout (the operator-supplied or DefaultPauseTimeout) bounds two
// independent wait stages: the per-tool deadline applied via ToolCtx,
// and the overall stage-2 wait for activeTools to drain. The same
// number is reused because they're observationally the same
// constraint: "give tools this long to finish."
//
// Force-cancelled count: every non-idempotent / external tool whose
// goroutine didn't clean up before the deadline is force-cancelled
// (manager calls its stored cancel func) and counts toward the
// returned ForceCancelledCount.
func (m *Manager) Pause(ctx context.Context, timeout time.Duration) (PauseResult, error) {
	if m == nil {
		return PauseResult{}, errors.New("pause: nil Manager")
	}
	start := time.Now()
	if timeout <= 0 {
		timeout = m.defaultTimeout
	}
	if timeout > MaxPauseTimeout {
		timeout = MaxPauseTimeout
	}

	m.mu.Lock()
	if m.State() != StateRunning {
		m.mu.Unlock()
		return PauseResult{State: m.State().String()}, ErrAlreadyPausing
	}
	m.state.Store(int32(StatePausing))
	// Close the broadcast channel under the lock so concurrent
	// PauseCh() callers observe a consistent (state, channel) pair.
	close(m.pauseCh)
	m.mu.Unlock()

	// Apply per-tool cancel policy. Idempotent → cancel immediately;
	// non-idempotent / external → leave running, deadline applied
	// implicitly via the goroutine's own ctx (already wrapped by
	// ToolCtx with WithTimeout). For runs already in flight whose
	// ToolCtx fired BEFORE pause was declared, we apply category
	// policy here defensively.
	var (
		forceCancel     int
		idempotentCount int
	)
	m.activeTools.Range(func(_, v any) bool {
		e := v.(*toolEntry)
		if e.category == CategoryIdempotent {
			e.cancel()
			idempotentCount++
		}
		return true
	})

	// Stage 2: wait for activeTools to drain or hit the deadline.
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()

	for {
		empty := true
		m.activeTools.Range(func(_, _ any) bool {
			empty = false
			return false
		})
		if empty {
			break
		}
		select {
		case <-ctx.Done():
			// Caller cancelled the pause request; mark anything
			// still in flight as force-cancelled, transition to
			// StatePaused, return the partial result.
			forceCancel += m.forceCancelRemaining()
			return m.finalizePause(start, &forceCancel, idempotentCount, []string{
				fmt.Sprintf("pause request cancelled by caller: %v", ctx.Err()),
			}), nil
		case <-deadline.C:
			forceCancel += m.forceCancelRemaining()
			break
		case <-tick.C:
			continue
		}
		// deadline path falls through; break above only breaks the
		// select, so we explicitly exit the for-loop here.
		break
	}

	return m.finalizePause(start, &forceCancel, idempotentCount, nil), nil
}

// forceCancelRemaining cancels every entry still in activeTools and
// removes them from the registry. Returns the number of entries it
// cancelled. Called from the deadline / caller-cancellation paths in
// Pause and from the post-drain sweep in finalizePause. Single-
// goroutine-call discipline: callers must not invoke it concurrently
// with another forceCancelRemaining; sync.Map's per-key atomicity
// covers concurrent ToolCtx writers.
func (m *Manager) forceCancelRemaining() int {
	count := 0
	m.activeTools.Range(func(k, v any) bool {
		e := v.(*toolEntry)
		e.cancel()
		count++
		m.activeTools.Delete(k)
		return true
	})
	return count
}

// finalizePause transitions to StatePaused, queries the count of
// runs that committed pause_state='paused' to the DB, and assembles
// the PauseResult payload. forceCancel is a pointer so the post-drain
// sweep (which closes a race where a tool registers AFTER the drain
// loop's empty check but BEFORE we transition to StatePaused) can add
// any late-arrivers to the count surfaced in PauseResult.
func (m *Manager) finalizePause(start time.Time, forceCancel *int, idempotentCount int, warnings []string) PauseResult {
	// Post-drain sweep: a tool can register via ToolCtx after the
	// drain loop's empty check passed but before we transition to
	// StatePaused below. The transition makes the StatePausing-state
	// race window indistinguishable from StatePaused for ToolCtx, but
	// any goroutine already past its state-check at this point can
	// still call Store. Sweep here so the registry is empty at
	// StatePaused — operators relying on PausedRunsCount as a
	// quiescence signal need this guarantee.
	*forceCancel += m.forceCancelRemaining()

	m.state.Store(int32(StatePaused))

	paused, err := m.store.ListPausedRuns(context.Background())
	pausedCount := 0
	if err != nil {
		log.Printf("pause: ListPausedRuns failed during finalize: %v", err)
		warnings = append(warnings, "could not enumerate paused runs (state still transitioned to paused)")
	} else {
		pausedCount = len(paused)
	}

	if idempotentCount > 0 {
		log.Printf("pause: cancelled %d idempotent tools immediately", idempotentCount)
	}
	if *forceCancel > 0 {
		log.Printf("pause: force-cancelled %d tools at deadline", *forceCancel)
	}

	return PauseResult{
		State:               StatePaused.String(),
		DurationMs:          time.Since(start).Milliseconds(),
		ForceCancelledCount: *forceCancel,
		PausedRunsCount:     pausedCount,
		Warnings:            warnings,
	}
}

// Resume transitions StatePaused → StateRunning. Allocates a fresh
// pauseCh so the next Pause has a clean signal; flips every
// pause_state='paused' row back to 'running' in the store. Returns
// ErrNotPaused when the manager is in StateRunning or StatePausing.
//
// Resume is intentionally CHEAP: the actual loop re-entry happens
// when each run's goroutine wakes up and observes the new state. The
// resume call's job is only to clear the brakes; it doesn't drive
// the runs forward.
func (m *Manager) Resume(ctx context.Context) (ResumeResult, error) {
	if m == nil {
		return ResumeResult{}, errors.New("resume: nil Manager")
	}
	m.mu.Lock()
	if m.State() != StatePaused {
		st := m.State().String()
		m.mu.Unlock()
		return ResumeResult{State: st}, ErrNotPaused
	}
	// New broadcast channel for the next pause cycle. Old callers
	// holding the closed channel from the prior pause naturally fall
	// through their select-default branch on the next iteration.
	m.pauseCh = make(chan struct{})
	m.state.Store(int32(StateRunning))
	m.mu.Unlock()

	paused, err := m.store.ListPausedRuns(ctx)
	if err != nil {
		log.Printf("resume: ListPausedRuns failed: %v", err)
		return ResumeResult{
			State:    StateRunning.String(),
			Warnings: []string{fmt.Sprintf("could not enumerate paused runs: %v", err)},
		}, nil
	}
	resumed := 0
	var warnings []string
	for _, r := range paused {
		if err := m.store.SetRunPauseState(ctx, r.ID, store.PauseStateRunning); err != nil {
			warnings = append(warnings, fmt.Sprintf("set %s pause_state=running: %v", r.ID, err))
			continue
		}
		resumed++
	}
	return ResumeResult{
		State:            StateRunning.String(),
		ResumedRunsCount: resumed,
		Warnings:         warnings,
	}, nil
}

// Snapshot returns the current state + paused run count for the
// GET /v1/runtime/state handler. Cheap: one atomic load + one COUNT
// query (the store's ListPausedRuns is bounded by the partial index).
func (m *Manager) Snapshot(ctx context.Context) (StateSnapshot, error) {
	if m == nil {
		return StateSnapshot{State: StateRunning.String()}, nil
	}
	state := m.State()
	paused, err := m.store.ListPausedRuns(ctx)
	if err != nil {
		// Propagate the store error in both StateRunning and the
		// pausing/paused paths. Operators watching /v1/runtime/state
		// rely on the paused_runs_count being authoritative; silently
		// returning 0 on a DB hiccup would hide that the runtime is
		// flying blind on quiescence.
		return StateSnapshot{State: state.String()}, err
	}
	return StateSnapshot{State: state.String(), PausedRunsCount: len(paused)}, nil
}
