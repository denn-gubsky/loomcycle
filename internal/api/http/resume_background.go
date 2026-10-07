package http

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/cancel"
	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/runstate"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// A run's background children across a resume.
//
// A poll-mode child (an Agent spawn / parallel_spawn with mode "poll", or a
// TeamDef walk run in poll mode) is filed in its parent's background table,
// which lives in memory. Everything the table knows is also on the parent's
// transcript — the started row, the result row written when the child ended
// (already through the parent's subagent_stop hooks), the read row written
// the first time the parent was handed it, and the ids each children_note
// reported — so a parent resumed on another instance, or at boot, rebuilds
// its table from those rows:
//   - a child with a result row is restored as it ended, read and noted as it
//     was: no hook runs again, and nothing is handed over or reported twice;
//   - a child without one is restored outstanding, and its end is read from
//     its RUN ROW — wherever it runs — when it comes: through the parent's
//     subagent_stop hooks then, once, and recorded, so a later resume finds
//     the result row instead. A child whose row is not there (it never
//     started, or a snapshot did not carry it) ends failed, as a parked
//     fan-out's undispatched child does.
//
// The run-state bus says when a run ends, on any replica that shares the
// backplane, so the parent waits on it rather than polling the store; a slow
// recheck backs it up, since bus delivery is best-effort. A parent that paused
// while it waited for its children is resumed into that wait
// (loop.RunOptions.ResumeAwaitingChildren) and wakes once, when the last has
// ended. Cancelling a restored child cancels its run by id, through the cancel
// registry, which reaches another replica when the cluster is wired.
//
// A team walk does not move. It runs no loop of its own — its members are the
// runs — and lives in the memory of the instance that started it, so it is
// never paused, never carried by a snapshot and never resumed. A walk that
// ended before its parent paused is restored from its result row, its whole
// answer included. One that had not is gone with its instance: its parent
// reads it failed with that reason rather than a missing run, and a row it
// left behind running (a restart on the same database) is closed with it.
//
// A restored child's timeout_ms (on its started row) is re-armed from its run:
// the bound runs from the run's start, the time the run spent held for a
// verdict (its own transcript says when each hold began and ended) is not
// counted, and the time the run spent paused IS — the deadline is the instant
// the live clock would have reached, which a pause does not move. A child past
// it is cancelled as timed out at once; a held one waits for its hold to end.

// The recheck of a restored child's run row backs off from the first to the
// second; it is a backstop for an end the run-state bus did not deliver.
const (
	restoredChildRecheckFirst = time.Second
	restoredChildRecheckMax   = 30 * time.Second
	// restoredChildNotFoundChecks is how many reads in a row must miss a
	// restored child's run row before it is given up as never started or not
	// carried over.
	restoredChildNotFoundChecks = 3
	// restoredChildCancelTimeout bounds one cancel of a restored child's run,
	// a cluster round trip included.
	restoredChildCancelTimeout = 15 * time.Second
)

// pollChildLedger is one poll-mode child as its parent's transcript records it.
type pollChildLedger struct {
	started providers.SpawnChildEventInfo
	result  *providers.SpawnChildEventInfo
	read    bool
	noted   bool
}

