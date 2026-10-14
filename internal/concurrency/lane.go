// lane.go — one call lane: the cap on individual model CALLS for one
// (provider, model kind).
//
// A cap that counts runs gives a run a slot for its whole life, so a run that
// is waiting on a tool, a sub-agent or its operator keeps a slot it is not
// using. A Lane counts calls instead, and adds a short LEASE so that
// counting calls does not cost the host its warm prompt cache: after a call the
// slot stays reserved for the run that made it, so that run's next call lands
// on the same slot instead of behind an unrelated one.
//
// The lease is bounded two ways. AffinityHold bounds it in time (a run that
// went off to do something slow loses the slot), QuantumCalls bounds it in
// calls (a run that calls back at once every time still yields while others
// wait). With AffinityHold 0 there is no lease and the lane is a plain fair
// queue.
package concurrency

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// CallInfo identifies one model call to the scheduler.
type CallInfo struct {
	// Owner is the run id, the identity a lease is held under. "" is a
	// one-shot request: it takes a slot for the call and never a reservation.
	Owner string
	// TenantID and UserID are the fairness identity: waiters are served
	// round-robin across (TenantID, UserID). Both empty is one anonymous bucket.
	TenantID, UserID string
	// Background marks maintenance work (consolidation, reindex). It is served
	// after foreground work, with the share LaneConfig.BackgroundShare guarantees.
	Background bool
	// Model is the model identity as the host loads it. "" is not subject to
	// the models-in-flight cap (see ModelGate). A Lane ignores it.
	Model string
}

// LaneConfig is the policy of one lane.
type LaneConfig struct {
	// MaxConcurrent is the number of slots. NewLane and Reconfigure raise a
	// value below 1 to 1: a lane with no slot would park every caller forever,
	// since a wait here has no timeout. "No cap" is expressed by having no lane.
	MaxConcurrent int
	// QuantumCalls is how many consecutive calls a lease holder may make while
	// others wait. 0 = unlimited (the lease is bounded by AffinityHold only).
	QuantumCalls int
	// AffinityHold is how long a slot stays reserved for its owner after a
	// call. 0 = no reservation.
	AffinityHold time.Duration
	// BackgroundShare is the minimum fraction of grants given to background
	// work while BOTH classes wait, in [0,1). 0 = strict foreground priority.
	// Out-of-range values are clamped to [0,1].
	BackgroundShare float64
}

// QueueFullError is why a bounded acquire (AcquireBounded) was refused: the
// queue it would have joined already held Limit waiters. The provider wrapper
// (internal/lanegate) turns it into *ErrProviderConcurrencyExhausted.
type QueueFullError struct {
	Queued, Limit int
}

func (e *QueueFullError) Error() string {
	return fmt.Sprintf("queue full: %d waiting, limit %d", e.Queued, e.Limit)
}

// IsQueueFull reports whether err is a *QueueFullError.
func IsQueueFull(err error) bool {
	var e *QueueFullError
	return errors.As(err, &e)
}

// LaneStats is a point-in-time snapshot of a lane.
type LaneStats struct {
	MaxConcurrent    int
	Active           int    // calls in flight
	Reserved         int    // slots idle but held for their owner
	Queued           int    // every waiter, both classes
	QueuedBackground int    // the background waiters among Queued
	Grants           uint64 // calls admitted since the lane was built
	Preemptions      uint64 // leases ended by the quantum while others waited
}

// lease is an owner's hold on one slot. It is in Lane.leases from the grant
// that created it until it is dropped; busy says whether the owner's call is in
// flight on it (the slot is then counted in Lane.active) or it sits reserved
// (counted in Lane.reserved).
type lease struct {
	owner string
	busy  bool
	used  int // consecutive calls the owner has made on it
	// deadline is when the current reservation runs out. The expiry timer
	// checks it instead of trusting that it fired for THIS reservation: a timer
	// stopped just too late still runs its func once, after the slot was reused.
	deadline time.Time
	timer    *time.Timer
}

// laneWaiter is one queued Acquire.
type laneWaiter struct {
	info CallInfo
	ch   chan struct{} // closed on grant
	// granted and lease are written under Lane.mu by the granter. A cancelled
	// waiter reads granted under the same lock to tell "still queued" from
	// "handed a slot while cancelling", so no token can be stranded in ch.
	granted bool
	lease   *lease
}

// laneBucket is the FIFO of one (tenant, user)'s waiters in one class.
type laneBucket struct {
	key     userKey
	waiters []*laneWaiter
}

