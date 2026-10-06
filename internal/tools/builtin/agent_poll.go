package builtin

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	runcancel "github.com/denn-gubsky/loomcycle/internal/cancel"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// Poll mode: spawn and parallel_spawn hand back child_run_ids once their
// children are admitted, and the children run in the background while the
// parent goes on working. The run's background table (tools.Background,
// owned by the loop) keeps each child's state and result until the parent
// reads it with poll; the loop tells the parent when children end and does
// not let it finish while one is still running.

// DefaultPollWaitCapMs is the longest one poll waits when the operator set no
// cap of their own.
const DefaultPollWaitCapMs = 60_000

// cancelSettle bounds how long cancel waits for the children it cancelled to
// end, so it can report their final states. A cancelled child stops at its
// next await; this is only a backstop.
const cancelSettle = 15 * time.Second

// pollMode is a spawn's mode and what applies to it in poll mode.
type pollMode struct {
	poll        bool
	notify      bool
	cancelOnEnd bool
}

// pollModeOf validates mode, notify and on_parent_end.
func pollModeOf(in agentInput) (pollMode, tools.Result, bool) {
	var pm pollMode
	switch in.Mode {
	case "", "wait":
	case "poll":
		pm.poll = true
	default:
		return pm, errValidation(fmt.Sprintf("unknown mode %q (expected \"wait\" or \"poll\")", in.Mode), ""), false
	}
	if !pm.poll && (in.Notify != nil || in.OnParentEnd != "") {
		return pm, errValidation("notify and on_parent_end apply to mode \"poll\" only", "Add \"mode\": \"poll\", or drop them."), false
	}
	switch in.OnParentEnd {
	case "", "wait":
	case "cancel":
		pm.cancelOnEnd = true
	default:
		return pm, errValidation(fmt.Sprintf("unknown on_parent_end %q (expected \"wait\" or \"cancel\")", in.OnParentEnd), ""), false
	}
	pm.notify = in.Notify == nil || *in.Notify
	return pm, tools.Result{}, true
}

// backgroundFor is the calling run's background table, or why poll mode is
// refused: a run with no table (a stateful run, or a call from outside any
// run), or one on its last iteration, which has no turn left in which to
// collect a child. noTable and lastIteration are the fixes the refusals offer,
// which name the calling tool's own waiting form.
func backgroundFor(ctx context.Context, noTable, lastIteration string) (*tools.Background, tools.Result, bool) {
	bg := tools.BackgroundOf(ctx)
	if bg == nil {
		return nil, errBusiness("mode \"poll\" is not available in this run", noTable), false
	}
	if b, ok := tools.IterationBudget(ctx); ok && !b.Unbounded && b.Used >= b.Max {
		return nil, errBusiness("mode \"poll\" is refused on your last iteration: you would have no turn left to collect the children",
			lastIteration), false
	}
	return bg, tools.Result{}, true
}

// The fixes the Agent tool's poll-mode refusals offer.
const (
	agentPollNoTable       = "Use mode \"wait\" (the default)."
	agentPollLastIteration = "Use mode \"wait\" so the call returns their results, or do the work yourself."
)

// bgEntry is one child a poll-mode call starts.
type bgEntry struct {
	name, prompt, defID string
	compaction          *config.Compaction
	timeoutMs           int
	index               int // -1 for a spawn
}

// pollStartRow is one child in a poll-mode call's answer.
type pollStartRow struct {
	Index      *int   `json:"index,omitempty"`
	Agent      string `json:"agent"`
	ChildRunID string `json:"child_run_id"`
	State      string `json:"state"`
}

func newBatchID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "fan_" + hex.EncodeToString(b[:])
}

