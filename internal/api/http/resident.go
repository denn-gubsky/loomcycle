package http

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/steer"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// --- RFC BK: resident (interactive) sub-agents ---
//
// A resident child is a PERSISTENT interactive sub-run a parent drives via the
// Agent tool's open/send/close ops. Unlike a spawn (fire-and-return), the child
// stays resident between sends — its loop goroutine parks at awaiting_input, so
// anything it holds (a warm sandbox container, a REPL, working memory) survives
// from one send to the next. Each send blocks until the child re-parks.
//
// P1 is single-replica: the registry is in-process. A child is addressable only
// within its opener's tenant. The provider slot is held for the child's life
// (bounded by the per-run cap; a same-provider child gets the RFC BF ancestor
// carve-out so it pins nothing). Prompt sandbox-container teardown on close is a
// follow-up — on close/idle the container idle-reaps on the sidecar's own TTL.

const (
	// defaultMaxLiveChildren is LOOMCYCLE_MAX_LIVE_CHILDREN_PER_RUN's default:
	// one full parallel_spawn (MaxParallelSpawns) fits, a second at once does not.
	defaultMaxLiveChildren        = 32
	defaultMaxResidentChildren    = 8
	defaultResidentChildIdleTTLMs = 30 * 60 * 1000 // 30 min
	// defaultResidentMaxTurn bounds one turn of a resident child. The idle rule
	// cannot see a turn that never ends — a hung tool, a provider call that
	// never returns, a model looping on tool calls with no iteration cap (a
	// resident child without max_iterations is unbounded) — and such a child
	// would hold its goroutine, its sandbox and a resident slot forever. Two
	// hours is far past any turn a parent should wait on (four idle TTLs), yet
	// finite. LOOMCYCLE_RESIDENT_MAX_TURN_SECONDS overrides it; there is no
	// unlimited setting.
	defaultResidentMaxTurn = 2 * time.Hour
	// residentTombstoneTTL is how long the reason a child was reaped stays
	// answerable after its teardown removed it: long enough for a parent busy
	// elsewhere to poll and learn why, short enough that the map stays small.
	residentTombstoneTTL  = time.Hour
	residentSweepInterval = 60 * time.Second
	// residentCancelReparkTimeout bounds how long op=cancel waits for the child's
	// loop to re-park after its turn is stopped (RFC BK P2). The re-park is near-
	// instant once the turn ctx cancels; this is only a backstop.
	residentCancelReparkTimeout = 15 * time.Second
)

// residentChild is a live handle to one resident interactive sub-agent.
type residentChild struct {
	runID         string
	agentID       string // cancel-registry key (close/idle cancel by agent_id)
	agentName     string // the resident child's agent name (for Web-UI visibility)
	parentAgentID string // the opener's agent id (parent-teardown backstop)
	tenantID      string // ownership: send/close must come from this tenant
	userID        string
	cancel        context.CancelCauseFunc // direct fallback if the registry entry is gone
	idleTTL       time.Duration
	maxTurn       time.Duration // the turn ceiling: a turn running longer is reaped

	mu         sync.Mutex
	buf        strings.Builder // assistant text accumulated for the CURRENT turn
	state      string          // "awaiting_input" | "completed" | "failed"
	turnDone   chan struct{}   // closed once when the current turn parks or the run ends
	turnClosed bool
	running    bool      // a turn is in flight (between beginTurn and park/terminal) — RFC BK P2
	lastUsed   time.Time // the idle clock: open/send/poll/cancel and a turn's end
	// turnStarted is the turn-ceiling clock. Only beginTurn moves it: a parent
	// polling a wedged turn keeps the child from going idle, never from the
	// ceiling.
	turnStarted time.Time
	done        bool   // loop goroutine exited
	reapReason  string // set by the sweeper before it cancels the child
	// startupPark marks a child resumed parked whose loop has not yet parked:
	// the awaiting_input it announces then is the park it was already in, not
	// the end of a turn — a send that arrived first must not end on it.
	startupPark bool
}

