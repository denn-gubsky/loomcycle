package http

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/steer"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// A resident child lives in the registry of the replica that opened it (or
// resumed it), but a call addressing it may run on any replica. One that finds
// neither the live child nor its kept ending here reads the child from the
// store, which every replica shares: its run row says whether it is still
// running and how it ended, and its transcript holds its turns — each begins
// with a user_input event and, parked, ends with awaiting_input. Instructions
// and cancels go to its replica through the cluster routes that already
// carry a run's steering and cancels (the steer, turn-cancel and cancel
// coordinators); without them they say the child cannot be reached from here.

// residentStorePollInterval is how often a wait on a child held elsewhere
// re-reads the store: its replica does not tell this one when a turn ends.
const residentStorePollInterval = 250 * time.Millisecond

// residentOwnerCheckEvery is how many re-reads pass between checks that a
// live loop still holds the child (about two seconds): a replica that died
// will never end the turn being waited for.
const residentOwnerCheckEvery = 8

// residentTextPage bounds one read of a turn's events while its answer is
// collected.
const residentTextPage = 500

// storedResident is a resident child as the store has it.
type storedResident struct {
	run    store.Run
	state  string // "running" | "awaiting_input", or the run's terminal status
	output string // the text of its latest turn, so far
	// parkSeq is the seq of the awaiting_input its latest turn ended with;
	// 0 while a turn runs.
	parkSeq int64
}

func (v storedResident) ended() bool { return store.IsTerminalRunStatus(v.run.Status) }

// ending is an ended child's kept-ending shape, read from its row: how it was
// ended for it from the reason its run was cancelled with, and an iteration
// limit from its stop reason, with the number its record keeps.
func (v storedResident) ending() residentTombstone {
	t := residentTombstone{tenantID: v.run.TenantID, userID: v.run.UserID, agentName: v.run.Agent, state: string(v.run.Status), output: v.output}
	switch {
	case v.run.Status == store.RunCancelled:
		t.reason, t.closedBy = residentEndedFor(v.run.StopReason)
		if t.reason == "" && t.closedBy == "" {
			t.closedBy = "cancelled"
		}
	case v.run.Status == store.RunCompleted && v.run.StopReason == loop.StopReasonMaxIterations:
		t.capped = &builtin.ChildCappedError{Name: v.run.Agent, Limit: recordedIterationLimit(v.run), RunID: v.run.ID}
	}
	return t
}

// storedOwnedResident reads childRunID's run for a caller that may address it:
// a resident child's run (Agent op=open), under the rule a live child is
// addressed by (residentOwnedBy). Any other run, an unknown id and a store
// fault all read as not held.
func (s *Server) storedOwnedResident(ctx context.Context, childRunID string) (store.Run, bool) {
	if s.store == nil {
		return store.Run{}, false
	}
	run, err := s.store.GetRun(ctx, childRunID)
	if err != nil {
		return store.Run{}, false
	}
	rec, ok := decodeRunConfig(run.RunConfig)
	if !ok || rec.Spawn == nil || !rec.Spawn.Resident {
		return store.Run{}, false
	}
	if !residentOwnedBy(run.TenantID, run.UserID, tools.RunIdentity(ctx)) {
		return store.Run{}, false
	}
	return run, true
}

// readStoredResident reads the child's state and its latest turn's answer.
func (s *Server) readStoredResident(ctx context.Context, runID string) (storedResident, error) {
	run, err := s.store.GetRun(ctx, runID)
	if err != nil {
		return storedResident{}, err
	}
	v := storedResident{run: run, state: string(run.Status)}
	types := []string{"user_input", string(providers.EventAwaitingInput)}
	last, err := s.store.GetLastEventOfTypes(ctx, runID, types)
	var notFound *store.ErrNotFound
	switch {
	case errors.As(err, &notFound):
		last = store.Event{}
	case err != nil:
		return v, err
	}
	turnStart := last.Seq
	if last.Type == string(providers.EventAwaitingInput) {
		v.parkSeq = last.Seq
		prev, err := s.store.GetLastEventOfTypesBefore(ctx, runID, types[:1], last.Seq)
		switch {
		case errors.As(err, &notFound):
			turnStart = 0
		case err != nil:
			return v, err
		default:
			turnStart = prev.Seq
		}
	}
	if !v.ended() {
		v.state = "running"
		if v.parkSeq > 0 {
			v.state = "awaiting_input"
		}
	}
	var out strings.Builder
	for after := turnStart; ; {
		evs, err := s.store.GetRunEventsSince(ctx, runID, after, residentTextPage)
		if err != nil {
			return v, err
		}
		for _, e := range evs {
			after = e.Seq
			if e.Type != string(providers.EventText) {
				continue
			}
			var p struct {
				Text string `json:"text"`
			}
			if json.Unmarshal(e.Payload, &p) == nil {
				out.WriteString(p.Text)
			}
		}
		if len(evs) < residentTextPage {
			break
		}
	}
	v.output = out.String()
	return v, nil
}