// pollLedgerOf reads a run's poll-mode children off its events, in the order
// they were started. The rows are read whatever order they arrived in: a read
// row can be written just before the result row it follows.
func pollLedgerOf(runEvents []store.Event) []*pollChildLedger {
	var order []*pollChildLedger
	byID := map[string]*pollChildLedger{}
	results := map[string]*providers.SpawnChildEventInfo{}
	read, noted := map[string]bool{}, map[string]bool{}
	for _, ev := range runEvents {
		switch ev.Type {
		case string(providers.EventSpawnChildStarted), string(providers.EventSpawnChildResult), string(providers.EventSpawnChildRead):
			var pe providers.Event
			if json.Unmarshal(ev.Payload, &pe) != nil || pe.SpawnChild == nil || pe.SpawnChild.Mode != "poll" || pe.SpawnChild.RunID == "" {
				continue
			}
			sc := *pe.SpawnChild
			switch ev.Type {
			case string(providers.EventSpawnChildStarted):
				if byID[sc.RunID] == nil {
					c := &pollChildLedger{started: sc}
					byID[sc.RunID] = c
					order = append(order, c)
				}
			case string(providers.EventSpawnChildResult):
				results[sc.RunID] = &sc
			default:
				read[sc.RunID] = true
			}
		case string(providers.EventChildrenNote):
			var pe providers.Event
			if json.Unmarshal(ev.Payload, &pe) != nil || pe.ChildrenNote == nil {
				continue
			}
			for _, id := range pe.ChildrenNote.ChildRunIDs {
				noted[id] = true
			}
		}
	}
	for _, c := range order {
		c.result, c.read, c.noted = results[c.started.RunID], read[c.started.RunID], noted[c.started.RunID]
	}
	return order
}

// parkedForChildren reports whether a run paused while it waited for its
// background children: its last awaiting_children was never followed by the
// note that wakes it, nor by anything else the run did.
func parkedForChildren(runEvents []store.Event) bool {
	parked := false
	for _, ev := range runEvents {
		switch ev.Type {
		case string(providers.EventAwaitingChildren):
			parked = true
		case string(providers.EventChildrenNote), "user_input", "text", "tool_call", "tool_result":
			parked = false
		}
	}
	return parked
}

// specOf is the table entry a started row describes.
func specOf(sc providers.SpawnChildEventInfo) tools.ChildSpec {
	spec := tools.ChildSpec{
		RunID: sc.RunID, Agent: sc.Agent, Index: -1, BatchID: sc.BatchID, Kind: sc.Kind,
		Notify: !sc.NoNotify, CancelOnParentEnd: sc.CancelOnParentEnd,
	}
	// A spawn's row records index 0, and only a parallel_spawn has a batch.
	if sc.BatchID != "" {
		spec.Index = sc.Index
	}
	return spec
}

// restoredEnding is the state and result a result row records.
func restoredEnding(r providers.SpawnChildEventInfo) (string, tools.ChildResult) {
	state := r.Ended
	if state == "" {
		state = tools.ChildFailed
		if r.Ok {
			state = tools.ChildCompleted
		}
	}
	return state, tools.ChildResult{Output: r.Output, Error: r.Error, Structured: r.State, Status: r.Status, Detail: walkDetail(r.Detail)}
}

// walkDetail gives a walk's answer, read back from JSON, the shape the live
// one has: its steps a []map[string]any, which is what TeamDef poll's
// per-walk output cap reads. Decoded they are a []any, which the cap would
// not see, and a restored walk would reach the caller uncut.
func walkDetail(d map[string]any) map[string]any {
	steps, ok := d["steps"].([]any)
	if !ok {
		return d
	}
	typed := make([]map[string]any, 0, len(steps))
	for _, s := range steps {
		m, ok := s.(map[string]any)
		if !ok {
			return d
		}
		typed = append(typed, m)
	}
	out := make(map[string]any, len(d))
	for k, v := range d {
		out[k] = v
	}
	out["steps"] = typed
	return out
}