// beginTurn resets the per-turn buffer + wake channel. Called before open's
// first turn and before every send. Returns the channel the caller waits on.
func (rc *residentChild) beginTurn(now time.Time) <-chan struct{} {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	rc.buf.Reset()
	rc.turnDone = make(chan struct{})
	rc.turnClosed = false
	rc.running = true
	rc.lastUsed = now
	rc.turnStarted = now
	return rc.turnDone
}

func (rc *residentChild) appendText(t string) {
	rc.mu.Lock()
	rc.buf.WriteString(t)
	rc.mu.Unlock()
}

// endTurn records the turn's terminal state and wakes the waiter exactly once.
// Idempotent per turn: the park boundary (fwd) and the loop-exit both call it;
// whichever comes first wins, the second is a no-op. The idle clock restarts
// when the turn ends, so a parent gets the whole TTL to collect a long turn's
// result — measured from its start, a turn longer than the TTL would be reaped
// at the first sweep after it parked.
func (rc *residentChild) endTurn(state string) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	rc.state = state
	if rc.running {
		rc.lastUsed = time.Now()
	}
	rc.running = false
	if !rc.turnClosed && rc.turnDone != nil {
		rc.turnClosed = true
		close(rc.turnDone)
	}
}

// observe is the child's capturing emit: it accumulates the child's assistant
// text for the current turn, and the awaiting_input boundary ends the turn and
// wakes the waiter.
func (rc *residentChild) observe(ev providers.Event) {
	switch ev.Type {
	case providers.EventText:
		rc.appendText(ev.Text)
	case providers.EventAwaitingInput:
		rc.mu.Lock()
		startup := rc.startupPark
		rc.startupPark = false
		rc.mu.Unlock()
		if !startup {
			rc.endTurn("awaiting_input")
		}
	}
}

// parked files a resumed child that was parked between turns when it paused:
// no turn in flight, waiting for its parent's next send.
func (rc *residentChild) parked(now time.Time) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	rc.state = "awaiting_input"
	rc.lastUsed = now
	rc.startupPark = true
}

// currentTurnDone returns the channel the in-flight (or just-ended) turn signals
// on, and whether a turn is currently running. Used by poll/cancel, which don't
// start a new turn.
func (rc *residentChild) currentTurnDone() (<-chan struct{}, bool) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return rc.turnDone, rc.running
}

// touch restarts the idle clock. poll and cancel use it: a parent checking on
// a child is using it, even though it starts no turn.
func (rc *residentChild) touch(now time.Time) {
	rc.mu.Lock()
	rc.lastUsed = now
	rc.mu.Unlock()
}

// discountPause takes a runtime pause that ran from since to now off the
// child's idle and turn-ceiling clocks: a clock that started before the pause
// moves forward by the pause's length, one that moved during it restarts now.
func (rc *residentChild) discountPause(since, now time.Time) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	rc.lastUsed = discountPaused(rc.lastUsed, since, now)
	rc.turnStarted = discountPaused(rc.turnStarted, since, now)
}

func discountPaused(t, since, now time.Time) time.Time {
	if t.Before(since) {
		return t.Add(now.Sub(since))
	}
	return now
}

func (rc *residentChild) markDone(state string) {
	rc.mu.Lock()
	rc.done = true
	rc.mu.Unlock()
	rc.endTurn(state) // wake a waiter blocked on the final (non-parking) turn
}

// reaped returns why the sweeper reaped the child, or "" if it did not.
func (rc *residentChild) reaped() string {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return rc.reapReason
}

func (rc *residentChild) readTurn() (string, string) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return rc.buf.String(), rc.state
}

// residentInfo is the non-secret read model for Web-UI visibility (RFC BK P3).
// State: "running" (mid-turn) | "awaiting_input" (parked) | "completed" | "failed".
type residentInfo struct {
	ChildRunID     string    `json:"child_run_id"`
	AgentID        string    `json:"agent_id"`
	Agent          string    `json:"agent,omitempty"`
	ParentAgentID  string    `json:"parent_agent_id,omitempty"`
	TenantID       string    `json:"tenant_id,omitempty"`
	State          string    `json:"state"`
	IdleTTLSeconds int       `json:"idle_ttl_seconds"`
	LastUsedAt     time.Time `json:"last_used_at"`
}

