// Package concurrency caps simultaneous agent runs and queues bursts.
//
// Same shape as jobs-search-agent's concurrency.ts: a counting semaphore plus
// a bounded FIFO queue with a per-acquire timeout. Excess returns BackpressureError.
package concurrency

import (
	"context"
	"errors"
	"sync"
	"time"
)

// BackpressureError signals the queue is full; the caller should HTTP 429.
type BackpressureError struct{ msg string }

func (e *BackpressureError) Error() string { return e.msg }

// Is supports errors.Is for code-level matching.
func (e *BackpressureError) Code() string { return "backpressure" }

// Semaphore caps concurrent acquisitions. Acquire blocks (with timeout) when
// slots are full and the queue has room; returns BackpressureError when the
// queue is also full.
type Semaphore struct {
	maxConcurrent int
	maxQueue      int
	timeout       time.Duration

	mu      sync.Mutex
	active  int
	queued  int
	waiters []chan struct{}
}

// New constructs a Semaphore. timeout is the max wait per acquire.
func New(maxConcurrent, maxQueue int, timeout time.Duration) *Semaphore {
	return &Semaphore{
		maxConcurrent: maxConcurrent,
		maxQueue:      maxQueue,
		timeout:       timeout,
	}
}

// Acquire returns a release func. Call release exactly once when done.
// Returns ctx.Err() if ctx cancels first; *BackpressureError if queue is full.
func (s *Semaphore) Acquire(ctx context.Context) (release func(), err error) {
	s.mu.Lock()
	if s.active < s.maxConcurrent {
		s.active++
		s.mu.Unlock()
		return s.releaseFn(), nil
	}
	if s.queued >= s.maxQueue {
		s.mu.Unlock()
		return nil, &BackpressureError{msg: "queue full"}
	}
	w := make(chan struct{}, 1)
	s.waiters = append(s.waiters, w)
	s.queued++
	s.mu.Unlock()

	timer := time.NewTimer(s.timeout)
	defer timer.Stop()

	select {
	case <-w:
		// Slot acquired.
		return s.releaseFn(), nil
	case <-ctx.Done():
		s.cancelWaiter(w)
		return nil, ctx.Err()
	case <-timer.C:
		s.cancelWaiter(w)
		return nil, &BackpressureError{msg: "queue timeout"}
	}
}

// Stats returns a snapshot for /metrics or admin views.
func (s *Semaphore) Stats() (active, queued int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.active, s.queued
}

func (s *Semaphore) releaseFn() func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			s.active--
			// Wake the next waiter if any.
			if len(s.waiters) > 0 {
				w := s.waiters[0]
				s.waiters = s.waiters[1:]
				s.queued--
				s.active++
				select {
				case w <- struct{}{}:
				default:
				}
			}
		})
	}
}

func (s *Semaphore) cancelWaiter(target chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, w := range s.waiters {
		if w == target {
			s.waiters = append(s.waiters[:i], s.waiters[i+1:]...)
			s.queued--
			return
		}
	}
}

// IsBackpressure reports whether err is a BackpressureError.
func IsBackpressure(err error) bool {
	var b *BackpressureError
	return errors.As(err, &b)
}
