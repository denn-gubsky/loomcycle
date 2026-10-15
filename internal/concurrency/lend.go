package concurrency

import (
	"context"
	"sync"
)

// A run whose model call waits for a provider's lane is not computing, yet it
// used to keep its global run slot for as long as it waited: a saturated local
// provider then held every slot with waiters, and a run bound for an idle
// provider could not start. So the run LENDS its slot while that call waits and
// takes one back (RECLAIMS) before the call computes. The global cap then
// bounds the runs computing or working, not every run alive.
//
// Two rules keep that safe:
//
//   - A lent run counts against maxQueue with the queued ones (Semaphore.lent).
//     Lending is refused while queued + lent runs are at maxQueue, and so is a
//     new run: runs alive stay within maxConcurrent + maxQueue, as without
//     lending, whatever the per-user cap.
//   - A run never waits for a global slot while it holds a lane slot. Work that
//     does not lend (a tool's model call, a summary, a sub-agent's call) waits
//     for its lane HOLDING a global slot; a run that held the lane slot and
//     waited for that global slot would deadlock with it. So a granted call
//     takes its slot back only if one is free (TryReclaim); otherwise it gives
//     the lane slot back, waits for a global slot holding nothing (Reclaim),
//     and queues for the lane again holding it — the order everything else
//     uses (lanegate.Provider.Call).
//
// A reclaiming run goes ahead of every new run, with the already-admitted work
// Wait serves.

// RunSlot is an admitted run's hold on its global slot. Bind one with
// AdmitRequest.Slot; the release Admit returns then ends it. A nil *RunSlot
// lends nothing. Its methods are for the run's own goroutine: end waits for a
// Reclaim in progress.
type RunSlot struct {
	mu    sync.Mutex // serializes Lend, Reclaim and the run's end
	s     *Semaphore
	key   userKey
	qs    userQuotaGate
	lent  bool
	ended bool
}

// runRelease is the release of an admitted run: plain releaseFn, or one bound
// to rs so the run can lend its global slot.
func (s *Semaphore) runRelease(key userKey, qs userQuotaGate, rs *RunSlot) func() {
	if rs == nil {
		return s.releaseFn(key, qs)
	}
	rs.mu.Lock()
	rs.s, rs.key, rs.qs = s, key, qs
	rs.mu.Unlock()
	return rs.end
}

// end gives back what the run holds when it ends: its per-user count, and its
// global slot unless it is lent. Idempotent.
func (rs *RunSlot) end() {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if rs.ended || rs.s == nil {
		return
	}
	rs.ended = true
	if rs.lent {
		rs.s.mu.Lock()
		rs.s.lent--
		rs.s.mu.Unlock()
	}
	rs.s.endRun(rs.key, rs.qs, !rs.lent)
}

// Lend gives the run's global slot back while its call waits for a lane: the
// next waiting run, or work Wait serves, takes it. The run keeps its per-user
// count. It reports whether the slot is lent now: lending is refused while
// queued and lent runs are at the queue bound, and the run then waits holding
// its slot, as without lending.
func (rs *RunSlot) Lend() bool {
	if rs == nil {
		return false
	}
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if rs.s == nil || rs.ended {
		return false
	}
	if rs.lent {
		return true
	}
	s := rs.s
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.queued+s.lent >= s.maxQueue {
		return false
	}
	rs.lent = true
	s.lent++
	s.active--
	s.wakeNextLocked()
	return true
}

// Lent reports whether the run's slot is lent now.
func (rs *RunSlot) Lent() bool {
	if rs == nil {
		return false
	}
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return rs.lent
}

// TryReclaim takes a global slot back only if one is free, and reports whether
// the run holds one now. For a call that holds a lane slot: it must not wait.
func (rs *RunSlot) TryReclaim() bool {
	if rs == nil {
		return true
	}
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if rs.s == nil || !rs.lent {
		return true
	}
	if rs.ended {
		return false
	}
	s := rs.s
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active >= s.maxConcurrent {
		return false
	}
	s.active++
	s.lent--
	rs.lent = false
	return true
}

// Reclaim takes a global slot back for a run that lent its own: at once when
// one is free, else ahead of every new run, in FIFO order with the work Wait
// serves. It never refuses — the run was admitted. It returns ctx.Err() when ctx
// ends first; the slot then stays lent, and the run's end gives back no global
// slot. Call it holding no lane slot (see the package comment). A no-op unless
// the slot is lent.
func (rs *RunSlot) Reclaim(ctx context.Context) error {
	if rs == nil {
		return nil
	}
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if rs.s == nil || !rs.lent || rs.ended {
		return nil
	}
	s := rs.s
	s.mu.Lock()
	if s.active < s.maxConcurrent {
		s.active++
		s.lent--
		s.mu.Unlock()
		rs.lent = false
		return nil
	}
	// Waiting now: counted in queued, not in lent.
	w := &semWaiter{ch: make(chan struct{}, 1)}
	s.admitted = append(s.admitted, w)
	s.queued++
	s.lent--
	s.mu.Unlock()

	select {
	case <-w.ch:
		rs.lent = false
		return nil
	case <-ctx.Done():
		s.mu.Lock()
		if s.removeWaiter(w) {
			s.queued--
			s.lent++
			s.mu.Unlock()
			return ctx.Err()
		}
		// The slot was handed over as ctx ended: it is the run's now.
		s.mu.Unlock()
		<-w.ch
		rs.lent = false
		return nil
	}
}

type runSlotKey struct{}

// WithRunSlot puts the run's slot on the ctx its loop makes its own model calls
// under, so the lane those calls wait in can lend it. nil clears it: a
// sub-agent's calls run under its parent's ctx, and the parent's slot is not
// the child's to lend.
func WithRunSlot(ctx context.Context, rs *RunSlot) context.Context {
	return context.WithValue(ctx, runSlotKey{}, rs)
}

// RunSlotFrom returns the slot WithRunSlot put on ctx, nil for none.
func RunSlotFrom(ctx context.Context) *RunSlot {
	rs, _ := ctx.Value(runSlotKey{}).(*RunSlot)
	return rs
}
