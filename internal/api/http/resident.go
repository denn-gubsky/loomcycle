package http

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/cancel"
	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/steer"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
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
	// residentTombstoneTTL is how long how a child ended stays answerable
	// after its teardown removed it: long enough for a parent busy elsewhere
	// to poll and read it, short enough that the map stays small.
	residentTombstoneTTL = time.Hour
	// residentTombstoneMax bounds how many endings are kept at once, the
	// oldest dropped first. Each holds at most one turn's output, which the
	// live child held already; 256 is far more endings than the parents of
	// one process read back within the TTL.
	residentTombstoneMax  = 256
	residentSweepInterval = 60 * time.Second
	// residentCancelReparkTimeout bounds how long op=cancel waits for the child's
	// loop to re-park after its turn is stopped (RFC BK P2). The re-park is near-
	// instant once the turn ctx cancels; this is only a backstop.
	residentCancelReparkTimeout = 15 * time.Second
)

// The reasons a resident child's run is ended for it. Each is carried as its
// run's cancel cause and recorded as the run's stop_reason, so how the child
// ended reads the same from the cause on the replica that ran it and from the
// row anywhere else (residentEndedFor).
const (
	residentReasonSuffix           = " (resident sub-agent)"
	residentReasonClosedByParent   = "closed by parent" + residentReasonSuffix
	residentReasonClosedByOperator = "closed by operator" + residentReasonSuffix
	residentReasonParentEnded      = "parent run ended" + residentReasonSuffix
	// The sweeper's reasons start with these, then say how long.
	residentReapIdle    = "idle timeout:"
	residentReapCeiling = "turn ceiling:"
)

// residentEndedFor reads how a resident child's run was ended for it from the
// reason it was cancelled with: reapReason when the sweeper reaped it,
// closedBy when it was closed or cancelled otherwise. Both are "" for "".
func residentEndedFor(reason string) (reapReason, closedBy string) {
	switch reason {
	case "":
		return "", ""
	case residentReasonClosedByParent:
		return "", "closed by its parent"
	case residentReasonClosedByOperator:
		return "", "closed by the operator"
	case residentReasonParentEnded:
		return "", "closed when its parent run ended"
	}
	if r, ok := strings.CutSuffix(reason, residentReasonSuffix); ok &&
		(strings.HasPrefix(r, residentReapIdle) || strings.HasPrefix(r, residentReapCeiling)) {
		return r, ""
	}
	return "", "cancelled (" + reason + ")"
}

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
	closedBy    string // who closed it, read from its run's cancel cause at its end
	// startupPark marks a child resumed parked whose loop has not yet parked:
	// the awaiting_input it announces then is the park it was already in, not
	// the end of a turn — a send that arrived first must not end on it.
	startupPark bool
	// capped is set when the child's run ended at its iteration limit: its
	// last turn is handed back as that error, not as a completed turn.
	capped *builtin.ChildCappedError
}

// beginTurn resets the per-turn buffer + wake channel. Called before open's
// first turn and before every send. Returns the channel the caller waits on.
func (rc *residentChild) beginTurn(now time.Time) <-chan struct{} {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return rc.beginTurnLocked(now)
}