// awaitStoredResident reads the child until its turn has ended — it parked
// after sinceSeq, or its run ended — or the wait is over, as awaitTurn waits
// on a live child. A turn still running when the wait is over reads as
// "running" with its answer so far. A child no live loop holds any more is
// ended at once and read as that (endIfOwnerGone): checked on the first read
// and every residentOwnerCheckEvery after, so a wait does not outlast the
// replica it waits on.
func (s *Server) awaitStoredResident(ctx context.Context, runID string, sinceSeq int64, timeout time.Duration, blockWhenZero bool) (storedResident, error) {
	defer providers.BeginWait(ctx)()
	var deadline <-chan time.Time
	if timeout > 0 {
		t := time.NewTimer(timeout)
		defer t.Stop()
		deadline = t.C
	}
	for i := 0; ; i++ {
		v, err := s.readStoredResident(ctx, runID)
		if err == nil && i%residentOwnerCheckEvery == 0 {
			v, err = s.endIfOwnerGone(ctx, v)
		}
		if err != nil {
			return v, err
		}
		if v.ended() || v.parkSeq > sinceSeq {
			return v, nil
		}
		v.state = "running"
		if v.parkSeq > 0 {
			// The park the send was made on: the child has not taken the
			// instruction yet, and the text read is the previous turn's.
			v.output = ""
		}
		if timeout <= 0 && !blockWhenZero {
			return v, nil
		}
		select {
		case <-ctx.Done():
			v.state = "interrupted"
			return v, ctx.Err()
		case <-deadline:
			return v, nil
		case <-time.After(residentStorePollInterval):
		}
	}
}

// handBackStored hands a child read from the store back through the parent's
// subagent_stop hooks exactly as a read of it on its own replica would: a
// turn as a live child's, an ending as its kept ending.
func (s *Server) handBackStored(ctx context.Context, v storedResident) (string, string, error) {
	if v.ended() {
		return s.handBackEnding(ctx, v.run.ID, v.ending())
	}
	out, err := s.residentHandBack(ctx, v.run.ID, v.run.Agent, residentEnding{}, v.output, v.state, nil)
	return out, v.state, err
}

// residentUnreachableErr is the answer for a child held by a replica this one
// has no route to.
func residentUnreachableErr(childRunID, what string) error {
	return fmt.Errorf("resident sub-agent %q is not held by this replica, and there is no route to the one running it to %s; poll it from here, or open a new one", childRunID, what)
}

// pollUnheldResident is poll for a child this replica does not hold: its kept
// ending, else the child as the store has it.
func (s *Server) pollUnheldResident(ctx context.Context, childRunID string, timeoutMs int) (string, string, error) {
	if t, ok := s.keptEnding(ctx, childRunID); ok {
		return s.handBackEnding(ctx, childRunID, t)
	}
	if _, ok := s.storedOwnedResident(ctx, childRunID); !ok {
		return "", "", residentNotFoundErr(childRunID)
	}
	v, err := s.awaitStoredResident(ctx, childRunID, 0, time.Duration(timeoutMs)*time.Millisecond, false)
	if err != nil {
		return v.output, v.state, err
	}
	return s.handBackStored(ctx, v)
}

