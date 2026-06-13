package pause

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/coord"
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

	// resumeCh is closed on Resume to WAKE runs parked at an iteration
	// boundary (the loop's PauseGate.Park selects on it). A fresh one is
	// allocated each Resume so the next pause cycle has a clean signal.
	// Mirror of pauseCh — pauseCh signals "stop", resumeCh signals "go".
	resumeCh chan struct{}

	// activeRuns is the in-flight-run registry the PauseGate registers
	// with (RegisterRun on loop entry, DeregisterRun on exit, parked flag
	// set while a run is parked at a boundary). Pause()'s Stage-2 barrier
	// waits until every registered run is parked (or the deadline) so
	// paused_runs_count is meaningful on return. Key: runID; value:
	// *runEntry. sync.Map so per-run registration doesn't contend with
	// state transitions (same rationale as activeTools).
	activeRuns sync.Map

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

	// v0.12.3 Phase 4 — cluster-mode wiring. All nil in single-replica
	// mode (LOOMCYCLE_REPLICA_ID unset); the manager runs exactly as
	// v0.11.x with no DB-state reads, no backplane publishes.

	// rss is the cluster-wide pause-state singleton store. State()
	// reads it on the hot path (lock-free); SetRuntimeStateStore
	// writes it. atomic.Pointer makes the read/write race-free —
	// review-1 finding #1 caught the original plain-pointer race.
	rss atomic.Pointer[coord.RuntimeStateStore]

	// bp is the backplane for cross-replica pause/resume signals.
	// Read inside Pause/Resume under m.mu (no race); SetBackplane
	// writes under m.mu. State() never reads bp, so it stays a
	// plain field (no atomic needed).
	bp coord.Backplane

	// stateCache is the 1s in-memory cache over rss.Get() so the
	// hot-path State() call doesn't hit the DB on every loop
	// iteration. Invalidated on backplane pause/resume receipt + on
	// every local Pause/Resume.
	stateCacheMu    sync.Mutex
	stateCacheValue RuntimeState
	stateCacheAt    time.Time
	// stateCacheTTL is read by State() (lock-free) + written by
	// SetRuntimeStateStore. atomic.Int64 (nanos) for race-free read.
	stateCacheTTL atomic.Int64
}

// toolEntry holds the per-tool-call cancellation handle. Manager-
// owned; created in ToolCtx, removed when the tool's goroutine exits.
type toolEntry struct {
	cancel   context.CancelFunc
	category ToolCategory
	toolName string
	id       string
}

// runEntry tracks one in-flight run for the pause barrier. parked flips
// true while the run's loop is parked at an iteration boundary.
type runEntry struct {
	parked atomic.Bool
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
		resumeCh:       make(chan struct{}),
		defaultTimeout: timeout,
	}
	m.state.Store(int32(StateRunning))
	return m
}

// State returns the current RuntimeState. Lock-free atomic read in
// single-replica mode; 1s-cached DB read in cluster mode so any
// replica converges on the cluster-wide state within the cache TTL.
// Safe to call from any goroutine including the loop's hot path.
func (m *Manager) State() RuntimeState {
	if m == nil {
		return StateRunning
	}
	rss := m.rss.Load() // atomic.Pointer load — race-free
	if rss == nil {
		// Single-replica mode: v0.11.x in-process atomic.
		return loadState(&m.state)
	}
	ttl := time.Duration(m.stateCacheTTL.Load())
	// Cluster mode: serve from cache (TTL nanos), refresh on miss.
	m.stateCacheMu.Lock()
	if !m.stateCacheAt.IsZero() && time.Since(m.stateCacheAt) < ttl {
		v := m.stateCacheValue
		m.stateCacheMu.Unlock()
		return v
	}
	m.stateCacheMu.Unlock()
	// Refresh outside the cache lock so a slow DB doesn't serialise
	// every State() caller. 2s timeout — a longer DB hiccup should
	// surface as a fallback to the in-process atomic, not block the
	// caller indefinitely.
	rctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	stateStr, _, err := rss.Get(rctx)
	if err != nil {
		log.Printf("pause: RuntimeStateStore.Get failed (serving in-process state): %v", err)
		return loadState(&m.state)
	}
	parsed := parseRuntimeState(stateStr)
	m.state.Store(int32(parsed))
	m.stateCacheMu.Lock()
	m.stateCacheValue = parsed
	m.stateCacheAt = time.Now()
	m.stateCacheMu.Unlock()
	return parsed
}