func (rc *residentChild) beginTurnLocked(now time.Time) <-chan struct{} {
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
	case providers.EventSteer:
		// The child took an instruction. A send made here began its turn
		// before pushing it; one that reached this child's queue from another
		// replica (routed by the steer coordinator) began none, so its turn
		// begins now — or its answer would run on from the last turn's, and
		// the idle rule would see a working child as unused.
		rc.mu.Lock()
		if !rc.running {
			rc.beginTurnLocked(time.Now())
		}
		rc.mu.Unlock()
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

func (rc *residentChild) markDone(state string, capped *builtin.ChildCappedError) {
	rc.mu.Lock()
	rc.done = true
	rc.capped = capped
	rc.mu.Unlock()
	rc.endTurn(state) // wake a waiter blocked on the final (non-parking) turn
}

// residentEnding is how a resident child stands at a hand-back: why the
// sweeper reaped it ("" if it did not), whether its run has ended, and the
// iteration-limit error it ended with (nil unless it did).
type residentEnding struct {
	reapReason string
	ended      bool
	capped     *builtin.ChildCappedError
}

func (rc *residentChild) ending() residentEnding {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return residentEnding{reapReason: rc.reapReason, ended: rc.done, capped: rc.capped}
}

// noteCancelled records how the child's run was ended for it, from the cause
// it was cancelled with — the reason its row's stop_reason also records — so
// a close that reached it from anywhere, another replica included, reads as
// one. A run that was not cancelled, or a reap the sweeper already noted, is
// left as it is.
func (rc *residentChild) noteCancelled(cause error) {
	if !errors.Is(cause, cancel.ErrCancelledByAPI) {
		return
	}
	reason := cancel.ReasonFromCause(cause)
	if reason == "" {
		reason = "cancelled by api" // what finishRunWithCancel records for it
	}
	reap, closed := residentEndedFor(reason)
	rc.mu.Lock()
	defer rc.mu.Unlock()
	if rc.reapReason == "" && rc.closedBy == "" {
		rc.reapReason, rc.closedBy = reap, closed
	}
}

// tombstone is the child's ending as its parent reads it after the run left
// the registry.
func (rc *residentChild) tombstone(now time.Time) residentTombstone {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return residentTombstone{
		tenantID: rc.tenantID, userID: rc.userID, agentName: rc.agentName,
		reason: rc.reapReason, closedBy: rc.closedBy,
		state: rc.state, output: rc.buf.String(), capped: rc.capped, at: now,
	}
}

func (rc *residentChild) isDone() bool {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return rc.done
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
	// gone keeps how a child ended after its teardown removed it — reaped,
	// closed, or its run ended on its own with its last turn's output — so a
	// parent's later poll reads that ending and a send learns the child has
	// ended, instead of a bare not-found. Like the registry it is per-process:
	// a parent polling on another replica gets not-found. Bounded by
	// residentTombstoneMax and pruned by the sweeper after
	// residentTombstoneTTL.
	gone map[string]residentTombstone
	// pausedSince is when the sweeper first saw the runtime paused; zero
	// while it runs. See sweepResidentChildren.
	pausedSince time.Time
}

type residentTombstone struct {
	tenantID  string
	userID    string
	agentName string
	reason    string // why the sweeper reaped it; "" if it did not
	closedBy  string // who closed it; "" if it was not closed
	state     string // its run's final state: "completed" | "failed"
	output    string // its last turn's output
	capped    *builtin.ChildCappedError
	at        time.Time
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

// remove drops a torn-down child and keeps its ending. The two happen under
// one lock, so a lookup finds the child either live or ended, never neither.
func (r *residentRegistry) remove(rc *residentChild) { r.removeAt(rc, time.Now()) }

func (r *residentRegistry) removeAt(rc *residentChild, now time.Time) {
	t := rc.tombstone(now)
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.m, rc.runID)
	r.gone[rc.runID] = t
	for len(r.gone) > residentTombstoneMax {
		oldest := ""
		for id, g := range r.gone {
			if oldest == "" || g.at.Before(r.gone[oldest].at) {
				oldest = id
			}
		}
		delete(r.gone, oldest)
	}
}

// ended returns how a child that is no longer registered ended, for a caller
// that could have addressed it alive (residentOwnedBy); false when no ending
// is kept or the caller could not.
func (r *residentRegistry) ended(runID string, caller tools.RunIdentityValue) (residentTombstone, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if t, ok := r.gone[runID]; ok && residentOwnedBy(t.tenantID, t.userID, caller) {
		return t, true
	}
	return residentTombstone{}, false
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
	// The parent's end closes the child but does not wait for it, so the tree
	// it works in is held until its goroutine has returned.
	releaseTree := s.holdRunTree(parent.RootRunID)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("resident child %s panicked: %v", prep.RunID, r)
				rc.markDone("failed", nil)
			}
			deregSteer()
			prep.cleanup()
			prep.Slot.releaseCurrent()
			s.residentReg.remove(rc)
			live[0]()
			releaseTree()
		}()
		res, runErr := loop.Run(prep.LoopCtx, prep.Opts)
		st := "completed"
		if runErr != nil {
			st = "failed"
			prep.Emit(runErrorEvent(runErr))
		}
		s.finishRunWithCancel(context.WithoutCancel(prep.SteerCtx), prep.SteerCtx, prep.RunID, res, runErr, prep.Meta)
		// Recorded completed, but the child did not get to finish its turn:
		// its parent is told so, as a spawned child's parent is.
		var capped *builtin.ChildCappedError
		if runErr == nil && res.StopReason == loop.StopReasonMaxIterations {
			capped = &builtin.ChildCappedError{Name: name, Limit: iterationLimitOf(prep.Opts), RunID: prep.RunID}
		}
		rc.noteCancelled(context.Cause(prep.SteerCtx))
		rc.markDone(st, capped)
	}()

	out, state, aerr := rc.awaitTurn(ctx, turnDone, time.Duration(timeoutMs)*time.Millisecond, true)
	out, aerr = s.residentHandBack(ctx, rc.runID, rc.agentName, rc.ending(), out, state, aerr)
	return prep.RunID, out, state, aerr
}