// sendUnheldResident is send for a child this replica does not hold. A
// parked child on another replica gets the instruction through the steer
// route, and the turn is awaited in the store.
func (s *Server) sendUnheldResident(ctx context.Context, childRunID, prompt string, timeoutMs int) (string, string, error) {
	if t, ok := s.keptEnding(ctx, childRunID); ok {
		return "", "", residentEndedSendErr(childRunID, t)
	}
	if _, ok := s.storedOwnedResident(ctx, childRunID); !ok {
		return "", "", residentNotFoundErr(childRunID)
	}
	v, err := s.readStoredResident(ctx, childRunID)
	if err != nil {
		return "", "", err
	}
	if v, err = s.endIfOwnerGone(ctx, v); err != nil {
		return "", "", err
	}
	if v.ended() {
		return "", "", residentEndedSendErr(childRunID, v.ending())
	}
	if v.state != "awaiting_input" {
		return "", "", residentStillRunningErr(childRunID)
	}
	if s.steerReg == nil {
		return "", "", residentUnreachableErr(childRunID, "send to it")
	}
	if _, err := s.steerReg.Push(ctx, childRunID, steer.Message{Text: prompt, Source: "agent", EnqueuedAt: time.Now()}); err != nil {
		if errors.Is(err, steer.ErrRunNotFound) {
			return "", "", residentUnreachableErr(childRunID, "send to it")
		}
		return "", "", fmt.Errorf("steer resident sub-agent %q: %w", childRunID, err)
	}
	v, err = s.awaitStoredResident(ctx, childRunID, v.parkSeq, time.Duration(timeoutMs)*time.Millisecond, true)
	if err != nil {
		return v.output, v.state, err
	}
	return s.handBackStored(ctx, v)
}

// cancelUnheldResident is cancel for a child this replica does not hold: its
// turn is stopped through the turn-cancel route, and the re-park awaited in
// the store. A turn no reachable replica had armed is read as it stands.
func (s *Server) cancelUnheldResident(ctx context.Context, childRunID string) (string, string, error) {
	if t, ok := s.keptEnding(ctx, childRunID); ok {
		return s.handBackEnding(ctx, childRunID, t)
	}
	if _, ok := s.storedOwnedResident(ctx, childRunID); !ok {
		return "", "", residentNotFoundErr(childRunID)
	}
	v, err := s.readStoredResident(ctx, childRunID)
	if err != nil {
		return "", "", err
	}
	if v, err = s.endIfOwnerGone(ctx, v); err != nil {
		return "", "", err
	}
	if v.ended() || v.state == "awaiting_input" || s.turnCancelReg == nil {
		return s.handBackStored(ctx, v)
	}
	fired, err := s.turnCancelReg.Cancel(ctx, childRunID, residentReasonTurnCancelledByParent)
	if err != nil {
		return "", "", err
	}
	if fired {
		v, err = s.awaitStoredResident(ctx, childRunID, 0, residentCancelReparkTimeout, false)
		if err != nil {
			return v.output, v.state, err
		}
	}
	return s.handBackStored(ctx, v)
}

// endIfOwnerGone ends a child no live loop holds anywhere — its replica is
// gone, or a crash left its row — and returns it as it then reads: nothing
// would ever take an instruction for it or finish its turn.
func (s *Server) endIfOwnerGone(ctx context.Context, v storedResident) (storedResident, error) {
	if v.ended() || !s.finishUnheldRun(ctx, v.run, residentReasonOwnerGone) {
		return v, nil
	}
	return s.readStoredResident(ctx, v.run.ID)
}

// closeUnheldResident is close for a child this replica does not hold: its
// run is cancelled through the cancel route by its agent id, or, when no
// live loop holds it anywhere, by finishing its row. One already ended, or
// unknown to the caller, is closed already.
func (s *Server) closeUnheldResident(ctx context.Context, childRunID string) error {
	run, ok := s.storedOwnedResident(ctx, childRunID)
	if !ok || store.IsTerminalRunStatus(run.Status) {
		return nil
	}
	if _, found := s.cancelReg.Cancel(run.AgentID, residentReasonClosedByParent); found {
		return nil
	}
	if s.finishUnheldRun(ctx, run, residentReasonClosedByParent) {
		return nil
	}
	return residentUnreachableErr(childRunID, "close it")
}