// restoreBackgroundFn is what refills a resumed run's background table from
// its ledger and its open resident children, or nil when it has neither.
func (s *Server) restoreBackgroundFn(parent store.Run, ledger []*pollChildLedger, residents []store.Run) func(context.Context, *tools.Background) {
	if len(ledger) == 0 && len(residents) == 0 {
		return nil
	}
	return func(ctx context.Context, bg *tools.Background) {
		// Filed as live open does, so a poll or cancel naming one by
		// child_run_ids knows it is this run's. Its registry entry is its own
		// resume's (resumePausedRun).
		for _, r := range residents {
			bg.AddResident(r.ID, r.Agent)
		}
		var outstanding []tools.ChildSpec
		bounds := map[string]int{}
		for _, c := range ledger {
			spec := specOf(c.started)
			if c.result != nil {
				state, res := restoredEnding(*c.result)
				bg.Restore(tools.RestoredChild{ChildSpec: spec, State: state, Result: res, Read: c.read, Noted: c.noted})
				continue
			}
			bg.Restore(tools.RestoredChild{ChildSpec: spec, State: tools.ChildRunning, Read: c.read,
				Cancel: func(cause error) { go s.cancelRestoredChild(spec, cause) }})
			outstanding = append(outstanding, spec)
			if c.started.TimeoutMs > 0 && spec.Kind != tools.ChildKindTeam {
				bounds[spec.RunID] = c.started.TimeoutMs
			}
		}
		if len(outstanding) == 0 {
			return
		}
		// They were admitted when they started, and are alive still: the run's
		// next spawn counts them, whatever the limit is now.
		releases := s.liveChildren.Hold(parent.ID, len(outstanding))
		go s.watchRestoredChildren(ctx, bg, parent.UserID, outstanding, bounds, releases)
	}
}

// residentChildrenOf is a resumed run's resident children that had not ended
// when it paused: its child runs whose record marks them resident. A failed
// read is logged and leaves none — the children are still reachable by id
// through the resident registry; only a poll by child_run_ids would not know
// them.
func (s *Server) residentChildrenOf(ctx context.Context, parent store.Run) []store.Run {
	if s.residentReg == nil || parent.AgentID == "" {
		return nil
	}
	runs, err := s.store.ListRunsByParentAgentID(ctx, parent.AgentID)
	if err != nil {
		log.Printf("resume: list run %s's children: %v", parent.ID, err)
		return nil
	}
	var out []store.Run
	for _, r := range runs {
		if r.ParentRunID != parent.ID || isTerminalRunStatus(r.Status) {
			continue
		}
		if rec, ok := decodeRunConfig(r.RunConfig); ok && rec.Spawn != nil && rec.Spawn.Resident {
			out = append(out, r)
		}
	}
	return out
}

// watchRestoredChildren files each restored child's end in the table when its
// run ends, or when its re-armed timeout_ms (bounds, by run id) runs out. It
// lives as long as the parent's run, at most: when the run ends, the table
// cancels what is left, and nothing is waited for any more.
func (s *Server) watchRestoredChildren(ctx context.Context, bg *tools.Background, userID string, children []tools.ChildSpec, bounds map[string]int, releases []func()) {
	pending := make(map[string]int, len(children))
	for i, c := range children {
		pending[c.RunID] = i
	}
	clocks := make(map[string]*restoredClock, len(bounds))
	for id, ms := range bounds {
		clocks[id] = &restoredClock{resumedChildClock: resumedChildClock{bound: time.Duration(ms) * time.Millisecond}}
	}
	defer func() {
		for _, i := range pending {
			releases[i]()
		}
	}()
	// Subscribed before the first read, so an end between the two is not
	// missed. A child runs as its parent's user, on any replica.
	var ends <-chan runstate.RunStateEvent
	if s.runStateBus != nil {
		sub := s.runStateBus.Subscribe(userID)
		defer sub.Close()
		ends = sub.C
	}
	missing := map[string]int{}
	check := func(id string) {
		i, ok := pending[id]
		if !ok {
			return
		}
		state, res, ended := s.restoredChildEnd(ctx, children[i], missing)
		if !ended {
			clk := clocks[id]
			if clk == nil || !s.restoredClockExpired(ctx, id, clk) {
				return
			}
			state, res = s.timeOutRestoredChild(ctx, children[i], bounds[id])
		}
		releases[i]()
		delete(pending, id)
		bg.Finish(id, state, res)
	}
	// The earliest deadline among the pending children whose clock runs.
	var deadlineC <-chan time.Time
	deadlineTimer := time.NewTimer(time.Hour)
	deadlineTimer.Stop()
	defer deadlineTimer.Stop()
	armDeadline := func() {
		deadlineTimer.Stop()
		deadlineC = nil
		var next time.Time
		for id := range pending {
			if clk := clocks[id]; clk != nil && !clk.start.IsZero() {
				if at, held := clk.deadline(clk.start); !held && (next.IsZero() || at.Before(next)) {
					next = at
				}
			}
		}
		if !next.IsZero() {
			deadlineTimer.Reset(max(time.Until(next), 0))
			deadlineC = deadlineTimer.C
		}
	}
	wait, most := restoredChildRecheckFirst, restoredChildRecheckMax
	if s.childRecheckFirst > 0 {
		wait, most = s.childRecheckFirst, max(s.childRecheckFirst, restoredChildRecheckMax)
	}
	timer := time.NewTimer(0)
	defer timer.Stop()
	for len(pending) > 0 {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-ends:
			if !ok {
				ends = nil
				continue
			}
			// A hold or a wait is announced as "running"; only an end is
			// worth a read — or, for a bounded child, a hold's start or end,
			// which stops or restarts its clock.
			if ev.Status != string(store.RunRunning) || clocks[ev.RunID] != nil {
				check(ev.RunID)
				armDeadline()
			}
		case <-deadlineC:
			for id := range pending {
				if clocks[id] != nil {
					check(id)
				}
			}
			armDeadline()
		case <-timer.C:
			for id := range pending {
				check(id)
			}
			armDeadline()
			timer.Reset(wait)
			if wait *= 2; wait > most {
				wait = most
			}
		}
	}
}