// residentHandBack passes what a resident child hands its parent through the
// parent's subagent_stop hooks, as a one-shot child's result is. A resident
// child hands back an output at every turn, not once at the end, so the hooks
// run on each, with the child's state as the status. A refusal of an open
// child's turn leaves it open: the parent may send again, or close it. A
// refusal of the last answer of a child whose run has ended says so instead
// — there is nothing left to send to.
//
// A turn that ended because the child stopped at its iteration limit is
// handed back as a builtin.ChildCappedError carrying the turn's output, with
// status failed for the hooks. A child whose run has ended and left the
// registry hands back its kept ending the same way (readEndedResident).
func (s *Server) residentHandBack(ctx context.Context, runID, agentName string, e residentEnding, out, state string, err error) (string, error) {
	if e.reapReason != "" && err == nil {
		err = residentReapedErr(runID, e.reapReason)
	}
	status := state
	if e.capped != nil && state == "completed" && err == nil {
		capped := *e.capped
		capped.Output = out
		err, status = &capped, string(store.RunFailed)
	}
	out, herr := s.subagentStop(ctx, agentName, runID, status, out, err)
	if herr != nil && herr != err {
		if e.ended {
			return "", fmt.Errorf("%w (child_run_id %s has ended and this was its last answer: open a new one to go on)", herr, runID)
		}
		return "", fmt.Errorf("%w (child_run_id %s is still open: send again or close it)", herr, runID)
	}
	return out, herr
}

// sendResidentChild injects the next instruction into a resident child and waits
// for that turn (RFC BK P2: timeoutMs bounds the wait — 0 blocks until re-park,
// >0 returns state "running" + the partial output if the turn is still going).
func (s *Server) sendResidentChild(ctx context.Context, childRunID, prompt string, timeoutMs int) (string, string, error) {
	rc, ok := s.lookupOwnedResident(ctx, childRunID)
	if !ok {
		return "", "", s.sendToEndedResident(ctx, childRunID)
	}
	// Its run has ended but its teardown has not yet removed it: nothing
	// would take the instruction.
	if rc.isDone() {
		return "", "", residentEndedSendErr(childRunID, rc.tombstone(time.Now()))
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
	out, err = s.residentHandBack(ctx, rc.runID, rc.agentName, rc.ending(), out, state, err)
	return out, state, err
}

// pollResidentChild checks a resident child without sending new input (RFC BK P2)
// — returns its current output-so-far + state. timeoutMs=0 is a non-blocking
// snapshot; >0 waits up to that long for the child to park.
func (s *Server) pollResidentChild(ctx context.Context, childRunID string, timeoutMs int) (string, string, error) {
	rc, ok := s.lookupOwnedResident(ctx, childRunID)
	if !ok {
		return s.readEndedResident(ctx, childRunID)
	}
	rc.touch(time.Now())
	td, _ := rc.currentTurnDone()
	if td == nil {
		// No turn has ever started (shouldn't happen post-open) — report state.
		out, st := rc.readTurn()
		out, err := s.residentHandBack(ctx, rc.runID, rc.agentName, rc.ending(), out, st, nil)
		return out, st, err
	}
	out, state, err := rc.awaitTurn(ctx, td, time.Duration(timeoutMs)*time.Millisecond, false)
	out, err = s.residentHandBack(ctx, rc.runID, rc.agentName, rc.ending(), out, state, err)
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
		// Its run has ended: there is no turn to stop, only its ending to read.
		return s.readEndedResident(ctx, childRunID)
	}
	rc.touch(time.Now())
	td, running := rc.currentTurnDone()
	if !running {
		out, st := rc.readTurn() // already parked/idle — nothing to cancel
		out, err := s.residentHandBack(ctx, rc.runID, rc.agentName, rc.ending(), out, st, nil)
		return out, st, err
	}
	if s.turnCancelReg == nil || !s.turnCancelReg.CancelLocal(childRunID, "cancelled by parent (resident sub-agent)") {
		// Not armed / token vanished (the turn just ended) — treat as parked.
		out, st := rc.readTurn()
		out, err := s.residentHandBack(ctx, rc.runID, rc.agentName, rc.ending(), out, st, nil)
		return out, st, err
	}
	// Wait (bounded) for the loop to re-park after the turn is stopped.
	out, state, err := rc.awaitTurn(ctx, td, residentCancelReparkTimeout, false)
	out, err = s.residentHandBack(ctx, rc.runID, rc.agentName, rc.ending(), out, state, err)
	return out, state, err
}

