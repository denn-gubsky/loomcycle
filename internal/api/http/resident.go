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
	defaultMaxResidentChildren    = 8
	defaultResidentChildIdleTTLMs = 30 * 60 * 1000 // 30 min
	residentSweepInterval         = 60 * time.Second
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

	mu         sync.Mutex
	buf        strings.Builder // assistant text accumulated for the CURRENT turn
	state      string          // "awaiting_input" | "completed" | "failed"
	turnDone   chan struct{}   // closed once when the current turn parks or the run ends
	turnClosed bool
	running    bool // a turn is in flight (between beginTurn and park/terminal) — RFC BK P2
	lastUsed   time.Time
	done       bool // loop goroutine exited
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
	return rc.turnDone
}

func (rc *residentChild) appendText(t string) {
	rc.mu.Lock()
	rc.buf.WriteString(t)
	rc.mu.Unlock()
}

// endTurn records the turn's terminal state and wakes the waiter exactly once.
// Idempotent per turn: the park boundary (fwd) and the loop-exit both call it;
// whichever comes first wins, the second is a no-op.
func (rc *residentChild) endTurn(state string) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	rc.state = state
	rc.running = false
	if !rc.turnClosed && rc.turnDone != nil {
		rc.turnClosed = true
		close(rc.turnDone)
	}
}

// currentTurnDone returns the channel the in-flight (or just-ended) turn signals
// on, and whether a turn is currently running. Used by poll/cancel, which don't
// start a new turn.
func (rc *residentChild) currentTurnDone() (<-chan struct{}, bool) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return rc.turnDone, rc.running
}

func (rc *residentChild) markDone(state string) {
	rc.mu.Lock()
	rc.done = true
	rc.mu.Unlock()
	rc.endTurn(state) // wake a waiter blocked on the final (non-parking) turn
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
}

func newResidentRegistry() *residentRegistry {
	return &residentRegistry{m: map[string]*residentChild{}}
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

func (r *residentRegistry) remove(runID string) {
	r.mu.Lock()
	delete(r.m, runID)
	r.mu.Unlock()
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
	if s.cfg != nil && s.cfg.Env.MaxInteractiveChildren > 0 {
		return s.cfg.Env.MaxInteractiveChildren
	}
	return defaultMaxResidentChildren
}

func (s *Server) residentChildIdleTTL() time.Duration {
	if s.cfg != nil && s.cfg.Env.InteractiveChildIdleTTLMs > 0 {
		return time.Duration(s.cfg.Env.InteractiveChildIdleTTLMs) * time.Millisecond
	}
	return time.Duration(defaultResidentChildIdleTTLMs) * time.Millisecond
}

// openResidentChild starts a resident interactive sub-run, runs its first turn,
// parks it at awaiting_input, and returns (childRunID, firstOutput, state).
func (s *Server) openResidentChild(ctx context.Context, name, prompt, defID string, idleTTLSeconds int) (string, string, string, error) {
	if s.residentReg == nil || s.steerReg == nil {
		// A resident child parks on its steer queue between turns; without the
		// steer registry it could not park (nor could send reach it).
		return "", "", "", fmt.Errorf("resident sub-agents are not enabled on this runtime")
	}
	parent := tools.RunIdentity(ctx)
	if cap := s.maxResidentChildren(); s.residentReg.countByParent(parent.AgentID) >= cap {
		return "", "", "", fmt.Errorf("resident sub-agent cap reached (%d open for this run); close one before opening another", cap)
	}

	rc := &residentChild{parentAgentID: parent.AgentID, idleTTL: s.residentChildIdleTTL()}
	if idleTTLSeconds > 0 {
		rc.idleTTL = time.Duration(idleTTLSeconds) * time.Second
	}
	// Capturing emit: accumulate the child's assistant text for the current turn;
	// the awaiting_input boundary ends the turn and wakes the waiter.
	fwd := func(ev providers.Event) {
		switch ev.Type {
		case providers.EventText:
			rc.appendText(ev.Text)
		case providers.EventAwaitingInput:
			rc.endTurn("awaiting_input")
		}
	}
	// The child must SURVIVE this tool call returning → detach its ctx from the
	// parent request's cancellation (keep values) before prepareSubRun wraps it
	// in its own cancel scope (fired by close / idle-reap / parent teardown).
	prep, err := s.prepareSubRun(context.WithoutCancel(ctx), name, prompt, defID, true, fwd)
	if err != nil {
		return "", "", "", err
	}
	rc.runID = prep.RunID
	rc.agentID = prep.AgentID
	rc.agentName = name
	rc.tenantID = prep.TenantID
	rc.userID = prep.UserID
	rc.cancel = prep.CancelFn

	steerQ, onSteer, deregSteer := s.makeSteer(prep.SteerCtx, prep.RunID, prep.AgentID, prep.SessionID, prep.UserID, prep.Emit)
	prep.Opts.Interactive = true
	prep.Opts.SteerQueue = steerQ
	prep.Opts.OnSteer = onSteer
	prep.Opts.ArmTurnCancel = s.armTurnCancel(prep.RunID)

	turnDone := rc.beginTurn(time.Now())
	s.residentReg.add(rc)

	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("resident child %s panicked: %v", prep.RunID, r)
				rc.markDone("failed")
			}
			deregSteer()
			prep.cleanup()
			prep.Slot.releaseCurrent()
			s.residentReg.remove(prep.RunID)
		}()
		res, runErr := loop.Run(prep.LoopCtx, prep.Opts)
		st := "completed"
		if runErr != nil {
			st = "failed"
			prep.Emit(providers.Event{Type: providers.EventError, Error: runErr.Error()})
		}
		s.finishRunWithCancel(context.WithoutCancel(prep.SteerCtx), prep.SteerCtx, prep.RunID, res, runErr, prep.Meta)
		rc.markDone(st)
	}()

	// open always blocks for the FIRST park (no timeout) — by the time it returns
	// the child is parked and ready for the first send.
	out, state, aerr := rc.awaitTurn(ctx, turnDone, 0, true)
	return prep.RunID, out, state, aerr
}