// snapshotInfo returns the child's current read model.
func (rc *residentChild) snapshotInfo() residentInfo {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	state := rc.state
	if rc.running {
		state = "running"
	}
	return residentInfo{
		ChildRunID:     rc.runID,
		AgentID:        rc.agentID,
		Agent:          rc.agentName,
		ParentAgentID:  rc.parentAgentID,
		TenantID:       rc.tenantID,
		State:          state,
		IdleTTLSeconds: int(rc.idleTTL / time.Second),
		LastUsedAt:     rc.lastUsed,
	}
}

// residentRegistry maps child run_id → residentChild (P1 in-process).
type residentRegistry struct {
	mu sync.Mutex
	m  map[string]*residentChild
	// gone keeps why a reaped child went away after its teardown removed it,
	// so a parent's later poll/send learns the reason instead of a bare
	// not-found. Pruned by the sweeper after residentTombstoneTTL.
	gone map[string]residentTombstone
	// pausedSince is when the sweeper first saw the runtime paused; zero
	// while it runs. See sweepResidentChildren.
	pausedSince time.Time
}

type residentTombstone struct {
	tenantID      string
	userID        string
	parentAgentID string
	reason        string
	at            time.Time
}

// residentOwnedBy reports whether caller may address a child opened in this
// tenant by this user. It is the run-content rule (auth.OwnedRowVisible) read
// off the caller's run identity: any run in the tenant may — the tenant's
// runs collaborate, as they do on run cancel, compact and retune — except an
// isolated member's run, which may address only its own user's children. A
// run always sits in one tenant, so OwnedRowVisible's admin/legacy
// cross-tenant pass has no counterpart here. The opener's agent id is
// deliberately not part of the rule.
func residentOwnedBy(tenantID, userID string, caller tools.RunIdentityValue) bool {
	if tenantID != caller.TenantID {
		return false
	}
	return !caller.Isolated || userID == caller.UserID
}

func newResidentRegistry() *residentRegistry {
	return &residentRegistry{m: map[string]*residentChild{}, gone: map[string]residentTombstone{}}
}

func (r *residentRegistry) add(rc *residentChild) {
	r.mu.Lock()
	r.m[rc.runID] = rc
	r.mu.Unlock()
}

func (r *residentRegistry) get(runID string) (*residentChild, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rc, ok := r.m[runID]
	return rc, ok
}

// remove drops a torn-down child, keeping its reap reason when it was reaped.
func (r *residentRegistry) remove(rc *residentChild) {
	reason := rc.reaped()
	r.mu.Lock()
	delete(r.m, rc.runID)
	if reason != "" {
		r.gone[rc.runID] = residentTombstone{tenantID: rc.tenantID, userID: rc.userID, parentAgentID: rc.parentAgentID, reason: reason, at: time.Now()}
	}
	r.mu.Unlock()
}

// goneReason returns why a child that is no longer registered was reaped, for
// a caller that could have addressed it (residentOwnedBy); "" when it was not
// reaped (closed, ended) or the caller could not.
func (r *residentRegistry) goneReason(runID string, caller tools.RunIdentityValue) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if t, ok := r.gone[runID]; ok && residentOwnedBy(t.tenantID, t.userID, caller) {
		return t.reason
	}
	return ""
}

func (r *residentRegistry) pruneGone(now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, t := range r.gone {
		if now.Sub(t.at) > residentTombstoneTTL {
			delete(r.gone, id)
		}
	}
}

// notePause records that the runtime is paused at now, keeping the earliest
// sighting of the pause.
func (r *residentRegistry) notePause(now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.pausedSince.IsZero() {
		r.pausedSince = now
	}
}

// endPause returns when the pause the sweeper last saw began, and clears it;
// zero when it saw none.
func (r *residentRegistry) endPause() time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	since := r.pausedSince
	r.pausedSince = time.Time{}
	return since
}

func (r *residentRegistry) countByParent(parentAgentID string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, rc := range r.m {
		if rc.parentAgentID == parentAgentID {
			n++
		}
	}
	return n
}

func (r *residentRegistry) snapshot() []*residentChild {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*residentChild, 0, len(r.m))
	for _, rc := range r.m {
		out = append(out, rc)
	}
	return out
}