// laneQueue is the waiters of one class (foreground or background): FIFO within
// a bucket, round-robin across buckets. ring holds only non-empty buckets, in
// the order they will be served.
type laneQueue struct {
	buckets map[userKey]*laneBucket
	ring    []*laneBucket
	n       int
}

func (q *laneQueue) push(w *laneWaiter) {
	key := userKey{tenant: w.info.TenantID, user: w.info.UserID}
	b := q.buckets[key]
	if b == nil {
		if q.buckets == nil {
			q.buckets = map[userKey]*laneBucket{}
		}
		b = &laneBucket{key: key}
		q.buckets[key] = b
		// A bucket that just became non-empty joins at the back: it waits one
		// full round, the same as a bucket that was just served.
		q.ring = append(q.ring, b)
	}
	b.waiters = append(b.waiters, w)
	q.n++
}

// pop takes the oldest waiter of the bucket whose turn it is and moves that
// bucket to the back. Caller has checked n > 0.
func (q *laneQueue) pop() *laneWaiter {
	b := q.ring[0]
	w := b.waiters[0]
	b.waiters[0] = nil
	b.waiters = b.waiters[1:]
	q.ring = q.ring[1:]
	if len(b.waiters) == 0 {
		delete(q.buckets, b.key)
	} else {
		q.ring = append(q.ring, b)
	}
	q.n--
	return w
}

// remove drops a waiter that is still queued. It keeps the bucket's place in
// the ring: a cancel must not cost the bucket's other waiters their turn.
func (q *laneQueue) remove(w *laneWaiter) {
	key := userKey{tenant: w.info.TenantID, user: w.info.UserID}
	b := q.buckets[key]
	if b == nil {
		return
	}
	for i, x := range b.waiters {
		if x != w {
			continue
		}
		b.waiters = append(b.waiters[:i], b.waiters[i+1:]...)
		q.n--
		if len(b.waiters) == 0 {
			delete(q.buckets, key)
			for j, rb := range q.ring {
				if rb == b {
					q.ring = append(q.ring[:j], q.ring[j+1:]...)
					break
				}
			}
		}
		return
	}
}

// Lane caps the model calls in flight for one (provider, kind) and decides who
// goes next. The zero value is not usable; build one with NewLane.
type Lane struct {
	mu  sync.Mutex
	cfg LaneConfig

	active   int // calls in flight
	reserved int // idle leases; active+reserved is the slots in use
	leases   map[string]*lease

	fg, bg laneQueue
	// ownerQueued counts the queued waiters of each non-empty owner, so a
	// release can tell that its owner has another call waiting (see finish).
	ownerQueued map[string]int
	// bgCredit is the background class's accumulated share of the grants made
	// while both classes waited; a whole credit buys one background grant.
	bgCredit float64

	grants      uint64
	preemptions uint64
}

// NewLane builds a lane.
func NewLane(cfg LaneConfig) *Lane {
	return &Lane{
		cfg:         normalizeLaneConfig(cfg),
		leases:      map[string]*lease{},
		ownerQueued: map[string]int{},
	}
}

func normalizeLaneConfig(cfg LaneConfig) LaneConfig {
	if cfg.MaxConcurrent < 1 {
		cfg.MaxConcurrent = 1
	}
	if cfg.QuantumCalls < 0 {
		cfg.QuantumCalls = 0
	}
	if cfg.AffinityHold < 0 {
		cfg.AffinityHold = 0
	}
	// Written as "not > 0" so a NaN share also lands on strict priority.
	if !(cfg.BackgroundShare > 0) {
		cfg.BackgroundShare = 0
	}
	if cfg.BackgroundShare > 1 {
		cfg.BackgroundShare = 1
	}
	return cfg
}

// Acquire blocks until c may make one call and returns the release for it.
// Call release when the call ends; it is idempotent.
//
// Acquire never times out and the queue has no depth limit: a call belongs to
// a run that was already admitted, and refusing it would fail work that has
// started. Only ctx ends a wait, with ctx.Err().
func (l *Lane) Acquire(ctx context.Context, c CallInfo) (release func(), err error) {
	return l.acquire(ctx, c, -1)
}

// AcquireBounded is Acquire for a request that holds no admission slot (one a
// caller sends directly, outside any run): nothing else bounds how many of them
// can wait, so the lane does. When no slot is free and maxQueued callers
// already wait, it returns *QueueFullError at once instead of queueing.
func (l *Lane) AcquireBounded(ctx context.Context, c CallInfo, maxQueued int) (release func(), err error) {
	if maxQueued < 0 {
		maxQueued = 0
	}
	return l.acquire(ctx, c, maxQueued)
}