// closeResidentChild finalizes a resident child (idempotent). Cancelling the
// child's loop ctx terminates it and fires its goroutine teardown.
func (s *Server) closeResidentChild(ctx context.Context, childRunID string) error {
	rc, ok := s.lookupOwnedResident(ctx, childRunID)
	if !ok {
		return nil // idempotent: already gone (or not ours → opaque)
	}
	if _, found := s.cancelReg.Cancel(rc.agentID, residentReasonClosedByParent); !found && rc.cancel != nil {
		rc.cancel(cancel.CauseWithReason(residentReasonClosedByParent))
	}
	return nil
}

// endedResident returns the kept ending of a child_run_id with no live child.
// The error is nil only for a run that ended on its own, whose last turn can
// still be read: a reaped or closed child answers with why, an id with no
// ending kept (or that the caller could not address) with not-found.
func (s *Server) endedResident(ctx context.Context, childRunID string) (residentTombstone, error) {
	var t residentTombstone
	ok := false
	if s.residentReg != nil {
		t, ok = s.residentReg.ended(childRunID, tools.RunIdentity(ctx))
	}
	if !ok {
		return t, fmt.Errorf("resident sub-agent %q not found (it may have been closed or timed out)", childRunID)
	}
	return t, t.endedForItErr(childRunID)
}

// endedForItErr is why a reaped or closed child's run was ended for it; nil
// when it ended on its own.
func (t residentTombstone) endedForItErr(childRunID string) error {
	switch {
	case t.reason != "":
		return residentReapedErr(childRunID, t.reason)
	case t.closedBy != "":
		return fmt.Errorf("resident sub-agent %q was %s; open a new one", childRunID, t.closedBy)
	}
	return nil
}

// readEndedResident answers poll (and cancel) for a child whose run has ended
// and left the registry: its last turn handed back as a poll of it read it
// once the run ended, the iteration-limit error included.
func (s *Server) readEndedResident(ctx context.Context, childRunID string) (string, string, error) {
	t, err := s.endedResident(ctx, childRunID)
	if err != nil {
		return "", "", err
	}
	out, err := s.residentHandBack(ctx, childRunID, t.agentName, residentEnding{ended: true, capped: t.capped}, t.output, t.state, nil)
	return out, t.state, err
}

// sendToEndedResident is send's answer for a child_run_id with no live child.
func (s *Server) sendToEndedResident(ctx context.Context, childRunID string) error {
	t, err := s.endedResident(ctx, childRunID)
	if err != nil {
		return err
	}
	return residentEndedSendErr(childRunID, t)
}

// residentEndedSendErr tells a send that the child's run has ended.
func residentEndedSendErr(childRunID string, t residentTombstone) error {
	if err := t.endedForItErr(childRunID); err != nil {
		return err
	}
	how := "its run " + t.state
	if t.capped != nil {
		how = "it stopped at its iteration limit"
	}
	return fmt.Errorf("resident sub-agent %q has ended (%s) and takes no more sends; poll it to read its last answer, or open a new one", childRunID, how)
}

func residentReapedErr(childRunID, reason string) error {
	return fmt.Errorf("resident sub-agent %q was reaped by the runtime (%s); open a new one", childRunID, reason)
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
		if _, found := s.cancelReg.Cancel(rc.agentID, residentReasonParentEnded); !found && rc.cancel != nil {
			rc.cancel(cancel.CauseWithReason(residentReasonParentEnded))
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
			reason = fmt.Sprintf("%s a turn ran longer than %s", residentReapCeiling, rc.maxTurn)
		case !rc.running && now.Sub(rc.lastUsed) > rc.idleTTL:
			reason = fmt.Sprintf("%s unused for longer than %s", residentReapIdle, rc.idleTTL)
		}
		if reason != "" && rc.reapReason == "" {
			rc.reapReason = reason
		}
		rc.mu.Unlock()
		if reason == "" {
			continue
		}
		log.Printf("resident child %s reaped: %s", rc.runID, reason)
		if _, found := s.cancelReg.Cancel(rc.agentID, reason+residentReasonSuffix); !found && rc.cancel != nil {
			rc.cancel(cancel.CauseWithReason(reason + residentReasonSuffix))
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
	if _, found := s.cancelReg.Cancel(rc.agentID, residentReasonClosedByOperator); !found && rc.cancel != nil {
		rc.cancel(cancel.CauseWithReason(residentReasonClosedByOperator))
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