// SetRuntimeStateStore installs the v0.12.3 cluster-wide pause-state
// store + the 1s cache TTL. Called once from main.go inside the
// cluster-mode init block. Single-replica deployments never call this
// and the manager behaves identically to v0.11.x.
func (m *Manager) SetRuntimeStateStore(rss *coord.RuntimeStateStore, cacheTTL time.Duration) {
	if cacheTTL <= 0 {
		cacheTTL = 1 * time.Second
	}
	// Atomic writes — State() reads both fields lock-free.
	m.stateCacheTTL.Store(int64(cacheTTL))
	m.rss.Store(rss)
}

// SetBackplane installs the cluster-mode signal bus. When set,
// Pause/Resume publish on `loomcycle.pause` so remote replicas can
// apply the transition. Called once from main.go.
func (m *Manager) SetBackplane(bp coord.Backplane) {
	m.mu.Lock()
	m.bp = bp
	m.mu.Unlock()
}

// SubscribeBackplane starts the goroutine that listens for remote
// pause/resume events on `loomcycle.pause` and applies them locally
// via applyRemotePause / applyRemoteResume. The goroutine exits on
// ctx.Done. Backplane's reconnect-on-drop logic carries the wire-
// reliability concern; this goroutine only handles event dispatch.
func (m *Manager) SubscribeBackplane(ctx context.Context, bp coord.Backplane) error {
	ch, err := bp.Subscribe(ctx, "loomcycle.pause")
	if err != nil {
		return fmt.Errorf("pause: subscribe to loomcycle.pause: %w", err)
	}
	go func() {
		for evt := range ch {
			var p pauseBackplaneEvent
			if err := json.Unmarshal(evt.Payload, &p); err != nil {
				log.Printf("pause: malformed backplane event: %v", err)
				continue
			}
			switch p.Op {
			case "pause":
				m.applyRemotePause()
			case "resume":
				m.applyRemoteResume()
			default:
				log.Printf("pause: unknown backplane op %q", p.Op)
			}
		}
	}()
	return nil
}

// applyRemotePause is called by the SubscribeBackplane goroutine when
// another replica publishes a pause event. Transitions THIS replica's
// in-process state to StatePaused (skipping pausing — the originating
// replica already did the tool drain), closes the local pauseCh so
// the loop's iteration-boundary check sees the pause, and invalidates
// the state cache.
//
// Idempotent: a replica already at StatePaused ignores the event.
func (m *Manager) applyRemotePause() {
	m.mu.Lock()
	if loadState(&m.state) != StateRunning {
		m.mu.Unlock()
		return
	}
	m.state.Store(int32(StatePaused))
	close(m.pauseCh)
	m.mu.Unlock()

	m.stateCacheMu.Lock()
	m.stateCacheAt = time.Time{}
	m.stateCacheMu.Unlock()
	log.Printf("pause: cluster pause signal received — local state → paused")
}