// spawnInBackground starts entries as background children of the calling
// run and answers with their ids. live holds one admitted live-children slot
// per entry; each is released when its child ENDS, not when this returns. At
// most concurrencyCap run at once; the rest are queued, and the queue keeps
// draining after the call has returned. batchID is "" for a spawn.
func (a *AgentTool) spawnInBackground(ctx context.Context, bg *tools.Background, pm pollMode, live []func(), entries []bgEntry, concurrencyCap int, batchID string) (tools.Result, error) {
	subCtx := IncrementAgentDepth(ctx)
	toolUseID := tools.ToolUseID(ctx)
	emit := tools.EventEmitter(ctx)
	sem := make(chan struct{}, concurrencyCap)
	rows := make([]pollStartRow, 0, len(entries))
	for i, e := range entries {
		runID := store.NewRunID()
		cctx, err := bg.Start(subCtx, tools.ChildSpec{
			RunID: runID, Agent: e.name, Index: e.index, BatchID: batchID,
			Notify: pm.notify, CancelOnParentEnd: pm.cancelOnEnd,
		})
		if err != nil {
			for _, rel := range live[i:] {
				rel()
			}
			return errBusiness(err.Error(), ""), nil
		}
		// The spawn ledger, so a later resume can rebuild the run's handles.
		if toolUseID != "" && tools.HasEventEmitter(ctx) {
			idx := e.index
			if idx < 0 {
				idx = 0
			}
			emit(providers.Event{Type: providers.EventSpawnChildStarted, SpawnChild: &providers.SpawnChildEventInfo{
				ToolUseID: toolUseID, Index: idx, RunID: runID, Agent: e.name, Mode: "poll", BatchID: batchID,
			}})
		}
		// A slot taken here, before returning, makes the reported state true:
		// running when it has one, queued when it waits for a sibling.
		state := tools.ChildQueued
		select {
		case sem <- struct{}{}:
			state = tools.ChildRunning
			bg.SetState(runID, state)
		default:
		}
		go a.runBackgroundChild(cctx, bg, sem, state == tools.ChildRunning, live[i], runID, e)
		row := pollStartRow{Agent: e.name, ChildRunID: runID, State: state}
		if e.index >= 0 {
			idx := e.index
			row.Index = &idx
		}
		rows = append(rows, row)
	}
	var body []byte
	var err error
	if batchID == "" {
		r := rows[0]
		body, err = json.Marshal(struct {
			ChildRunID string `json:"child_run_id"`
			Agent      string `json:"agent"`
			State      string `json:"state"`
		}{r.ChildRunID, r.Agent, r.State})
	} else {
		body, err = json.Marshal(struct {
			BatchID  string         `json:"batch_id"`
			Children []pollStartRow `json:"children"`
		}{batchID, rows})
	}
	if err != nil {
		return errFrom(fmt.Sprintf("internal: marshal poll-mode answer: %s", err), err), nil
	}
	return tools.Result{Text: string(body)}, nil
}

// runBackgroundChild runs one background child to its end and files the
// result. cctx is the child's own: the run's values, cancelled with the run
// or by the parent's cancel.
func (a *AgentTool) runBackgroundChild(cctx context.Context, bg *tools.Background, sem chan struct{}, hasSlot bool, release func(), runID string, e bgEntry) {
	defer release() // alive until it ends, queued time included
	defer func() {
		if r := recover(); r != nil {
			log.Printf("background child %s panicked: %v", runID, r)
			bg.Finish(runID, tools.ChildFailed, tools.ChildResult{Error: fmt.Sprintf("sub-agent %q failed: internal error", e.name)})
		}
	}()
	if !hasSlot {
		select {
		case sem <- struct{}{}:
			bg.SetState(runID, tools.ChildRunning)
		case <-cctx.Done():
			bg.Finish(runID, tools.ChildCancelled, tools.ChildResult{
				Error: fmt.Sprintf("sub-agent %q was cancelled before it started: %v", e.name, context.Cause(cctx))})
			return
		}
	}
	defer func() { <-sem }()

	childCtx := tools.WithChildRunID(cctx, runID)
	if !e.compaction.IsZero() {
		childCtx = tools.WithCompactionOverride(childCtx, e.compaction)
	}
	// The child's review holds reach the parent's stream as they do in wait
	// mode; here they also move its state to held and back.
	parentEmit := tools.EventEmitter(cctx)
	childCtx = tools.WithEventEmitter(childCtx, func(ev providers.Event) {
		if h := ev.SubagentHold; ev.Type == providers.EventSubagentHold && h != nil && h.SubagentRunID == runID {
			if h.State == providers.SubagentHoldHeld {
				bg.SetState(runID, tools.ChildHeld)
			} else {
				bg.SetState(runID, tools.ChildRunning)
			}
		}
		parentEmit(ev)
	})
	out, state, _, timedOut, err := a.runChildBounded(childCtx, e.timeoutMs, e.name, e.prompt, e.defID)
	switch {
	case timedOut:
		bg.Finish(runID, tools.ChildTimeout, tools.ChildResult{Error: childTimedOutMessage(e.name, e.timeoutMs, runID), Status: "timeout"})
	case err != nil && cctx.Err() != nil:
		bg.Finish(runID, tools.ChildCancelled, tools.ChildResult{Error: err.Error()})
	case err != nil:
		bg.Finish(runID, tools.ChildFailed, tools.ChildResult{Error: err.Error()})
	default:
		if out == "" && len(state) == 0 {
			out = fmt.Sprintf("(sub-agent %q completed with no final text)", e.name)
		}
		bg.Finish(runID, tools.ChildCompleted, tools.ChildResult{Output: out, Structured: state})
	}
}

