package concurrency

import (
	"context"
	"log"
	"time"
)

// Why a run is waiting, as reported to AdmitRequest.OnQueued and by Position.
const (
	// QueueReasonUserCap: the run's (tenant, user) already has its cap of runs
	// active. It waits for one of them to end.
	QueueReasonUserCap = "user_cap"
	// QueueReasonGlobalCap: every run slot of the runtime is taken.
	QueueReasonGlobalCap = "global_cap"
)

// QueueInfo says where a waiting run stands.
type QueueInfo struct {
	Reason string
	// Position is 1 for the run served next. For QueueReasonUserCap it counts
	// only the same user's waiting runs, the only ones it can be served after.
	Position int
}

// AdmitRequest is one run asking for a run slot.
type AdmitRequest struct {
	TenantID, UserID string
	// Ticket names the waiting run (its run id) so Position can find it.
	// Empty: Position cannot report it.
	Ticket string
	// Reject refuses at once instead of waiting: *ErrPerUserQuotaExhausted
	// over the per-user cap, *BackpressureError when no slot is free.
	Reject bool
	// OnQueued is called when the run starts to wait, before it blocks, at most
	// once per reason: a run over its user's cap that then finds every slot
	// taken is reported twice. An error ends the wait and is returned.
	OnQueued func(QueueInfo) error
	// Slot, when set, is bound to the admitted run, so it can lend its global
	// slot while a call of it waits for a lane (RunSlot). The release Admit
	// returns then ends it.
	Slot *RunSlot
}

// Admit takes a run slot for a NEW run. Unlike Acquire it never times out: a
// run that cannot start waits until a slot is free or ctx ends. Over its
// user's cap it waits for one of that user's runs to end, up to
// WithQueuedPerUserCap waiting runs per user; past that, or with no per-user
// waiting configured, it is refused with *ErrPerUserQuotaExhausted. The queue
// bound (maxQueue) covers both kinds of wait; past it the run is refused with
// *BackpressureError.
//
// The per-user count is of runs holding a slot or waiting for one, as for
// AcquireForUser; a run waiting on its user's cap is not counted until it
// moves on. In cluster mode the count is the shared one and a run over the
// cap asks it again every second, since another replica's release cannot wake
// it.
func (s *Semaphore) Admit(ctx context.Context, r AdmitRequest) (release func(), err error) {
	key := userKey{tenant: r.TenantID, user: r.UserID}
	s.mu.Lock()
	qs, cap := s.quotaStore, s.maxPerUser
	s.mu.Unlock()
	perUser := cap > 0 && key.user != ""

	// waited: the run queued on its user's cap and still holds that place in
	// the queue, which it takes into the wait for a slot.
	waited := false
	if perUser {
		if qs != nil {
			waited, err = s.admitUserShared(ctx, qs, key, cap, r)
		} else {
			waited, err = s.admitUserLocal(ctx, key, cap, r)
		}
		if err != nil {
			return nil, err
		}
	}
	// acct is the key the release and cancel paths account to: the run's own
	// when it was counted against its user, none otherwise.
	acct, acctQS := userKey{}, userQuotaGate(nil)
	if perUser {
		acct, acctQS = key, qs
	}
	return s.admitGlobal(ctx, key, acct, acctQS, r, waited)
}

// AdmitSlot takes a run slot for a request sent directly, outside any run (the
// LLM gateway, /v1/embeddings), which its caller has already counted against
// its user with ReserveUser. It waits in the same round-robin queue as runs,
// keyed by (tenant, user), and never times out: only ctx ends the wait. It
// counts no user, so it is bounded like any run not counted against its user:
// max_queue_depth in all, max_queued_runs_per_user per key; past either it is
// refused with *BackpressureError.
func (s *Semaphore) AdmitSlot(ctx context.Context, tenantID, userID string) (release func(), err error) {
	key := userKey{tenant: tenantID, user: userID}
	return s.admitGlobal(ctx, key, userKey{}, nil, AdmitRequest{TenantID: tenantID, UserID: userID}, false)
}

// admitUserLocal counts the run against its user's cap, waiting in that user's
// FIFO when the cap is reached. On return without error the run is counted, and
// waited reports that it queued: it then still holds its place in the queue
// (counted in queued), for admitGlobal to take over.
func (s *Semaphore) admitUserLocal(ctx context.Context, key userKey, cap int, r AdmitRequest) (waited bool, err error) {
	s.mu.Lock()
	if s.perUser == nil {
		s.perUser = map[userKey]int{}
	}
	if s.perUser[key] < cap {
		s.perUser[key]++
		s.mu.Unlock()
		return false, nil
	}
	if r.Reject || len(s.userWait[key]) >= s.maxQueuedPerUser {
		s.mu.Unlock()
		return false, &ErrPerUserQuotaExhausted{TenantID: key.tenant, UserID: key.user, Cap: cap}
	}
	if s.queued+s.lent >= s.maxQueue {
		s.mu.Unlock()
		return false, &BackpressureError{msg: "queue full"}
	}
	w := &semWaiter{ch: make(chan struct{}, 1), key: key, ticket: r.Ticket, reason: QueueReasonUserCap}
	if s.userWait == nil {
		s.userWait = map[userKey][]*semWaiter{}
	}
	s.userWait[key] = append(s.userWait[key], w)
	s.queued++
	s.track(w)
	pos := len(s.userWait[key])
	s.mu.Unlock()

	if r.OnQueued != nil {
		if err := r.OnQueued(QueueInfo{Reason: QueueReasonUserCap, Position: pos}); err != nil {
			s.cancelUserWaiter(w, key)
			return false, err
		}
	}
	select {
	case <-w.ch:
		// wakeUserWaiter counted the run against its user and dequeued it.
		return true, nil
	case <-ctx.Done():
		s.cancelUserWaiter(w, key)
		return false, ctx.Err()
	}
}