// listInfo returns a read-model snapshot of every resident child (RFC BK P3).
// Snapshots the pointers under the registry lock, then reads each child under
// its OWN lock — no lock-ordering between the registry and a child.
func (r *residentRegistry) listInfo() []residentInfo {
	children := r.snapshot()
	out := make([]residentInfo, 0, len(children))
	for _, rc := range children {
		out = append(out, rc.snapshotInfo())
	}
	return out
}

// maxResidentChildren / residentChildIdleTTL read the operator knobs with
// defaults. (Env is parsed into cfg.Env at config load — see config additions.)
func (s *Server) maxResidentChildren() int {
	if s.cfg() != nil && s.cfg().Env.MaxInteractiveChildren > 0 {
		return s.cfg().Env.MaxInteractiveChildren
	}
	return defaultMaxResidentChildren
}

// maxLiveChildren is the per-run live-children limit (0 = the default).
func (s *Server) maxLiveChildren() int {
	if s.cfg() != nil && s.cfg().Env.MaxLiveChildrenPerRun > 0 {
		return s.cfg().Env.MaxLiveChildrenPerRun
	}
	return defaultMaxLiveChildren
}

// residentMaxTurn is the turn ceiling (LOOMCYCLE_RESIDENT_MAX_TURN_SECONDS;
// 0 = the default).
func (s *Server) residentMaxTurn() time.Duration {
	if s.cfg() != nil && s.cfg().Env.ResidentMaxTurnSeconds > 0 {
		return time.Duration(s.cfg().Env.ResidentMaxTurnSeconds) * time.Second
	}
	return defaultResidentMaxTurn
}

func (s *Server) residentChildIdleTTL() time.Duration {
	if s.cfg() != nil && s.cfg().Env.InteractiveChildIdleTTLMs > 0 {
		return time.Duration(s.cfg().Env.InteractiveChildIdleTTLMs) * time.Millisecond
	}
	return time.Duration(defaultResidentChildIdleTTLMs) * time.Millisecond
}

// openResidentChild starts a resident interactive sub-run, runs its first turn,
// parks it at awaiting_input, and returns (childRunID, firstOutput, state).
// timeoutMs bounds the wait for that first turn exactly as it bounds a send's:
// 0 blocks until the child parks; >0 returns state "running" + the partial
// output if the turn is still going, and the parent polls to collect it.
func (s *Server) openResidentChild(ctx context.Context, name, prompt, defID string, idleTTLSeconds, timeoutMs int) (string, string, string, error) {
	if s.residentReg == nil || s.steerReg == nil {
		// A resident child parks on its steer queue between turns; without the
		// steer registry it could not park (nor could send reach it).
		return "", "", "", fmt.Errorf("resident sub-agents are not enabled on this runtime")
	}
	parent := tools.RunIdentity(ctx)
	if cap := s.maxResidentChildren(); s.residentReg.countByParent(parent.AgentID) >= cap {
		return "", "", "", fmt.Errorf("resident sub-agent cap reached (%d open for this run); close one before opening another", cap)
	}
	// A resident child is one of the run's live children for as long as it
	// lives: released by its loop goroutine's teardown, or here if it never
	// starts.
	live, err := s.liveChildren.Admit(tools.RunID(ctx), 1)
	if err != nil {
		return "", "", "", err
	}
	started := false
	defer func() {
		if !started {
			live[0]()
		}
	}()
	prompt, err = s.subagentStart(ctx, name, prompt)
	if err != nil {
		return "", "", "", err
	}

	rc := &residentChild{parentAgentID: parent.AgentID, idleTTL: s.residentChildIdleTTL(), maxTurn: s.residentMaxTurn()}
	if idleTTLSeconds > 0 {
		rc.idleTTL = time.Duration(idleTTLSeconds) * time.Second
	}
	fwd := rc.observe
	// The child must SURVIVE this tool call returning → detach its ctx from the
	// parent request's cancellation (keep values) before prepareSubRun wraps it
	// in its own cancel scope (fired by close / idle-reap / parent teardown).
	// No extra system segment: a resident sub-agent is opened by the Agent tool,
	// not by a team state, so only the AgentDef's own prompt applies.
	prep, err := s.prepareSubRun(context.WithoutCancel(ctx), name, "", prompt, defID, true, fwd)
	if err != nil {
		return "", "", "", err
	}
	rc.runID = prep.RunID
	rc.agentID = prep.AgentID
	rc.agentName = name
	rc.tenantID = prep.TenantID
	rc.userID = prep.UserID
	rc.cancel = prep.CancelFn

	steerQ, onSteer, closeSteer, deregSteer := s.makeSteer(prep.SteerCtx, prep.RunID, prep.AgentID, prep.SessionID, prep.UserID, prep.Emit)
	prep.Opts.Interactive = true
	prep.Opts.SteerQueue = steerQ
	prep.Opts.OnSteer = onSteer
	prep.Opts.CloseSteerIfEmpty = closeSteer
	prep.Opts.ArmTurnCancel = s.armTurnCancel(prep.RunID)

	turnDone := rc.beginTurn(time.Now())
	s.residentReg.add(rc)

	started = true
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("resident child %s panicked: %v", prep.RunID, r)
				rc.markDone("failed")
			}
			deregSteer()
			prep.cleanup()
			prep.Slot.releaseCurrent()
			s.residentReg.remove(rc)
			live[0]()
		}()
		res, runErr := loop.Run(prep.LoopCtx, prep.Opts)
		st := "completed"
		if runErr != nil {
			st = "failed"
			prep.Emit(runErrorEvent(runErr))
		}
		s.finishRunWithCancel(context.WithoutCancel(prep.SteerCtx), prep.SteerCtx, prep.RunID, res, runErr, prep.Meta)
		rc.markDone(st)
	}()

	out, state, aerr := rc.awaitTurn(ctx, turnDone, time.Duration(timeoutMs)*time.Millisecond, true)
	out, aerr = s.residentHandBack(ctx, rc, out, state, aerr)
	return prep.RunID, out, state, aerr
}

