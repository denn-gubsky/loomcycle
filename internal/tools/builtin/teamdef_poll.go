package builtin

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"

	"github.com/denn-gubsky/loomcycle/internal/teamrun"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// Poll mode for team walks: op=run with mode "poll" starts the walk as a
// background child of the calling run — in the run's background table beside
// its poll-mode sub-agents — and returns its run id at once. The run is told
// when the walk ends, does not end while it runs, and reads its answer with
// op=poll (or Agent poll, which reads every background child). Unlike mode
// "detach", the walk lives and dies with the calling run.

// The fixes TeamDef's poll-mode refusals offer.
const (
	teamPollNoTable       = "Omit mode to wait for the walk, or use mode \"detach\" for a walk that runs on outside your run."
	teamPollLastIteration = "Omit mode so the call returns the walk's answer."
)

// walkEnding is how a poll-mode walk ended, as its row in the caller's table
// records it.
type walkEnding struct {
	state string
	res   tools.ChildResult
}

// finishPollWalk runs a poll-mode walk to its end and files the ending in the
// caller's table. The walk's live-children slot is freed first, so a parent
// woken by the end finds the slot free. A panic is filed as a failure: the
// parent must not wait for a walk that will never end.
func finishPollWalk(bg *tools.Background, runID string, releaseLive func(), run func() walkEnding) {
	defer releaseLive()
	defer func() {
		if r := recover(); r != nil {
			log.Printf("teamdef: poll-mode walk %s panicked: %v", runID, r)
			releaseLive()
			bg.Finish(runID, tools.ChildFailed, tools.ChildResult{Error: "run: internal error"})
		}
	}()
	end := run()
	releaseLive()
	bg.Finish(runID, end.state, end.res)
}

// pollWalkResult is a poll-mode walk's ending, from what a waited-for run would
// have answered. A walk whose ctx was cancelled — by its caller's run ending,
// or a cancel naming it — ended cancelled, as its run records it. Otherwise
// the answer is kept whole for TeamDef poll: a completed walk's final output
// is also its output for Agent poll; a walk stopped at an iteration cap is
// answered as a waited-for one is, and failed, as its run records it.
func pollWalkResult(answer func([]teamrun.StepRecord, error) (map[string]any, error),
	trace []teamrun.StepRecord, werr error, cancelled bool, cause error) walkEnding {
	if cancelled {
		why := werr
		if cause != nil {
			why = cause
		}
		return walkEnding{state: tools.ChildCancelled, res: tools.ChildResult{Error: fmt.Sprintf("run: the walk was cancelled: %v", why)}}
	}
	out, failed := answer(trace, werr)
	switch {
	case failed != nil:
		return walkEnding{state: tools.ChildFailed, res: tools.ChildResult{Error: fmt.Sprintf("run: %s", failed)}}
	case werr != nil:
		return walkEnding{state: tools.ChildFailed, res: tools.ChildResult{Error: fmt.Sprintf("run: %s", werr), Detail: out}}
	}
	final, _ := out["final_output"].(string)
	return walkEnding{state: tools.ChildCompleted, res: tools.ChildResult{Output: final, Detail: out}}
}

// memberHolds wraps the hold observer a walk's member reports its review holds
// to, so each hold also reaches report. The returned end closes a hold still
// open when the member returns.
func memberHolds(ctx context.Context, report func(bool)) (context.Context, func()) {
	inner := teamrun.HoldObserver(ctx)
	var mu sync.Mutex
	on := false
	set := func(h bool) {
		if inner != nil {
			inner(h)
		}
		mu.Lock()
		defer mu.Unlock()
		if on != h {
			on = h
			report(h)
		}
	}
	end := func() {
		mu.Lock()
		defer mu.Unlock()
		if on {
			on = false
			report(false)
		}
	}
	return teamrun.WithHoldObserver(ctx, set), end
}

// execPoll reads the walks this run started in poll mode: those named by run
// id, or — none named — every one whose answer the run has not read yet, each
// handed over once. wait "any" / "all" blocks, bounded by wait_ms and the
// runtime's cap, until one or all of them have ended; the wait is a wait, so a
// code agent's budget does not run while it blocks.
//
// An ended walk's row carries the fields a waited-for op=run returns. A
// sub-agent of this run is not a walk, and is answered exactly as an id this
// run never started.
func (t *TeamDef) execPoll(ctx context.Context, in teamDefInput) (tools.Result, error) {
	bg := tools.BackgroundOf(ctx)
	if bg == nil {
		return errBusiness("poll: this run has no team walks to poll",
			"Run a team with mode \"poll\" from inside an agent's run, then poll the run_id it returns."), nil
	}
	if in.WaitMs < 0 {
		return errValidation("poll: wait_ms must be >= 0", ""), nil
	}
	switch in.Wait {
	case "", "none", "any", "all":
	default:
		return errValidation(fmt.Sprintf("poll: unknown wait %q (expected \"none\", \"any\" or \"all\")", in.Wait), ""), nil
	}
	views, unknown := bg.Select(in.RunIDs, "")
	walks := make([]tools.ChildView, 0, len(views))
	for _, v := range views {
		switch {
		case v.Kind == tools.ChildKindTeam:
			walks = append(walks, v)
		case len(in.RunIDs) > 0:
			unknown = append(unknown, v.RunID)
		}
	}
	if len(unknown) > 0 {
		return errNotFound(fmt.Sprintf("poll: not a team walk of this run: %s", strings.Join(unknown, ", ")),
			"Use the run_id a run with mode \"poll\" returned to you."), nil
	}
	ids := make([]string, len(walks))
	for i, v := range walks {
		ids[i] = v.RunID
	}
	if in.Wait == "any" || in.Wait == "all" {
		awaitBackground(ctx, bg, walks, in.Wait == "all", in.WaitMs, pollWaitCap(t.PollWaitCapMs))
		walks, _ = bg.Select(ids, "")
	}

	rows := make([]map[string]any, len(walks))
	var read []string
	pending := 0
	for i, v := range walks {
		row := map[string]any{}
		if v.Ended() {
			for k, val := range v.Result.Detail {
				row[k] = val
			}
			if v.Result.Error != "" {
				row["error"] = v.Result.Error
			}
			read = append(read, v.RunID)
		} else {
			pending++
		}
		row["run_id"] = v.RunID
		row["name"] = strings.TrimPrefix(v.Agent, teamWalkLabel)
		row["state"] = v.State
		rows[i] = row
	}
	res, err := okJSON(map[string]any{"walks": rows, "pending": pending})
	if !res.IsError {
		bg.MarkRead(read)
	}
	return res, err
}