// applyRemoteResume mirrors applyRemotePause for the resume direction.
// Allocates a fresh pauseCh so the next pause has a clean signal.
func (m *Manager) applyRemoteResume() {
	m.mu.Lock()
	if loadState(&m.state) == StateRunning {
		m.mu.Unlock()
		return
	}
	m.pauseCh = make(chan struct{})
	close(m.resumeCh)
	m.resumeCh = make(chan struct{})
	m.state.Store(int32(StateRunning))
	m.mu.Unlock()

	m.stateCacheMu.Lock()
	m.stateCacheAt = time.Time{}
	m.stateCacheMu.Unlock()
	log.Printf("pause: cluster resume signal received — local state → running")
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

// RegisterRun adds a run to the in-flight registry so Pause()'s barrier knows
// to wait for it to reach an iteration boundary. Called by the run's PauseGate
// (via the server) at loop entry. Idempotent. No-op on a nil manager.
func (m *Manager) RegisterRun(runID string) {
	if m == nil || runID == "" {
		return
	}
	m.activeRuns.LoadOrStore(runID, &runEntry{})
}

// DeregisterRun removes a run from the registry (loop exit — completed,
// failed, or cancelled). A deregistered run no longer holds back the barrier.
func (m *Manager) DeregisterRun(runID string) {
	if m == nil || runID == "" {
		return
	}
	m.activeRuns.Delete(runID)
}

// BeginPark is called by a run's PauseGate at an iteration boundary when a
// pause is in effect. Under the manager lock it RE-CHECKS state (so a run that
// raced a concurrent Resume does not park) and returns the resume channel to
// wait on. shouldPark=false means the runtime is already running again — the
// caller must not park. It does NOT mark the run parked: the caller must
// persist pause_state='paused' to the store FIRST, then call MarkParked, so
// the Pause() barrier (which waits on the parked flag) never reports a run
// parked before its store row is durably 'paused' (else finalizePause /
// snapshot, which read the store, would miss it).
func (m *Manager) BeginPark(runID string) (resume <-chan struct{}, shouldPark bool) {
	if m == nil {
		return nil, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if loadState(&m.state) == StateRunning {
		return nil, false
	}
	return m.resumeCh, true
}

// MarkParked flags a run parked for the Pause() barrier. Called by the
// PauseGate AFTER it has persisted pause_state='paused' (see BeginPark).
func (m *Manager) MarkParked(runID string) {
	if m == nil {
		return
	}
	if e, ok := m.activeRuns.Load(runID); ok {
		e.(*runEntry).parked.Store(true)
	}
}

// EndPark clears a run's parked flag (the loop woke and is resuming).
func (m *Manager) EndPark(runID string) {
	if m == nil {
		return
	}
	if e, ok := m.activeRuns.Load(runID); ok {
		e.(*runEntry).parked.Store(false)
	}
}

// runsQuiesced reports whether every registered in-flight run is parked at a
// boundary (or none are registered). total is the registered count; used by
// Pause()'s Stage-2 barrier and for the not-all-parked warning.
func (m *Manager) runsQuiesced() (allParked bool, total, parked int) {
	m.activeRuns.Range(func(_, v any) bool {
		total++
		if v.(*runEntry).parked.Load() {
			parked++
		}
		return true
	})
	return total == parked, total, parked
}

// unparkedRunIDs returns the registered runs that have NOT reached a pause
// boundary yet — used to name them in the timeout/cancel warning. The classic
// case is a fan-out PARENT blocked in Agent.parallel_spawn: its loop is
// suspended in the tool while its children park, so it can't reach its own
// boundary until the spawn returns (on resume). Snapshotting mid-fan-out would
// capture the parked children WITHOUT the parent, so callers must snapshot only
// at a quiescent boundary (a clean Pause with no warning).
func (m *Manager) unparkedRunIDs() []string {
	var out []string
	m.activeRuns.Range(func(k, v any) bool {
		if !v.(*runEntry).parked.Load() {
			out = append(out, k.(string))
		}
		return true
	})
	return out
}

// unparkedWarning builds the "runs didn't reach a boundary" warning (shared by
// the deadline + caller-cancel paths), naming the runs so an operator knows a
// fan-out parent (or a long tool/provider turn) held back the quiesce — and
// that paused_runs_count therefore excludes them. Empty when all parked.
func (m *Manager) unparkedWarning(whenPhrase string) string {
	ids := m.unparkedRunIDs()
	if len(ids) == 0 {
		return ""
	}
	return fmt.Sprintf(
		"%d in-flight run(s) did not reach a pause boundary %s — still executing a tool / "+
			"provider turn (e.g. a fan-out PARENT blocked in parallel_spawn whose children HAVE "+
			"parked); they park on their next boundary, so paused_runs_count excludes them. Snapshot "+
			"only at a quiescent boundary (a clean Pause with no warning), not mid-fan-out. runs: %s",
		len(ids), whenPhrase, strings.Join(ids, ", "))
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
		c, cancel := context.WithTimeout(parent, m.defaultTimeout)
		return c, cancel

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
	// Use loadState (in-process atomic) instead of m.State() — Pause
	// is the authority on whether a LOCAL pause is already in flight,
	// not the cluster-wide DB state. m.State() in cluster mode would
	// do a DB read while holding m.mu, serialising the entire
	// SubscribeBackplane pipeline for the duration. Review-1 #2.
	if loadState(&m.state) != StateRunning {
		state := loadState(&m.state)
		m.mu.Unlock()
		return PauseResult{State: state.String()}, ErrAlreadyPausing
	}
	m.state.Store(int32(StatePausing))
	// Close the broadcast channel under the lock so concurrent
	// PauseCh() callers observe a consistent (state, channel) pair.
	close(m.pauseCh)
	bp := m.bp
	m.mu.Unlock()
	rss := m.rss.Load()

	// v0.12.3 Phase 4: cluster mode — write DB state + publish on
	// backplane so remote replicas transition into pausing/paused
	// within the cache-TTL window. Errors are logged but not fatal:
	// the local pause still proceeds + the local tool drain still
	// runs. A remote replica that misses the publish will eventually
	// converge via the next cache refresh (≤ TTL).
	if rss != nil {
		rctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := rss.Set(rctx, "pausing", 0); err != nil {
			log.Printf("pause: rss.Set(pausing) failed: %v (continuing in-process only)", err)
		}
		cancel()
	}
	if bp != nil {
		payload, _ := json.Marshal(pauseBackplaneEvent{Op: "pause"})
		if err := bp.Publish(context.Background(), "loomcycle.pause", payload); err != nil {
			log.Printf("pause: backplane publish(pause) failed: %v", err)
		}
	}
	m.invalidateStateCache()

	// Apply per-tool cancel policy. Idempotent → cancel immediately;
	// non-idempotent / external → leave running, deadline applied
	// implicitly via the goroutine's own ctx (already wrapped by
	// ToolCtx with WithTimeout). For runs already in flight whose
	// ToolCtx fired BEFORE pause was declared, we apply category
	// policy here defensively.
	var (
		forceCancelMu   sync.Mutex
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

	// Stage 2: wait for activeTools to drain AND for every in-flight run to
	// reach a pause boundary (PauseGate.Park persisting pause_state='paused'),
	// or hit the deadline. Waiting for runs to park is what makes
	// paused_runs_count meaningful on return — a run blocked in a long tool /
	// provider turn parks at its NEXT boundary, bounded by this deadline.
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()

	var warnings []string
	for {
		toolsEmpty := true
		m.activeTools.Range(func(_, _ any) bool {
			toolsEmpty = false
			return false
		})
		runsParked, _, _ := m.runsQuiesced()
		if toolsEmpty && runsParked {
			break
		}
		select {
		case <-ctx.Done():
			// Caller cancelled the pause request; mark anything
			// still in flight as force-cancelled, transition to
			// StatePaused, return the partial result.
			m.forceCancelRemaining(&forceCancelMu, &forceCancel)
			if w := m.unparkedWarning("at cancellation"); w != "" {
				warnings = append(warnings, w)
			}
			return m.finalizePause(start, forceCancel, idempotentCount, append(warnings,
				fmt.Sprintf("pause request cancelled by caller: %v", ctx.Err()))), nil
		case <-deadline.C:
			m.forceCancelRemaining(&forceCancelMu, &forceCancel)
			if w := m.unparkedWarning("within the timeout"); w != "" {
				warnings = append(warnings, w)
			}
			break
		case <-tick.C:
			continue
		}
		// deadline path falls through; break above only breaks the
		// select, so we explicitly exit the for-loop here.
		break
	}

	return m.finalizePause(start, forceCancel, idempotentCount, warnings), nil
}

// forceCancelRemaining cancels every entry still in activeTools and
// removes them from the registry. Called from the deadline / caller-
// cancellation paths in Pause.
func (m *Manager) forceCancelRemaining(mu *sync.Mutex, counter *int) {
	m.activeTools.Range(func(k, v any) bool {
		e := v.(*toolEntry)
		e.cancel()
		mu.Lock()
		*counter++
		mu.Unlock()
		m.activeTools.Delete(k)
		return true
	})
}

// invalidateStateCache forces the next State() call in cluster mode
// to re-read from the DB. Called after every local state change so
// concurrent State() callers don't see a stale cached value.
func (m *Manager) invalidateStateCache() {
	m.stateCacheMu.Lock()
	m.stateCacheAt = time.Time{}
	m.stateCacheMu.Unlock()
}

// finalizePause transitions to StatePaused, queries the count of
// runs that committed pause_state='paused' to the DB, and assembles
// the PauseResult payload.
func (m *Manager) finalizePause(start time.Time, forceCancel, idempotentCount int, warnings []string) PauseResult {
	m.state.Store(int32(StatePaused))
	m.invalidateStateCache()

	paused, err := m.store.ListPausedRuns(context.Background())
	pausedCount := 0
	if err != nil {
		log.Printf("pause: ListPausedRuns failed during finalize: %v", err)
		warnings = append(warnings, "could not enumerate paused runs (state still transitioned to paused)")
	} else {
		pausedCount = len(paused)
	}

	// v0.12.3 cluster-mode: write final state + cluster-wide paused
	// count to the singleton row so remote /v1/_state reads see the
	// authoritative cluster value (not just this replica's view).
	if rss := m.rss.Load(); rss != nil {
		rctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := rss.Set(rctx, "paused", pausedCount); err != nil {
			log.Printf("pause: rss.Set(paused) failed: %v", err)
			warnings = append(warnings, fmt.Sprintf("rss.Set(paused) failed: %v", err))
		}
		cancel()
	}

	if idempotentCount > 0 {
		log.Printf("pause: cancelled %d idempotent tools immediately", idempotentCount)
	}
	if forceCancel > 0 {
		log.Printf("pause: force-cancelled %d tools at deadline", forceCancel)
	}

	return PauseResult{
		State:               StatePaused.String(),
		DurationMs:          time.Since(start).Milliseconds(),
		ForceCancelledCount: forceCancel,
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
	// loadState — see the Pause() comment about avoiding m.State()
	// under m.mu in cluster mode (review-1 #2).
	if loadState(&m.state) != StatePaused {
		st := loadState(&m.state).String()
		m.mu.Unlock()
		return ResumeResult{State: st}, ErrNotPaused
	}
	m.mu.Unlock()

	// Snapshot the paused set BEFORE waking any gate. While the manager is
	// still StatePaused the gates are parked (selecting on resumeCh) and no new
	// runs are admitted, so this list is a stable, accurate count. If we woke
	// the gates first (the old ordering: close(resumeCh) then ListPausedRuns),
	// a woken gate flips its own pause_state back to running and races the
	// list — under-counting ResumedRunsCount nondeterministically (the flaky
	// TestManager_PauseWaitsForRunToPark). The per-run SetRunPauseState(running)
	// below is idempotent with a gate's self-flip; only the count must be
	// snapshotted pre-wake.
	paused, listErr := m.store.ListPausedRuns(ctx)

	m.mu.Lock()
	// Re-check under the lock: a concurrent Resume may have transitioned us to
	// running between the pre-check and here. The loser reports ErrNotPaused
	// (the winner already woke the gates) — keeps the state transition atomic.
	if loadState(&m.state) != StatePaused {
		st := loadState(&m.state).String()
		m.mu.Unlock()
		return ResumeResult{State: st}, ErrNotPaused
	}
	// New broadcast channel for the next pause cycle. Old callers
	// holding the closed channel from the prior pause naturally fall
	// through their select-default branch on the next iteration.
	m.pauseCh = make(chan struct{})
	// Wake every parked run (PauseGate.Park is selecting on this channel),
	// then allocate a fresh one for the next pause cycle.
	close(m.resumeCh)
	m.resumeCh = make(chan struct{})
	m.state.Store(int32(StateRunning))
	bp := m.bp
	m.mu.Unlock()
	rss := m.rss.Load()
	m.invalidateStateCache()

	// v0.12.3 Phase 4: cluster mode — write DB state back to running
	// and publish on backplane so remote replicas re-allow new runs
	// + new tool dispatches.
	if rss != nil {
		rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		if err := rss.Set(rctx, "running", 0); err != nil {
			log.Printf("resume: rss.Set(running) failed: %v", err)
		}
		cancel()
	}
	if bp != nil {
		payload, _ := json.Marshal(pauseBackplaneEvent{Op: "resume"})
		if err := bp.Publish(context.Background(), "loomcycle.pause", payload); err != nil {
			log.Printf("resume: backplane publish(resume) failed: %v", err)
		}
	}

	// The gates are now woken (above); even if the snapshot failed we must not
	// short-circuit before that. Handle the snapshot error here for the count.
	if listErr != nil {
		log.Printf("resume: ListPausedRuns failed: %v", listErr)
		return ResumeResult{
			State:    StateRunning.String(),
			Warnings: []string{fmt.Sprintf("could not enumerate paused runs: %v", listErr)},
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
	if state == StateRunning {
		// Fast path: no need to query when we know no runs are paused.
		// (Edge case: a run in StatePaused while manager is StateRunning
		// is impossible — paused runs only exist after Pause completes,
		// and Resume flips them all back. Still, query for safety.)
		paused, _ := m.store.ListPausedRuns(ctx)
		return StateSnapshot{State: state.String(), PausedRunsCount: len(paused)}, nil
	}
	paused, err := m.store.ListPausedRuns(ctx)
	if err != nil {
		return StateSnapshot{State: state.String()}, err
	}
	return StateSnapshot{State: state.String(), PausedRunsCount: len(paused)}, nil
}