// sendResidentChild injects the next instruction into a resident child and waits
// for that turn (RFC BK P2: timeoutMs bounds the wait — 0 blocks until re-park,
// >0 returns state "running" + the partial output if the turn is still going).
func (s *Server) sendResidentChild(ctx context.Context, childRunID, prompt string, timeoutMs int) (string, string, error) {
	rc, ok := s.lookupOwnedResident(ctx, childRunID)
	if !ok {
		return "", "", fmt.Errorf("resident sub-agent %q not found (it may have been closed or timed out)", childRunID)
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
	return rc.awaitTurn(ctx, turnDone, time.Duration(timeoutMs)*time.Millisecond, true)
}

// pollResidentChild checks a resident child without sending new input (RFC BK P2)
// — returns its current output-so-far + state. timeoutMs=0 is a non-blocking
// snapshot; >0 waits up to that long for the child to park.
func (s *Server) pollResidentChild(ctx context.Context, childRunID string, timeoutMs int) (string, string, error) {
	rc, ok := s.lookupOwnedResident(ctx, childRunID)
	if !ok {
		return "", "", fmt.Errorf("resident sub-agent %q not found (it may have been closed or timed out)", childRunID)
	}
	td, _ := rc.currentTurnDone()
	if td == nil {
		// No turn has ever started (shouldn't happen post-open) — report state.
		out, st := rc.readTurn()
		return out, st, nil
	}
	return rc.awaitTurn(ctx, td, time.Duration(timeoutMs)*time.Millisecond, false)
}

// cancelResidentChildTurn turn-cancels a resident child's CURRENT turn (RFC BK
// P2): it fires the child's armed turn-cancel token so the loop stops the
// in-flight turn and re-parks at awaiting_input — the child stays alive. The
// child is co-located (fire the local token directly; ownership already gated by
// lookupOwnedResident). A child that isn't mid-turn is a no-op.
func (s *Server) cancelResidentChildTurn(ctx context.Context, childRunID string) (string, string, error) {
	rc, ok := s.lookupOwnedResident(ctx, childRunID)
	if !ok {
		return "", "", fmt.Errorf("resident sub-agent %q not found (it may have been closed or timed out)", childRunID)
	}
	td, running := rc.currentTurnDone()
	if !running {
		out, st := rc.readTurn() // already parked/idle — nothing to cancel
		return out, st, nil
	}
	if s.turnCancelReg == nil || !s.turnCancelReg.CancelLocal(childRunID, "cancelled by parent (resident sub-agent)") {
		// Not armed / token vanished (the turn just ended) — treat as parked.
		out, st := rc.readTurn()
		return out, st, nil
	}
	// Wait (bounded) for the loop to re-park after the turn is stopped.
	return rc.awaitTurn(ctx, td, residentCancelReparkTimeout, false)
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

// lookupOwnedResident resolves a child by run_id and enforces tenant ownership
// (a cross-tenant caller gets a not-found, never another tenant's child).
func (s *Server) lookupOwnedResident(ctx context.Context, childRunID string) (*residentChild, bool) {
	if s.residentReg == nil {
		return nil, false
	}
	rc, ok := s.residentReg.get(childRunID)
	if !ok {
		return nil, false
	}
	if rc.tenantID != tools.RunIdentity(ctx).TenantID {
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

// sweepResidentChildren cancels every resident child idle past its TTL (the
// per-tick body of RunResidentSweeper; separated so tests can drive one sweep
// without wall-clock waiting).
func (s *Server) sweepResidentChildren(now time.Time) {
	if s.residentReg == nil {
		return
	}
	for _, rc := range s.residentReg.snapshot() {
		rc.mu.Lock()
		idle := now.Sub(rc.lastUsed) > rc.idleTTL
		done := rc.done
		rc.mu.Unlock()
		if done || !idle {
			continue
		}
		log.Printf("resident child %s idle-reaped after %s", rc.runID, rc.idleTTL)
		if _, found := s.cancelReg.Cancel(rc.agentID, "idle timeout (resident sub-agent)"); !found && rc.cancel != nil {
			rc.cancel(fmt.Errorf("idle timeout"))
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
