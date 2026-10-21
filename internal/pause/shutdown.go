package pause

import (
	"sort"
	"time"
)

// ShutdownResult is what a shutdown pause did with the runs in flight here.
type ShutdownResult struct {
	// ParkedRuns is the runs parked at the end: their rows are durably
	// pause_state='paused', and the next boot resumes them.
	ParkedRuns int
	// InterruptedRuns is the runs not parked at the interrupt point: their
	// tool calls were stopped, then and for the rest of this process.
	InterruptedRuns []string
	// UnparkedRuns is the runs still not parked when the bound ran out — the
	// ones waiting on a model call, mostly. They are not resumed; the stale-run
	// sweeper ends them as it ended every run in flight before.
	UnparkedRuns []string
	// UnpausableRuns is the no-trace runs in flight: nothing stored to resume
	// from, so a pause leaves them alone.
	UnpausableRuns int
	Duration       time.Duration
}

// shutdownPollInterval is how often the shutdown barrier re-reads the runs.
const shutdownPollInterval = 20 * time.Millisecond

// PauseForShutdown pauses this replica's runs before the process exits, within
// timeout (capped at MaxPauseTimeout; 0 or less does nothing), so a restart
// resumes them instead of the stale-run sweeper failing them.
//
// It is the operator's pause with three differences:
//
//   - It is LOCAL. Nothing is written to the cluster's runtime-state row and
//     nothing is published to the other replicas, which keep serving; and the
//     process that boots next starts running, not paused. Once begun it is
//     never lifted: a cluster or operator resume is refused.
//   - It INTERRUPTS. A run parks only at an iteration boundary, which a run
//     inside a long tool call never reaches. At three quarters of the bound,
//     every run not yet parked has its tool calls interrupted (ToolInterrupt):
//     a Bash command's process group is killed, its call fails, and the run
//     parks at the next boundary with that failure in its transcript, where
//     the resumed run reads it. A parked run is left alone — a fan-out parent
//     parked mid-spawn keeps its children.
//   - It never fails: it returns what it found when the bound runs out.
//
// A run waiting on its model call when the bound runs out is not interrupted:
// a partial answer would already be in its transcript, which a resume cannot
// continue from. It stays as before, for the stale-run sweeper.
func (m *Manager) PauseForShutdown(timeout time.Duration) ShutdownResult {
	if m == nil || timeout <= 0 {
		return ShutdownResult{}
	}
	if timeout > MaxPauseTimeout {
		timeout = MaxPauseTimeout
	}
	start := time.Now()
	deadline := start.Add(timeout)
	interruptAt := start.Add(timeout * 3 / 4)

	m.mu.Lock()
	m.shutdown.Store(true)
	recorded := closedSignal()
	// Already pausing or paused (an operator's pause): its runs are parked or
	// parking, and nothing more is declared.
	if loadState(&m.state) == StateRunning {
		m.state.Store(int32(StatePausing))
		m.pausedAt = start
		close(m.pauseCh)
		m.recorded = m.liveRunIDs()
		recorded = recordRunPauses(m.recorder, m.recorded, start, time.Time{})
	}
	m.mu.Unlock()
	m.invalidateStateCache()

	var res ShutdownResult
	if !m.waitParked(interruptAt) {
		res.InterruptedRuns = m.interruptUnparkedTools()
		m.waitParked(deadline)
	}
	// The pause records each run's timeout_ms clock re-arms from; the process
	// is going, so they get what is left of the bound and no more.
	select {
	case <-recorded:
	case <-time.After(time.Until(deadline)):
	}
	m.state.Store(int32(StatePaused))
	m.invalidateStateCache()

	_, total, parked := m.runsQuiesced()
	res.ParkedRuns = parked
	if parked < total {
		res.UnparkedRuns = m.unparkedRunIDs()
		sort.Strings(res.UnparkedRuns)
	}
	res.UnpausableRuns = m.UnpausableRuns()
	res.Duration = time.Since(start)
	return res
}

// waitParked waits until every registered run is parked or until passes, and
// reports whether they all parked.
func (m *Manager) waitParked(until time.Time) bool {
	for {
		if all, _, _ := m.runsQuiesced(); all {
			return true
		}
		left := time.Until(until)
		if left <= 0 {
			return false
		}
		time.Sleep(min(left, shutdownPollInterval))
	}
}

// interruptUnparkedTools stops the tool calls of every registered run not
// parked now, and of every run that registers from now on, stops the current
// turn of each of those runs that has one to stop (SetShutdownTurnCanceller),
// and names the runs it stopped.
func (m *Manager) interruptUnparkedTools() []string {
	m.toolsInterrupted.Store(true)
	m.mu.Lock()
	cancelTurn := m.turnCanceller
	m.mu.Unlock()
	var ids []string
	m.activeRuns.Range(func(k, v any) bool {
		e := v.(*runEntry)
		if !e.parked.Load() {
			e.stop()
			ids = append(ids, k.(string))
		}
		return true
	})
	sort.Strings(ids)
	if cancelTurn != nil {
		for _, id := range ids {
			cancelTurn(id)
		}
	}
	return ids
}

func closedSignal() <-chan struct{} {
	c := make(chan struct{})
	close(c)
	return c
}