// cancelUserWaiter withdraws a run waiting on its user's cap. If it was woken
// in the meantime it holds a per-user count it will never use: give it back,
// which may wake the user's next waiting run.
func (s *Semaphore) cancelUserWaiter(w *semWaiter, key userKey) {
	s.mu.Lock()
	defer s.mu.Unlock()
	q := s.userWait[key]
	for i, x := range q {
		if x == w {
			q = append(q[:i], q[i+1:]...)
			if len(q) == 0 {
				delete(s.userWait, key)
			} else {
				s.userWait[key] = q
			}
			s.queued--
			s.untrack(w)
			return
		}
	}
	<-w.ch     // woken: the token is in the buffer (sent under s.mu)
	s.queued-- // the place it kept for the wait for a slot
	s.decrementPerUser(key)
}

// wakeUserWaiter moves the user's runs waiting on its cap on, oldest first,
// while the user is under the cap. A cap of 0 is none (a reload turned it
// off): every waiting run moves on. Caller holds s.mu.
func (s *Semaphore) wakeUserWaiter(key userKey) {
	for {
		q := s.userWait[key]
		if len(q) == 0 || (s.maxPerUser > 0 && s.perUser[key] >= s.maxPerUser) {
			return
		}
		w := q[0]
		q[0] = nil
		if q = q[1:]; len(q) == 0 {
			delete(s.userWait, key)
		} else {
			s.userWait[key] = q
		}
		// queued is left as is: the run keeps its place for the wait for a
		// slot, so a run that arrives meanwhile cannot fill the queue and have
		// a run that already waited refused as "queue full".
		s.perUser[key]++
		s.untrack(w)
		w.ch <- struct{}{}
	}
}

// admitUserShared is admitUserLocal against the cluster-wide counter. A run
// over the cap polls it; the waiting-run bound is per replica.
func (s *Semaphore) admitUserShared(ctx context.Context, qs userQuotaGate, key userKey, cap int, r AdmitRequest) (waited bool, err error) {
	ok, err := qs.TryAcquire(ctx, key.tenant, key.user, cap)
	if err != nil {
		return false, wrapQuotaErr(err)
	}
	if ok {
		return false, nil
	}
	if r.Reject {
		return false, &ErrPerUserQuotaExhausted{TenantID: key.tenant, UserID: key.user, Cap: cap}
	}
	s.mu.Lock()
	if s.userPolling[key] >= s.maxQueuedPerUser {
		s.mu.Unlock()
		return false, &ErrPerUserQuotaExhausted{TenantID: key.tenant, UserID: key.user, Cap: cap}
	}
	if s.queued+s.lent >= s.maxQueue {
		s.mu.Unlock()
		return false, &BackpressureError{msg: "queue full"}
	}
	if s.userPolling == nil {
		s.userPolling = map[userKey]int{}
	}
	s.userPolling[key]++
	s.queued++
	pos := s.userPolling[key]
	w := &semWaiter{key: key, ticket: r.Ticket, reason: QueueReasonUserCap, pos: pos}
	s.track(w)
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		if s.userPolling[key]--; s.userPolling[key] <= 0 {
			delete(s.userPolling, key)
		}
		// A run that got its count keeps its place for the wait for a slot.
		if !waited {
			s.queued--
		}
		s.untrack(w)
		s.mu.Unlock()
	}()

	if r.OnQueued != nil {
		if err := r.OnQueued(QueueInfo{Reason: QueueReasonUserCap, Position: pos}); err != nil {
			return false, err
		}
	}
	t := time.NewTicker(s.userPollInterval())
	defer t.Stop()
	logged := false
	for {
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-t.C:
		}
		ok, err := qs.TryAcquire(ctx, key.tenant, key.user, cap)
		if err != nil {
			// A database hiccup must not end a wait that has no timeout; the
			// next tick asks again.
			if !logged {
				log.Printf("concurrency: per-user quota check while queued: %v", err)
				logged = true
			}
			continue
		}
		if ok {
			return true, nil
		}
	}
}