// residentHandBack passes what a resident child hands its parent through the
// parent's subagent_stop hooks, as a one-shot child's result is. A resident
// child hands back an output at every turn, not once at the end, so the hooks
// run on each, with the child's state as the status. A refusal leaves the
// child open: the parent may send again, or close it.
func (s *Server) residentHandBack(ctx context.Context, rc *residentChild, out, state string, err error) (string, error) {
	if reason := rc.reaped(); reason != "" && err == nil {
		err = fmt.Errorf("resident sub-agent %q was reaped by the runtime (%s); open a new one", rc.runID, reason)
	}
	out, herr := s.subagentStop(ctx, rc.agentName, rc.runID, state, out, err)
	if herr != nil && herr != err {
		return "", fmt.Errorf("%w (child_run_id %s is still open: send again or close it)", herr, rc.runID)
	}
	return out, herr
}

// sendResidentChild injects the next instruction into a resident child and waits
// for that turn (RFC BK P2: timeoutMs bounds the wait — 0 blocks until re-park,
// >0 returns state "running" + the partial output if the turn is still going).
func (s *Server) sendResidentChild(ctx context.Context, childRunID, prompt string, timeoutMs int) (string, string, error) {
	rc, ok := s.lookupOwnedResident(ctx, childRunID)
	if !ok {
		return "", "", s.residentNotFound(ctx, childRunID)
	}
	// A send while the previous turn is still in flight would interleave two
	// turns. Refuse — the parent should poll to await it, or cancel to interrupt.
	if _, running := rc.currentTurnDone(); running {
		return "", "", fmt.Errorf("resident sub-agent %q is still running its previous turn; poll to await it or cancel to interrupt", childRunID)
	}
	turnDone := rc.beginTurn(time.Now())
	if _, err := s.steerReg.Push(ctx, childRunID, steer.Message{Text: prompt, Source: "agent", EnqueuedAt: time.Now()}); err != nil {
		return "", "", fmt.Errorf("steer resident sub-agent %q: %w", childRunID, err)
	}
	out, state, err := rc.awaitTurn(ctx, turnDone, time.Duration(timeoutMs)*time.Millisecond, true)
	out, err = s.residentHandBack(ctx, rc, out, state, err)
	return out, state, err
}

