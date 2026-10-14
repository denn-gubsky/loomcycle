package concurrency

// semWaiter is one Semaphore request waiting for a slot. ch is buffered 1; the
// releasing holder sends the slot into it under Semaphore.mu.
type semWaiter struct {
	ch  chan struct{}
	key userKey
}

// semBucket is the FIFO of one (tenant, user)'s waiting requests.
type semBucket struct {
	key     userKey
	waiters []*semWaiter
}

// semQueue holds the Semaphore's Acquire waiters: FIFO within a (tenant, user),
// round-robin across them. ring holds only non-empty buckets, in the order they
// will be served. It mirrors the lane's laneQueue, for whole runs instead of
// model calls.
type semQueue struct {
	buckets map[userKey]*semBucket
	ring    []*semBucket
	n       int
}

func (q *semQueue) push(w *semWaiter) {
	b := q.buckets[w.key]
	if b == nil {
		if q.buckets == nil {
			q.buckets = map[userKey]*semBucket{}
		}
		b = &semBucket{key: w.key}
		q.buckets[w.key] = b
		// A bucket that just became non-empty joins at the back: it waits one
		// full round, the same as a bucket that was just served.
		q.ring = append(q.ring, b)
	}
	b.waiters = append(b.waiters, w)
	q.n++
}

// pop takes the oldest waiter of the bucket whose turn it is and moves that
// bucket to the back. Caller has checked n > 0.
func (q *semQueue) pop() *semWaiter {
	b := q.ring[0]
	w := b.waiters[0]
	b.waiters[0] = nil
	b.waiters = b.waiters[1:]
	q.ring[0] = nil
	q.ring = q.ring[1:]
	if len(b.waiters) == 0 {
		delete(q.buckets, b.key)
	} else {
		q.ring = append(q.ring, b)
	}
	q.n--
	return w
}

// remove drops a waiter that is still queued and reports whether it was. It
// keeps the bucket's place in the ring: a cancel must not cost the bucket's
// other waiters their turn.
func (q *semQueue) remove(w *semWaiter) bool {
	b := q.buckets[w.key]
	if b == nil {
		return false
	}
	for i, x := range b.waiters {
		if x != w {
			continue
		}
		b.waiters = append(b.waiters[:i], b.waiters[i+1:]...)
		q.n--
		if len(b.waiters) == 0 {
			delete(q.buckets, w.key)
			for j, rb := range q.ring {
				if rb == b {
					q.ring = append(q.ring[:j], q.ring[j+1:]...)
					break
				}
			}
		}
		return true
	}
	return false
}

// nextWaiter removes and returns the waiter that gets the next free slot, or
// nil when none waits: already-admitted work (Wait) first, then the round-robin
// queue. Caller holds s.mu.
func (s *Semaphore) nextWaiter() *semWaiter {
	if len(s.admitted) > 0 {
		w := s.admitted[0]
		s.admitted[0] = nil
		s.admitted = s.admitted[1:]
		return w
	}
	if s.queue.n > 0 {
		return s.queue.pop()
	}
	return nil
}

// removeWaiter drops w from whichever queue holds it and reports whether it was
// still queued. Caller holds s.mu.
func (s *Semaphore) removeWaiter(w *semWaiter) bool {
	for i, x := range s.admitted {
		if x == w {
			s.admitted = append(s.admitted[:i], s.admitted[i+1:]...)
			return true
		}
	}
	return s.queue.remove(w)
}