// pollRow is one child in a poll or cancel answer.
type pollRow struct {
	ChildRunID string `json:"child_run_id"`
	Agent      string `json:"agent,omitempty"`
	// Kind is "team" for a team walk run in poll mode: Agent is then its run's
	// label (team:<name>) and Output its final output. TeamDef poll reads the
	// walk's whole answer.
	Kind       string         `json:"kind,omitempty"`
	Index      *int           `json:"index,omitempty"`
	State      string         `json:"state"`
	Output     string         `json:"output,omitempty"`
	Error      string         `json:"error,omitempty"`
	Structured map[string]any `json:"structured,omitempty"`
	Status     string         `json:"status,omitempty"`
	Truncated  bool           `json:"truncated,omitempty"`
	// StructuredOmitted is set, with StructuredBytes its size as JSON, when
	// Structured did not fit the row's share and was left out (capPollRows).
	StructuredOmitted bool `json:"structured_omitted,omitempty"`
	StructuredBytes   int  `json:"structured_bytes,omitempty"`
}

func rowOf(v tools.ChildView) pollRow {
	r := pollRow{ChildRunID: v.RunID, Agent: v.Agent, Kind: v.Kind, State: v.State}
	if v.Index >= 0 {
		idx := v.Index
		r.Index = &idx
	}
	if v.Ended() {
		r.Output, r.Error, r.Structured, r.Status = v.Result.Output, v.Result.Error, v.Result.Structured, v.Result.Status
	}
	return r
}

// unknownChildren is the answer for ids or a batch this run did not start —
// the same whether they do not exist or belong to another run.
func unknownChildren(ids []string) tools.Result {
	return errNotFound(fmt.Sprintf("not a child of this run: %s", strings.Join(ids, ", ")),
		"Use the child_run_id or batch_id that spawn, parallel_spawn or open returned to you, or the run_id of a team you ran in poll mode.")
}

func (a *AgentTool) pollWaitCap() time.Duration { return pollWaitCap(a.PollWaitCapMs) }

// pollWaitCap is the operator's cap on one poll's wait, in ms (0 = the
// default).
func pollWaitCap(ms int) time.Duration {
	if ms <= 0 {
		ms = DefaultPollWaitCapMs
	}
	return time.Duration(ms) * time.Millisecond
}