// pollResidentChild checks a resident child without sending new input (RFC BK P2)
// — returns its current output-so-far + state. timeoutMs=0 is a non-blocking
// snapshot; >0 waits up to that long for the child to park.
func (s *Server) pollResidentChild(ctx context.Context, childRunID string, timeoutMs int) (string, string, error) {
	rc, ok := s.lookupOwnedResident(ctx, childRunID)
	if !ok {
		return "", "", s.residentNotFound(ctx, childRunID)
	}
	rc.touch(time.Now())
	td, _ := rc.currentTurnDone()
	if td == nil {
		// No turn has ever started (shouldn't happen post-open) — report state.
		out, st := rc.readTurn()
		out, err := s.residentHandBack(ctx, rc, out, st, nil)
		return out, st, err
	}
	out, state, err := rc.awaitTurn(ctx, td, time.Duration(timeoutMs)*time.Millisecond, false)
	out, err = s.residentHandBack(ctx, rc, out, state, err)
	return out, state, err
}

// cancelResidentChildTurn turn-cancels a resident child's CURRENT turn (RFC BK
// P2): it fires the child's armed turn-cancel token so the loop stops the
// in-flight turn and re-parks at awaiting_input — the child stays alive. The
// child is co-located (fire the local token directly; ownership already gated by
// lookupOwnedResident). A child that isn't mid-turn is a no-op.
func (s *Server) cancelResidentChildTurn(ctx context.Context, childRunID string) (string, string, error) {
	rc, ok := s.lookupOwnedResident(ctx, childRunID)
	if !ok {
		return "", "", s.residentNotFound(ctx, childRunID)
	}
	rc.touch(time.Now())
	td, running := rc.currentTurnDone()
	if !running {
		out, st := rc.readTurn() // already parked/idle — nothing to cancel
		out, err := s.residentHandBack(ctx, rc, out, st, nil)
		return out, st, err
	}
	if s.turnCancelReg == nil || !s.turnCancelReg.CancelLocal(childRunID, "cancelled by parent (resident sub-agent)") {
		// Not armed / token vanished (the turn just ended) — treat as parked.
		out, st := rc.readTurn()
		out, err := s.residentHandBack(ctx, rc, out, st, nil)
		return out, st, err
	}
	// Wait (bounded) for the loop to re-park after the turn is stopped.
	out, state, err := rc.awaitTurn(ctx, td, residentCancelReparkTimeout, false)
	out, err = s.residentHandBack(ctx, rc, out, state, err)
	return out, state, err
}

// closeResidentChild finalizes a resident child (idempotent). Cancelling the
// child's loop ctx terminates it and fires its goroutine teardown.
func (s *Server) closeResidentChild(ctx context.Context, childRunID string) error {
	rc, ok := s.lookupOwnedResident(ctx, childRunID)
	if !ok {
		return nil // idempotent: already gone (or not ours → opaque)
	}
	if _, found := s.cancelReg.Cancel(rc.agentID, "closed by parent (resident sub-agent)"); !found && rc.cancel != nil {
		rc.cancel(fmt.Errorf("closed by parent"))
	}
	return nil
}

// residentNotFound is the error for a child_run_id with no live child: the
// reap reason when the sweeper reaped it, else the generic not-found.
func (s *Server) residentNotFound(ctx context.Context, childRunID string) error {
	if s.residentReg != nil {
		if reason := s.residentReg.goneReason(childRunID, tools.RunIdentity(ctx)); reason != "" {
			return fmt.Errorf("resident sub-agent %q was reaped by the runtime (%s); open a new one", childRunID, reason)
		}
	}
	return fmt.Errorf("resident sub-agent %q not found (it may have been closed or timed out)", childRunID)
}

// lookupOwnedResident resolves a child by run_id for a caller residentOwnedBy
// admits. Any other caller — another tenant, or an isolated member's run for
// another user's child — gets the same not-found as an unknown id.
func (s *Server) lookupOwnedResident(ctx context.Context, childRunID string) (*residentChild, bool) {
	if s.residentReg == nil {
		return nil, false
	}
	rc, ok := s.residentReg.get(childRunID)
	if !ok {
		return nil, false
	}
	if !residentOwnedBy(rc.tenantID, rc.userID, tools.RunIdentity(ctx)) {
		return nil, false
	}
	return rc, true
}

