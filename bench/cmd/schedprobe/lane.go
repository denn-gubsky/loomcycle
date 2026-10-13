package main

import (
	"context"
	"sync"
	"time"
)

// lane is the probe's stand-in for one RFC EI call lane: a fixed number of slots
// and a FIFO of waiting agents. Every policy the experiments compare is one
// lane configured two ways:
//
//   - quantum: how many consecutive calls a slot's owner may make while another
//     agent waits; 0 = unlimited.
//   - hold: how long a slot stays reserved for its owner after a call, so the
//     owner's next call finds its cache warm; holdForever keeps it until the
//     owner is done.
//
// So the run-level gate loomcycle has today is quantum 0 + holdForever (a slot
// for the whole conversation), call-level round-robin is quantum 1 + hold 0, and
// everything RFC EI proposes lies between.
//
// An agent owns at most one slot: its calls are sequential.
type lane struct {
	quantum int
	hold    time.Duration

	mu      sync.Mutex
	slots   []slot
	waiters []*waiter
}

// holdForever reserves a slot for its owner until done() — today's run-level gate.
const holdForever time.Duration = -1

type slot struct {
	owner string // "" = free
	busy  bool   // a call is in flight on it
	used  int    // calls the owner has made since it got the slot
	// idleSince is when the owner's last call ended; the hold runs from here.
	idleSince time.Time
	// gen invalidates a hold timer armed by an earlier release.
	gen int
}

type waiter struct {
	agent string
	ch    chan int // receives the slot index handed over
}

func newLane(slots, quantum int, hold time.Duration) *lane {
	return &lane{quantum: quantum, hold: hold, slots: make([]slot, slots)}
}

// acquire blocks until agent may make one call, and returns the slot it runs on.
// It never times out: only ctx ends the wait.
func (l *lane) acquire(ctx context.Context, agent string) (int, error) {
	l.mu.Lock()
	if i := l.ownedBy(agent); i >= 0 {
		s := &l.slots[i]
		if l.quantum > 0 && s.used >= l.quantum && len(l.waiters) > 0 {
			// Its turn is over: the slot goes to the oldest waiter and this agent
			// queues behind everyone already waiting.
			l.handOff(i)
		} else {
			s.busy = true
			s.used++
			l.mu.Unlock()
			return i, nil
		}
	}
	if i := l.freeSlot(); i >= 0 {
		l.slots[i] = slot{owner: agent, busy: true, used: 1}
		l.mu.Unlock()
		return i, nil
	}
	w := &waiter{agent: agent, ch: make(chan int, 1)}
	l.waiters = append(l.waiters, w)
	l.mu.Unlock()

	select {
	case i := <-w.ch:
		return i, nil
	case <-ctx.Done():
		l.mu.Lock()
		for k, x := range l.waiters {
			if x == w {
				l.waiters = append(l.waiters[:k], l.waiters[k+1:]...)
				l.mu.Unlock()
				return -1, ctx.Err()
			}
		}
		l.mu.Unlock()
		// A slot was handed over as the wait was cancelled: give it back.
		i := <-w.ch
		l.release(agent, i)
		l.done(agent)
		return -1, ctx.Err()
	}
}

// release ends one call on slot i. The slot stays the agent's for the hold, unless
// someone is waiting and the agent's turn is over.
func (l *lane) release(agent string, i int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	s := &l.slots[i]
	if s.owner != agent {
		return
	}
	s.busy = false
	s.idleSince = time.Now()
	s.gen++
	if l.hold == holdForever {
		return
	}
	if len(l.waiters) > 0 && (l.hold == 0 || (l.quantum > 0 && s.used >= l.quantum)) {
		l.handOff(i)
		return
	}
	if l.hold == 0 {
		return // idle and unreserved: the next newcomer takes it in freeSlot
	}
	// Armed even with nobody waiting: an agent that queues during the hold must
	// get the slot when it runs out, not when the owner next calls.
	gen := s.gen
	time.AfterFunc(l.hold, func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		s := &l.slots[i]
		if s.gen == gen && !s.busy && s.owner == agent && len(l.waiters) > 0 {
			l.handOff(i)
		}
	})
}

// done gives up whatever slot agent still holds: its conversation is over.
func (l *lane) done(agent string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for i := range l.slots {
		s := &l.slots[i]
		if s.owner == agent && !s.busy {
			s.owner = ""
			s.gen++
			if len(l.waiters) > 0 {
				l.handOff(i)
			}
		}
	}
}

// handOff gives slot i to the oldest waiter. Caller holds mu and has checked
// there is one.
func (l *lane) handOff(i int) {
	w := l.waiters[0]
	l.waiters = l.waiters[1:]
	l.slots[i] = slot{owner: w.agent, busy: true, used: 1, gen: l.slots[i].gen + 1}
	w.ch <- i
}

// ownedBy is the slot agent owns, or -1.
func (l *lane) ownedBy(agent string) int {
	for i, s := range l.slots {
		if s.owner == agent {
			return i
		}
	}
	return -1
}

// freeSlot is a slot nobody owns, or an idle one whose hold has run out; -1 when
// there is none. Caller holds mu.
func (l *lane) freeSlot() int {
	for i, s := range l.slots {
		if s.owner == "" {
			return i
		}
	}
	if l.hold == holdForever {
		return -1
	}
	for i, s := range l.slots {
		if !s.busy && time.Since(s.idleSince) >= l.hold {
			return i
		}
	}
	return -1
}

// policyLane builds the lane for a named policy. none returns nil: no gate.
func policyLane(policy string, slots, quantum int, hold time.Duration) *lane {
	switch policy {
	case "none":
		return nil
	case "run":
		return newLane(slots, 0, holdForever)
	case "call":
		return newLane(slots, 1, 0)
	default: // lease
		return newLane(slots, quantum, hold)
	}
}