// restoredClock is a restored child's timeout_ms, read off its run: the
// fan-out child's clock (the bound from the run's start, plus the time it spent
// held for a verdict), with the start read lazily, once its row is there.
type restoredClock struct {
	resumedChildClock
	start time.Time // the run's start; zero until its row is read
}

// restoredClockExpired brings a bounded child's clock up to date from its run
// and reports whether its bound has run out. A run it cannot read yet is not
// timed out: whether it exists at all is restoredChildEnd's to decide.
func (s *Server) restoredClockExpired(ctx context.Context, runID string, clk *restoredClock) bool {
	if clk.start.IsZero() {
		child, err := s.store.GetRun(ctx, runID)
		if err != nil || child.StartedAt.IsZero() {
			return false
		}
		clk.start = child.StartedAt
	}
	for {
		page, err := s.store.GetRunEventsSince(ctx, runID, clk.seq, resumedChildClockPage)
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("resume: read background child %s's events: %v", runID, err)
			}
			return false
		}
		clk.observe(page)
		if len(page) < resumedChildClockPage {
			break
		}
	}
	at, held := clk.deadline(clk.start)
	return !held && !time.Now().Before(at)
}

// timeOutRestoredChild cancels a restored child whose re-armed timeout_ms ran
// out, as the live clock cancels it, and is the ending the parent is handed:
// the timeout, after the parent's subagent_stop hooks see it fail, as they do
// live.
func (s *Server) timeOutRestoredChild(ctx context.Context, spec tools.ChildSpec, timeoutMs int) (string, tools.ChildResult) {
	s.cancelRestoredChild(spec, builtin.ChildTimeoutCause(timeoutMs))
	msg := builtin.ChildTimedOutMessage(spec.Agent, timeoutMs, spec.RunID)
	_, _ = s.subagentStop(ctx, spec.Agent, spec.RunID, string(store.RunFailed), "", errors.New(msg))
	return tools.ChildTimeout, tools.ChildResult{Error: msg, Status: "timeout"}
}