// acquire is Acquire with an optional queue bound; maxQueued < 0 is none.
func (l *Lane) acquire(ctx context.Context, c CallInfo, maxQueued int) (release func(), err error) {
	// A caller that is already cancelled must not take a slot, least of all a
	// reservation that would then sit idle for the hold.
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	l.mu.Lock()
	if ls := l.leases[c.Owner]; c.Owner != "" && ls != nil && !ls.busy {
		if q := l.cfg.QuantumCalls; q > 0 && ls.used >= q && l.queued() > 0 {
			// Its turn is over and someone waits: the slot goes to the next
			// waiter and this owner queues behind, below.
			l.dropIdle(ls)
			l.preemptions++
			l.dispatch()
		} else {
			// Back inside its hold: its own slot, ahead of the queue.
			ls.timer.Stop()
			ls.busy = true
			ls.used++
			l.reserved--
			l.active++
			l.grants++
			l.mu.Unlock()
			return l.releaseFn(ls), nil
		}
	}
	// A free slot implies an empty queue (every change that frees a slot or
	// adds one runs dispatch), so taking it here overtakes nobody.
	if l.active+l.reserved < l.cfg.MaxConcurrent {
		ls := l.grant(c)
		l.mu.Unlock()
		return l.releaseFn(ls), nil
	}
	// Checked under the lock that enqueues, so a burst cannot slip past it.
	if maxQueued >= 0 && l.queued() >= maxQueued {
		n := l.queued()
		l.mu.Unlock()
		return nil, &QueueFullError{Queued: n, Limit: maxQueued}
	}
	w := &laneWaiter{info: c, ch: make(chan struct{})}
	if c.Background {
		l.bg.push(w)
	} else {
		l.fg.push(w)
	}
	if c.Owner != "" {
		l.ownerQueued[c.Owner]++
	}
	l.mu.Unlock()

	select {
	case <-w.ch:
		return l.releaseFn(w.lease), nil
	case <-ctx.Done():
		l.mu.Lock()
		if w.granted {
			// The hand-off won the race: this waiter was given a slot as it
			// gave up. Hand the slot straight on; the call never ran, so it
			// neither counts as a grant nor leaves a reservation behind.
			l.grants--
			l.finish(w.lease, true)
		} else {
			l.unqueue(w)
		}
		l.mu.Unlock()
		return nil, ctx.Err()
	}
}

// Done tells the lane that owner's run has ended: its reservation, if it holds
// one, is dropped now and the slot handed on. A call of owner's still in
// flight finishes normally and then leaves no reservation.
func (l *Lane) Done(owner string) {
	if owner == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	ls := l.leases[owner]
	if ls == nil {
		return
	}
	if ls.busy {
		// finish sees the lease is no longer the owner's and frees the slot
		// outright.
		delete(l.leases, owner)
		return
	}
	l.dropIdle(ls)
	l.dispatch()
}

// Reconfigure applies a new config in place (config reload). A larger
// MaxConcurrent admits waiters at once. A smaller one never interrupts a call
// in flight: idle reservations are given up first, then the excess drains as
// calls end. The other fields apply to decisions made from now on; a
// reservation already running keeps the hold it was given, except that
// AffinityHold 0 ends every reservation at once.
func (l *Lane) Reconfigure(cfg LaneConfig) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.cfg = normalizeLaneConfig(cfg)
	for _, ls := range l.leases {
		if l.cfg.AffinityHold > 0 && l.active+l.reserved <= l.cfg.MaxConcurrent {
			break
		}
		if !ls.busy {
			l.dropIdle(ls)
		}
	}
	l.dispatch()
}

// Stats returns a snapshot of the lane.
func (l *Lane) Stats() LaneStats {
	l.mu.Lock()
	defer l.mu.Unlock()
	return LaneStats{
		MaxConcurrent:    l.cfg.MaxConcurrent,
		Active:           l.active,
		Reserved:         l.reserved,
		Queued:           l.queued(),
		QueuedBackground: l.bg.n,
		Grants:           l.grants,
		Preemptions:      l.preemptions,
	}
}

func (l *Lane) queued() int { return l.fg.n + l.bg.n }

// releaseFn is the release of one granted call. ls is the lease the call runs
// on, nil for a call that holds none.
func (l *Lane) releaseFn(ls *lease) func() {
	released := false
	return func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		if released {
			return
		}
		released = true
		l.finish(ls, false)
	}
}