// awaitTurn waits for the child's current turn to finish (park or terminate),
// returning its output-so-far + state. The zero-timeout behavior differs by
// caller (blockWhenZero): open/send block until the turn ends (P1 semantics);
// poll takes a non-blocking snapshot. A positive timeout bounds the wait — on
// expiry the turn is still in flight, so it returns the partial output with
// state "running". A cancelled caller ctx returns state "interrupted" (the child
// keeps running — re-addressable by a later send/poll, or reaped on teardown).
func (rc *residentChild) awaitTurn(ctx context.Context, turnDone <-chan struct{}, timeout time.Duration, blockWhenZero bool) (string, string, error) {
	// The caller's run is parked while it waits on its child, bounded or not.
	defer providers.BeginWait(ctx)()
	if timeout <= 0 {
		if blockWhenZero {
			select {
			case <-turnDone:
				out, st := rc.readTurn()
				return out, st, nil
			case <-ctx.Done():
				out, _ := rc.readTurn()
				return out, "interrupted", ctx.Err()
			}
		}
		// Non-blocking snapshot (poll): a closed turnDone (parked/ended) reports
		// its terminal state; an open one means a turn is still in flight.
		select {
		case <-turnDone:
			out, st := rc.readTurn()
			return out, st, nil
		default:
			out, _ := rc.readTurn()
			return out, "running", nil
		}
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-turnDone:
		out, st := rc.readTurn()
		return out, st, nil
	case <-timer.C:
		out, _ := rc.readTurn()
		return out, "running", nil
	case <-ctx.Done():
		out, _ := rc.readTurn()
		return out, "interrupted", ctx.Err()
	}
}

// closeResidentChildrenOf cancels every resident child opened by parentAgentID —
// the parent-teardown backstop (a parent that completes/errors without closing
// its children). Called from finishRunWithCancel. Cheap when there are none.
func (s *Server) closeResidentChildrenOf(parentAgentID string) {
	if s.residentReg == nil || parentAgentID == "" {
		return
	}
	for _, rc := range s.residentReg.snapshot() {
		if rc.parentAgentID != parentAgentID {
			continue
		}
		if _, found := s.cancelReg.Cancel(rc.agentID, "parent run ended (resident sub-agent)"); !found && rc.cancel != nil {
			rc.cancel(fmt.Errorf("parent run ended"))
		}
	}
}

// RunResidentSweeper idle-reaps resident children (per-replica; the registry is
// in-process, so no cluster coordination). Started once at boot (main.go, with
// the shutdown ctx). Exported so package main can launch it.
func (s *Server) RunResidentSweeper(ctx context.Context) {
	if s.residentReg == nil {
		return
	}
	t := time.NewTicker(residentSweepInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			s.sweepResidentChildren(now)
		}
	}
}

// sweepResidentChildren reaps resident children by two rules (the per-tick
// body of RunResidentSweeper; separated so tests can drive one sweep without
// wall-clock waiting):
//
//   - idle: no turn running, and nothing has used the child (open/send/poll/
//     cancel, or the end of its last turn) for longer than its idle TTL. A
//     running turn is never idle — a long turn sent with timeout_ms is
//     working, and reaping it would discard the result the parent will poll.
//   - turn ceiling: the current turn has run longer than the ceiling. This is
//     what bounds a turn that never ends; poll does not move its clock.
//
// The reason is logged, carried as the cancel cause into the run's terminal
// record, and kept on the child so the parent's next call says why.
//
// Neither rule runs while the runtime is paused, and the pause does not count
// towards either once it lifts: a paused runtime cancels nothing in flight,
// its parents cannot use their children, and a child reaped in the pause
// would be missing from the snapshot the pause was taken for. The pause is
// timed from the first sweep that sees it, so up to one sweep interval of it
// may still count.
func (s *Server) sweepResidentChildren(now time.Time) {
	if s.residentReg == nil {
		return
	}
	s.residentReg.pruneGone(now)
	if s.runtimePaused() {
		s.residentReg.notePause(now)
		return
	}
	if since := s.residentReg.endPause(); !since.IsZero() {
		for _, rc := range s.residentReg.snapshot() {
			rc.discountPause(since, now)
		}
	}
	for _, rc := range s.residentReg.snapshot() {
		rc.mu.Lock()
		var reason string
		switch {
		case rc.done:
		case rc.running && now.Sub(rc.turnStarted) > rc.maxTurn:
			reason = fmt.Sprintf("turn ceiling: a turn ran longer than %s", rc.maxTurn)
		case !rc.running && now.Sub(rc.lastUsed) > rc.idleTTL:
			reason = fmt.Sprintf("idle timeout: unused for longer than %s", rc.idleTTL)
		}
		if reason != "" && rc.reapReason == "" {
			rc.reapReason = reason
		}
		rc.mu.Unlock()
		if reason == "" {
			continue
		}
		log.Printf("resident child %s reaped: %s", rc.runID, reason)
		if _, found := s.cancelReg.Cancel(rc.agentID, reason+" (resident sub-agent)"); !found && rc.cancel != nil {
			rc.cancel(fmt.Errorf("%s", reason))
		}
	}
}