// restoredChildEnd reads a restored child's run row: its ending once the run
// has ended, through the parent's subagent_stop hooks for a sub-agent, as the
// live child's was. missing counts reads in a row that found no row.
func (s *Server) restoredChildEnd(ctx context.Context, spec tools.ChildSpec, missing map[string]int) (string, tools.ChildResult, bool) {
	child, err := s.store.GetRun(ctx, spec.RunID)
	if err != nil {
		var nf *store.ErrNotFound
		if !errors.As(err, &nf) {
			if ctx.Err() == nil {
				log.Printf("resume: read background child %s: %v", spec.RunID, err)
			}
			return "", tools.ChildResult{}, false
		}
		// A walk's run never travels with its parent: no read will find it.
		if spec.Kind == tools.ChildKindTeam {
			return tools.ChildFailed, tools.ChildResult{Error: "run: " + walkInterrupted}, true
		}
		if missing[spec.RunID]++; missing[spec.RunID] < restoredChildNotFoundChecks {
			return "", tools.ChildResult{}, false
		}
		return tools.ChildFailed, tools.ChildResult{Error: fmt.Sprintf(
			"background child %s (%s) has no run here: it never started before its parent paused, or it was not carried over with its parent; start it again if you need its result",
			spec.RunID, spec.Agent)}, true
	}
	delete(missing, spec.RunID)
	if !isTerminalRunStatus(child.Status) {
		return "", tools.ChildResult{}, false
	}
	if spec.Kind == tools.ChildKindTeam {
		state, res := restoredWalkEnding(child, spec)
		return state, res, true
	}
	state, res := s.restoredAgentEnding(ctx, child, spec)
	return state, res, true
}

// restoredAgentEnding is a sub-agent's ending from its run row, as the live
// child would have handed it back: its final text under the sub-agent header
// and its final state, or its failure — and then through the parent's
// subagent_stop hooks, which may refuse it or add to it.
func (s *Server) restoredAgentEnding(ctx context.Context, child store.Run, spec tools.ChildSpec) (string, tools.ChildResult) {
	var rec runResultRecord
	if len(child.Result) > 0 {
		_ = json.Unmarshal(child.Result, &rec)
	}
	var state string
	var res tools.ChildResult
	var runErr error
	switch {
	case child.Status == store.RunCompleted && child.StopReason == loop.StopReasonMaxIterations:
		// Ended at its iteration limit: failed, its last answer kept beside
		// the error, as a live poll child's is.
		text := rec.FinalText
		if text == "" && len(rec.State) == 0 {
			text = s.childFinalText(ctx, child)
		}
		state, res = tools.ChildFailed, tools.ChildResult{Output: formatSubAgentOutput(child.AgentID, child.ID, text), Structured: rec.State, Status: builtin.ChildStatusMaxIterations}
		runErr = errors.New(builtin.ChildCappedMessage(spec.Agent, 0, child.ID))
	case child.Status == store.RunCompleted && !loop.EndsRejected(child.StopReason):
		text := rec.FinalText
		if text == "" && len(rec.State) == 0 {
			text = s.childFinalText(ctx, child) // a row written before runs.result
		}
		if text == "" && len(rec.State) == 0 {
			text = fmt.Sprintf("(sub-agent %q completed with no final text)", spec.Agent)
		}
		state, res = tools.ChildCompleted, tools.ChildResult{Output: formatSubAgentOutput(child.AgentID, child.ID, text), Structured: rec.State}
	case child.Status == store.RunCompleted:
		state = tools.ChildFailed
		runErr = fmt.Errorf("the answer of sub-agent %q was rejected (%s; run=%s)", spec.Agent, child.StopReason, child.ID)
	case child.Status == store.RunCancelled:
		state = tools.ChildCancelled
		runErr = fmt.Errorf("sub-agent %q was cancelled (run=%s): %s", spec.Agent, child.ID, firstNonEmpty(child.StopReason, child.ErrorMsg, string(child.Status)))
	default:
		state = tools.ChildFailed
		runErr = fmt.Errorf("sub-agent %q failed (run=%s): %s", spec.Agent, child.ID, firstNonEmpty(child.ErrorMsg, child.StopReason, string(child.Status)))
	}
	status := string(store.RunCompleted)
	if runErr != nil {
		status = string(store.RunFailed)
	}
	out, err := s.subagentStop(ctx, spec.Agent, child.ID, status, res.Output, runErr)
	switch {
	case err == nil:
		res.Output = out
	case err != runErr:
		// Refused: the hook's reason replaces the answer, as it does live.
		if state == tools.ChildCompleted {
			state = tools.ChildFailed
		}
		res = tools.ChildResult{Error: err.Error()}
	default:
		res.Error = err.Error() // a capped child's answer stays beside it
	}
	return state, res
}