// pollChildren is the generalised poll: the states and results of children
// named by id or batch, or of every background child whose result the parent
// has not read, optionally waiting for one or all of them to end.
func (a *AgentTool) pollChildren(ctx context.Context, in agentInput) (tools.Result, error) {
	bg := tools.BackgroundOf(ctx)
	if bg == nil {
		return errBusiness("this run has no background children to poll", "Poll a resident child with child_run_id."), nil
	}
	if len(in.ChildRunIDs) > 0 && in.BatchID != "" {
		return errValidation("pass child_run_ids or batch_id, not both", ""), nil
	}
	if in.TimeoutMs != 0 {
		return errValidation("timeout_ms bounds a resident child's poll (child_run_id); to wait here use wait and wait_ms", ""), nil
	}
	if in.WaitMs < 0 {
		return errValidation("wait_ms must be >= 0", ""), nil
	}
	switch in.Wait {
	case "", "none", "any", "all":
	default:
		return errValidation(fmt.Sprintf("unknown wait %q (expected \"none\", \"any\" or \"all\")", in.Wait), ""), nil
	}
	views, unknown := bg.Select(in.ChildRunIDs, strings.TrimSpace(in.BatchID))
	if len(unknown) > 0 {
		return unknownChildren(unknown), nil
	}
	ids := make([]string, len(views))
	for i, v := range views {
		ids[i] = v.RunID
	}
	if in.Wait == "any" || in.Wait == "all" {
		a.awaitChildren(ctx, bg, views, in.Wait == "all", in.WaitMs)
		views, _ = bg.Select(ids, "")
	}

	rows := make([]pollRow, len(views))
	var read []string
	pending := 0
	for i, v := range views {
		if v.Resident {
			rows[i] = a.residentRow(ctx, v)
		} else {
			rows[i] = rowOf(v)
			if v.Ended() {
				read = append(read, v.RunID)
			}
		}
		switch rows[i].State {
		case tools.ChildQueued, tools.ChildRunning, tools.ChildHeld:
			pending++
		}
	}
	capPollRows(rows, quarterWindowChars(ctx))
	body, err := json.Marshal(struct {
		Children []pollRow `json:"children"`
		Pending  int       `json:"pending"`
	}{rows, pending})
	if err != nil {
		return errFrom(fmt.Sprintf("internal: marshal poll answer: %s", err), err), nil
	}
	bg.MarkRead(read)
	return tools.Result{Text: string(body)}, nil
}

// awaitChildren blocks until one (any) or all of the background children in
// views that are still running have ended, the wait bound runs out, or ctx
// ends. It is a wait: a code agent's budget does not run while it blocks.
func (a *AgentTool) awaitChildren(ctx context.Context, bg *tools.Background, views []tools.ChildView, all bool, waitMs int) {
	awaitBackground(ctx, bg, views, all, waitMs, a.pollWaitCap())
}

// awaitBackground is awaitChildren with the cap given: TeamDef poll waits for
// its walks the same way.
func awaitBackground(ctx context.Context, bg *tools.Background, views []tools.ChildView, all bool, waitMs int, bound time.Duration) {
	var waiting []string
	for _, v := range views {
		if !v.Resident && !v.Ended() {
			waiting = append(waiting, v.RunID)
		}
	}
	if len(waiting) == 0 {
		return
	}
	if waitMs > 0 && time.Duration(waitMs)*time.Millisecond < bound {
		bound = time.Duration(waitMs) * time.Millisecond
	}
	defer providers.BeginWait(ctx)()
	timer := time.NewTimer(bound)
	defer timer.Stop()
	for {
		changed := bg.Changed()
		ended := 0
		for _, id := range waiting {
			if v, _ := bg.Lookup(id); v.Ended() {
				ended++
			}
		}
		if ended == len(waiting) || (!all && ended > 0) {
			return
		}
		select {
		case <-changed:
		case <-timer.C:
			return
		case <-ctx.Done():
			return
		}
	}
}

// residentRow reports a resident child of this run as it is now: idle when
// it is parked between turns. A resident child is never waited for here —
// poll it with child_run_id and timeout_ms to wait on its turn.
func (a *AgentTool) residentRow(ctx context.Context, v tools.ChildView) pollRow {
	r := pollRow{ChildRunID: v.RunID, Agent: v.Agent}
	if a.PollChild == nil {
		r.State, r.Error = tools.ChildFailed, "resident sub-agents are not available on this runtime"
		return r
	}
	out, state, err := a.PollChild(ctx, v.RunID, 0)
	switch {
	case err != nil:
		// Closed or reaped: its run was ended for it.
		r.State, r.Error = tools.ChildCancelled, err.Error()
	case state == "awaiting_input":
		r.State, r.Output = tools.ChildIdle, out
	default:
		r.State, r.Output = state, out
	}
	return r
}