// --- RFC BK P3: Web-UI visibility + operator control ---

// residentForOperator resolves a resident child by run_id for an operator HTTP
// request, gated by the caller's tenant scope (all = admin/legacy/open). A child
// in another tenant folds into not-found (opaque — run_ids aren't secrets, but
// the gate must not become a cross-tenant existence oracle).
func (s *Server) residentForOperator(runID, scopeTenant string, all bool) (*residentChild, bool) {
	if s.residentReg == nil {
		return nil, false
	}
	rc, ok := s.residentReg.get(runID)
	if !ok {
		return nil, false
	}
	if !all && rc.tenantID != scopeTenant {
		return nil, false
	}
	return rc, true
}

// handleListResident serves GET /v1/_resident — the resident interactive
// sub-agents visible to the caller (RFC BK P3). Tenant-scoped: an operator sees
// its own tenant's; admin/legacy/open see all (or focus one via ?tenant=).
// Per-replica (the registry is in-process; a child is co-located with its run).
func (s *Server) handleListResident(w http.ResponseWriter, r *http.Request) {
	out := make([]residentInfo, 0)
	if s.residentReg != nil {
		scopeTenant, all := s.principalTenantScope(r.Context(), r.URL.Query().Get("tenant"))
		for _, info := range s.residentReg.listInfo() {
			if !all && info.TenantID != scopeTenant {
				continue
			}
			out = append(out, info)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"resident": out})
}

// handleResidentClose serves POST /v1/_resident/{run_id}/close — terminate a
// resident child (RFC BK P3), tenant-gated. Idempotent (unknown/other-tenant → 404).
func (s *Server) handleResidentClose(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("run_id")
	if !validIdent(runID) {
		http.Error(w, "run_id must match [A-Za-z0-9_-]{1,128}", http.StatusBadRequest)
		return
	}
	scopeTenant, all := s.principalTenantScope(r.Context(), "")
	rc, ok := s.residentForOperator(runID, scopeTenant, all)
	if !ok {
		http.Error(w, "no resident sub-agent for that run_id", http.StatusNotFound)
		return
	}
	if _, found := s.cancelReg.Cancel(rc.agentID, "closed by operator (resident sub-agent)"); !found && rc.cancel != nil {
		rc.cancel(fmt.Errorf("closed by operator"))
	}
	writeJSON(w, http.StatusOK, map[string]any{"run_id": runID, "closed": true})
}

// handleResidentCancel serves POST /v1/_resident/{run_id}/cancel — turn-cancel a
// resident child's CURRENT turn (RFC BK P3), tenant-gated. The child stays alive.
func (s *Server) handleResidentCancel(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("run_id")
	if !validIdent(runID) {
		http.Error(w, "run_id must match [A-Za-z0-9_-]{1,128}", http.StatusBadRequest)
		return
	}
	scopeTenant, all := s.principalTenantScope(r.Context(), "")
	rc, ok := s.residentForOperator(runID, scopeTenant, all)
	if !ok {
		http.Error(w, "no resident sub-agent for that run_id", http.StatusNotFound)
		return
	}
	stopped := s.turnCancelReg != nil && s.turnCancelReg.CancelLocal(rc.runID, "cancelled by operator (resident sub-agent)")
	writeJSON(w, http.StatusOK, map[string]any{"run_id": runID, "stopped": stopped})
}
