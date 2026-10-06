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
// its ledger, or nil when it started no poll-mode child.
func (s *Server) restoreBackgroundFn(parent store.Run, ledger []*pollChildLedger) func(context.Context, *tools.Background) {
	if len(ledger) == 0 {
		return nil
	}
	return func(ctx context.Context, bg *tools.Background) {
		var outstanding []tools.ChildSpec
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
		}
		if len(outstanding) == 0 {
			return
		}
		// They were admitted when they started, and are alive still: the run's
		// next spawn counts them, whatever the limit is now.
		releases := s.liveChildren.Hold(parent.ID, len(outstanding))
		go s.watchRestoredChildren(ctx, bg, parent.UserID, outstanding, releases)
	}
}

// watchRestoredChildren files each restored child's end in the table when its
// run ends. It lives as long as the parent's run, at most: when the run ends,
// the table cancels what is left, and nothing is waited for any more.
func (s *Server) watchRestoredChildren(ctx context.Context, bg *tools.Background, userID string, children []tools.ChildSpec, releases []func()) {
	pending := make(map[string]int, len(children))
	for i, c := range children {
		pending[c.RunID] = i
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
			return
		}
		releases[i]()
		delete(pending, id)
		bg.Finish(id, state, res)
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
			// worth a read.
			if ev.Status != string(store.RunRunning) {
				check(ev.RunID)
			}
		case <-timer.C:
			for id := range pending {
				check(id)
			}
			timer.Reset(wait)
			if wait *= 2; wait > most {
				wait = most
			}
		}
	}
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
		res = tools.ChildResult{Error: err.Error()}
	}
	return state, res
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