// admitGlobal takes a run slot, waiting round-robin with the other users'
// runs when none is free. acct/acctQS are what a release or a cancel gives
// back for the user (none when the run was not counted against its user).
// holdsPlace is a run that already waited on its user's cap: it is counted in
// queued already, and is never refused for a full queue — it was let into the
// queue once.
func (s *Semaphore) admitGlobal(ctx context.Context, key, acct userKey, acctQS userQuotaGate, r AdmitRequest, holdsPlace bool) (func(), error) {
	s.mu.Lock()
	if s.active < s.maxConcurrent {
		s.active++
		if holdsPlace {
			s.queued--
		}
		s.mu.Unlock()
		return s.runRelease(acct, acctQS, r.Slot), nil
	}
	if !holdsPlace && (r.Reject || s.queued+s.lent >= s.maxQueue) {
		msg := "queue full"
		if r.Reject {
			msg = "no free slot"
		}
		s.giveBackUserLocked(acct, acctQS)
		s.mu.Unlock()
		return nil, &BackpressureError{msg: msg}
	}
	// A run not counted against a per-user cap (the cap is off, the default,
	// or the run names no user) has nothing else bounding its key's share of
	// the queue. Since a queued run never times out, one caller could hold
	// every place and turn every other tenant's runs away as "queue full".
	// A run counted against its user needs no check: the key's waiting runs
	// are already at most its per-user cap.
	if acct.user == "" && s.maxQueuedPerUser > 0 {
		if b := s.queue.buckets[key]; b != nil && len(b.waiters) >= s.maxQueuedPerUser {
			s.mu.Unlock()
			return nil, &BackpressureError{msg: "queue full for this user"}
		}
	}
	w := &semWaiter{ch: make(chan struct{}, 1), key: key, ticket: r.Ticket, reason: QueueReasonGlobalCap}
	s.queue.push(w)
	if !holdsPlace {
		s.queued++
	}
	s.track(w)
	pos := s.positionLocked(w)
	s.mu.Unlock()

	if r.OnQueued != nil {
		if err := r.OnQueued(QueueInfo{Reason: QueueReasonGlobalCap, Position: pos}); err != nil {
			s.cancelWaiter(w, acct, acctQS)
			return nil, err
		}
	}
	select {
	case <-w.ch:
		return s.runRelease(acct, acctQS, r.Slot), nil
	case <-ctx.Done():
		s.cancelWaiter(w, acct, acctQS)
		return nil, ctx.Err()
	}
}

// giveBackUserLocked returns a per-user count taken for a run that is then
// refused. Caller holds s.mu.
func (s *Semaphore) giveBackUserLocked(acct userKey, qs userQuotaGate) {
	if acct.user == "" {
		return
	}
	if qs != nil {
		go releaseInBackground(qs, acct)
		return
	}
	s.decrementPerUser(acct)
}

// Position reports where the waiting run named ticket stands, and false when
// no run of that name is waiting (it started, ended, or was never queued).
func (s *Semaphore) Position(ticket string) (QueueInfo, bool) {
	if ticket == "" {
		return QueueInfo{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	w := s.tickets[ticket]
	if w == nil {
		return QueueInfo{}, false
	}
	if w.reason == QueueReasonGlobalCap {
		return QueueInfo{Reason: w.reason, Position: s.positionLocked(w)}, true
	}
	if w.ch == nil { // cluster mode: no order among a user's pollers
		return QueueInfo{Reason: w.reason, Position: w.pos}, true
	}
	for i, x := range s.userWait[w.key] {
		if x == w {
			return QueueInfo{Reason: w.reason, Position: i + 1}, true
		}
	}
	return QueueInfo{}, false
}

// positionLocked counts the waiters served before w, plus one: every Wait-er,
// then, round by round, one from each bucket in ring order. Caller holds s.mu
// and w is in s.queue.
func (s *Semaphore) positionLocked(w *semWaiter) int {
	b := s.queue.buckets[w.key]
	if b == nil {
		return 0
	}
	idx := 0
	for i, x := range b.waiters {
		if x == w {
			idx = i
			break
		}
	}
	ahead := len(s.admitted) + idx
	seen := false
	for _, rb := range s.queue.ring {
		if rb == b {
			seen = true
			continue
		}
		n := len(rb.waiters)
		if !seen {
			n = min(n, idx+1) // served in the same round as w, before it
		} else {
			n = min(n, idx)
		}
		ahead += n
	}
	return ahead + 1
}

func (s *Semaphore) track(w *semWaiter) {
	if w.ticket == "" {
		return
	}
	if s.tickets == nil {
		s.tickets = map[string]*semWaiter{}
	}
	s.tickets[w.ticket] = w
}

func (s *Semaphore) untrack(w *semWaiter) {
	if w.ticket != "" && s.tickets[w.ticket] == w {
		delete(s.tickets, w.ticket)
	}
}

// WithQueuedPerUserCap sets how many runs one (tenant, user) may have waiting
// on its per-user cap (Admit). 0, the default, means none: a run over the cap
// is refused, as AcquireForUser refuses it.
func (s *Semaphore) WithQueuedPerUserCap(n int) *Semaphore {
	s.mu.Lock()
	s.maxQueuedPerUser = n
	s.mu.Unlock()
	return s
}

func (s *Semaphore) userPollInterval() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.userPoll > 0 {
		return s.userPoll
	}
	return time.Second
}