// grant puts c's call in flight on a slot the caller has established is free,
// and starts a lease for it when c may hold one. Caller holds mu.
func (l *Lane) grant(c CallInfo) *lease {
	l.active++
	l.grants++
	// One reservation per owner per lane: an owner with a second call in
	// flight at once (a compaction beside its turn) runs that one as a plain
	// call, or it would hold two slots between calls.
	if c.Owner == "" || l.cfg.AffinityHold <= 0 || l.leases[c.Owner] != nil {
		return nil
	}
	ls := &lease{owner: c.Owner, busy: true, used: 1}
	l.leases[c.Owner] = ls
	return ls
}

// finish ends the call that ran on ls (nil = no lease): the slot is either
// kept reserved for its owner or handed on. abandoned is the cancelled-waiter
// path, where the call never ran. Caller holds mu.
func (l *Lane) finish(ls *lease, abandoned bool) {
	l.active--
	// A lease the lane no longer maps to its owner was ended by Done while the
	// call was in flight: the slot is simply free.
	if ls != nil && l.leases[ls.owner] == ls {
		ls.busy = false
		yield := l.cfg.QuantumCalls > 0 && ls.used >= l.cfg.QuantumCalls && l.queued() > 0
		switch {
		case abandoned,
			l.cfg.AffinityHold <= 0,
			// The lane shrank under this call: the slot is part of the excess.
			l.active+l.reserved >= l.cfg.MaxConcurrent,
			// The owner's other call is queued. Reserving the slot for the
			// owner's NEXT call would keep that one waiting on its own run.
			l.ownerQueued[ls.owner] > 0:
			delete(l.leases, ls.owner)
		case yield:
			delete(l.leases, ls.owner)
			l.preemptions++
		default:
			l.reserve(ls)
		}
	}
	l.dispatch()
}

// reserve keeps ls's slot for its owner for the hold. The timer is armed even
// with nobody waiting: a caller that queues DURING the hold must get the slot
// when the hold runs out, not whenever the owner next calls. Caller holds mu.
func (l *Lane) reserve(ls *lease) {
	l.reserved++
	hold := l.cfg.AffinityHold
	ls.deadline = time.Now().Add(hold)
	if ls.timer != nil {
		ls.timer.Reset(hold)
		return
	}
	ls.timer = time.AfterFunc(hold, func() { l.expire(ls) })
}

// expire ends ls's reservation when its hold has run out.
func (l *Lane) expire(ls *lease) {
	l.mu.Lock()
	defer l.mu.Unlock()
	// A firing that lost to the owner's return (or to Done) finds the lease
	// busy, gone, or reserved again with a later deadline.
	if l.leases[ls.owner] != ls || ls.busy || time.Now().Before(ls.deadline) {
		return
	}
	l.dropIdle(ls)
	l.dispatch()
}

// dropIdle ends an idle reservation. Caller holds mu and runs dispatch after.
func (l *Lane) dropIdle(ls *lease) {
	ls.timer.Stop()
	delete(l.leases, ls.owner)
	l.reserved--
}

// dispatch hands free slots to waiters until one or the other runs out.
// Caller holds mu. Every path that frees a slot or raises the cap ends here.
func (l *Lane) dispatch() {
	for l.active+l.reserved < l.cfg.MaxConcurrent && l.queued() > 0 {
		w := l.next()
		if w.info.Owner != "" {
			l.decOwnerQueued(w.info.Owner)
		}
		w.lease = l.grant(w.info)
		w.granted = true
		close(w.ch)
	}
}

// next picks the waiter to serve: foreground first, except that while both
// classes wait background earns BackgroundShare of a grant per grant and is
// served whenever it has a whole one saved up. Caller has checked queued() > 0.
func (l *Lane) next() *laneWaiter {
	if l.fg.n == 0 {
		return l.bg.pop()
	}
	if l.bg.n == 0 {
		return l.fg.pop()
	}
	l.bgCredit += l.cfg.BackgroundShare
	// The tolerance absorbs float drift in a sum like ten times 0.1.
	if l.cfg.BackgroundShare > 0 && l.bgCredit >= 1-1e-9 {
		l.bgCredit--
		return l.bg.pop()
	}
	return l.fg.pop()
}

// unqueue removes a waiter that gave up while still queued. Caller holds mu.
func (l *Lane) unqueue(w *laneWaiter) {
	if w.info.Background {
		l.bg.remove(w)
	} else {
		l.fg.remove(w)
	}
	if w.info.Owner != "" {
		l.decOwnerQueued(w.info.Owner)
	}
}

func (l *Lane) decOwnerQueued(owner string) {
	if l.ownerQueued[owner] <= 1 {
		delete(l.ownerQueued, owner)
		return
	}
	l.ownerQueued[owner]--
}