// capPollRows cuts each row that carries an answer to an equal share of
// quarter — the bound a parallel_spawn envelope's rows share, cut the same way
// (capRowOutputs): the error first, the structured state kept whole or left
// out, the output last.
func capPollRows(rows []pollRow, quarter int) {
	n := 0
	for _, r := range rows {
		if r.Output != "" || r.Error != "" || len(r.Structured) > 0 {
			n++
		}
	}
	if quarter <= 0 || n == 0 {
		return
	}
	share := quarter / n
	for i := range rows {
		r := &rows[i]
		left := share
		if e, cut := cutOnRune(r.Error, left); cut {
			r.Error, r.Truncated = e, true
		}
		left -= len(r.Error)
		if sz := jsonSize(r.Structured); sz > left {
			r.Structured, r.StructuredOmitted, r.StructuredBytes, r.Truncated = nil, true, sz, true
		} else {
			left -= sz
		}
		if out, cut := cutOnRune(r.Output, left); cut {
			r.Output, r.Truncated = out, true
		}
	}
}

// cancelChildren is the generalised cancel: background children named by id
// are cancelled (and reported once they have ended, bounded by cancelSettle);
// a resident child has its current turn stopped, as cancel with child_run_id
// does. Every id must be this run's, or nothing is cancelled.
func (a *AgentTool) cancelChildren(ctx context.Context, in agentInput) (tools.Result, error) {
	bg := tools.BackgroundOf(ctx)
	if bg == nil {
		return unknownChildren(in.ChildRunIDs), nil
	}
	if in.BatchID != "" || in.Wait != "" || in.WaitMs != 0 {
		return errValidation("cancel takes child_run_ids only", ""), nil
	}
	views, unknown := bg.Select(in.ChildRunIDs, "")
	if len(unknown) > 0 {
		return unknownChildren(unknown), nil
	}
	cause := runcancel.CauseWithReason("cancelled by its parent")
	var cancelled []tools.ChildView
	rows := make([]pollRow, len(views))
	for i, v := range views {
		if v.Resident {
			rows[i] = pollRow{ChildRunID: v.RunID, Agent: v.Agent}
			if a.CancelChild == nil {
				rows[i].State, rows[i].Error = tools.ChildFailed, "resident sub-agents are not available on this runtime"
				continue
			}
			out, state, err := a.CancelChild(ctx, v.RunID)
			switch {
			case err != nil:
				rows[i].State, rows[i].Error = tools.ChildCancelled, err.Error()
			case state == "awaiting_input":
				rows[i].State, rows[i].Output = tools.ChildIdle, out
			default:
				rows[i].State, rows[i].Output = state, out
			}
			continue
		}
		if !v.Ended() {
			bg.Cancel(v.RunID, cause)
			cancelled = append(cancelled, v)
		}
	}
	if len(cancelled) > 0 {
		a.awaitChildren(ctx, bg, cancelled, true, int(cancelSettle/time.Millisecond))
	}
	for i, v := range views {
		if v.Resident {
			continue
		}
		now, _ := bg.Lookup(v.RunID)
		rows[i] = pollRow{ChildRunID: now.RunID, Agent: now.Agent, Kind: now.Kind, State: now.State}
		if now.Index >= 0 {
			idx := now.Index
			rows[i].Index = &idx
		}
	}
	body, err := json.Marshal(struct {
		Children []pollRow `json:"children"`
	}{rows})
	if err != nil {
		return errFrom(fmt.Sprintf("internal: marshal cancel answer: %s", err), err), nil
	}
	return tools.Result{Text: string(body)}, nil
}