// walkInterrupted is why a walk still running when its parent paused ended.
const walkInterrupted = "the walk was interrupted: the run that started it was paused and resumed without it — a team walk runs on the instance that started it and does not survive a restart or move to another one; run it again if you need its answer"

// endInterruptedWalks closes the run rows of a resumed run's walks that were
// still running when it paused: their walk went with the instance that ran
// it, and its row would otherwise read running until the stale sweeper failed
// it for a missed heartbeat. Closed before the run's own resume returns, so a
// resume pass sees them ended — and does not leave their paused members
// running for a walk that will never read them. A walk live on this instance
// (none, for a run that needed resuming) is left alone.
func (s *Server) endInterruptedWalks(ctx context.Context, ledger []*pollChildLedger) {
	for _, c := range ledger {
		if c.result != nil || c.started.Kind != tools.ChildKindTeam {
			continue
		}
		if _, live := s.walks.get(c.started.RunID); live {
			continue
		}
		walk, err := s.store.GetRun(ctx, c.started.RunID)
		if err != nil || isTerminalRunStatus(walk.Status) {
			continue // not here (its parent reads why), or it ended on its own
		}
		s.finishRunFailedReason(walk.ID, walkInterrupted, runStateMeta{
			RunID: walk.ID, AgentID: walk.AgentID, Agent: walk.Agent, UserID: walk.UserID,
			TenantID: walk.TenantID, ParentRunID: walk.ParentRunID, ParentContext: walk.ParentContext,
		})
	}
}

// restoredWalkEnding is a team walk's ending from its run row. A walk's whole
// answer (its steps) is recorded on its parent's ledger when it ends; a walk
// that ended with no parent to record it — its run was ended for it — has
// only what its row holds: its final output and the end state it reached, or
// why it failed.
func restoredWalkEnding(child store.Run, spec tools.ChildSpec) (string, tools.ChildResult) {
	var rec runResultRecord
	if len(child.Result) > 0 {
		_ = json.Unmarshal(child.Result, &rec)
	}
	name := strings.TrimPrefix(spec.Agent, teamWalkAgentPrefix)
	switch child.Status {
	case store.RunCompleted:
		detail := map[string]any{"name": name, "status": "completed", "final_output": rec.FinalText}
		if rec.Terminal != "" {
			detail["final_state"] = rec.Terminal
		}
		return tools.ChildCompleted, tools.ChildResult{Output: rec.FinalText, Detail: detail}
	case store.RunCancelled:
		return tools.ChildCancelled, tools.ChildResult{Error: "run: the walk was cancelled: " + firstNonEmpty(child.StopReason, child.ErrorMsg, "cancelled")}
	}
	return tools.ChildFailed, tools.ChildResult{Error: "run: " + firstNonEmpty(child.ErrorMsg, child.StopReason, string(child.Status))}
}

// cancelRestoredChild cancels a restored child's run by id, wherever it runs:
// a walk through this replica's walk table (a walk is not reachable on
// another replica), a sub-agent through the cancel registry, which hands a
// run it does not hold to the cluster. A child whose run has ended is left
// alone.
func (s *Server) cancelRestoredChild(spec tools.ChildSpec, cause error) {
	ctx, stop := context.WithTimeout(context.Background(), restoredChildCancelTimeout)
	defer stop()
	reason := cancel.ReasonFromCause(cause)
	if reason == "" && cause != nil {
		reason = cause.Error()
	}
	if spec.Kind == tools.ChildKindTeam {
		if w, ok := s.walks.get(spec.RunID); ok {
			w.cancel(cancel.CauseWithReason(reason))
		}
		return
	}
	if s.store == nil || s.cancelReg == nil {
		return
	}
	child, err := s.store.GetRun(ctx, spec.RunID)
	if err != nil || isTerminalRunStatus(child.Status) || child.AgentID == "" {
		return
	}
	s.cancelReg.Cancel(child.AgentID, reason)
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}
