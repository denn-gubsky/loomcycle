package http

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// A throttled check (a password sign-in, a second-factor code on a reset or
// invite link) reads its key's hold, does the check, and records a failure;
// only a recorded failure sets a hold. Checks that arrive together would all
// pass the read before any of them recorded, so the hold would pace batches
// rather than guesses. Each check therefore takes a place on its key first:
// a key admits as many checks at once as could fail one after another with no
// hold between them (checksAtOnce), and a check over that waits, briefly, for
// a place and then reads the key again. A login takes one sign-in at a time
// (turn), which bounds the account's lock the same way.
//
// The places are this replica's; the failures and the holds are the store's.
// N replicas admit up to N times the allowance per hold.

const (
	// checkWait is how long one request waits, in all, for places. A place is
	// held for one check — a password hash (tens of milliseconds) and a few
	// store writes — so people whose sign-ins overlap are through in well
	// under a second; the bound leaves room for a busy hasher and still ends
	// a wait that a stuck or slow check would otherwise drag out.
	checkWait = 3 * time.Second
	// maxCheckWaiters is how many requests may wait on one key; the next is
	// refused at once. It is the hasher's width: more could not be checked
	// together anyway, and a flood parks at most this many goroutines per key
	// for checkWait.
	maxCheckWaiters = 8
	// maxChecksAtOnce bounds checksAtOnce for a curve that never holds.
	maxChecksAtOnce = 64
)

// checksAtOnce is how many checks may be in progress together on a key that
// has failures behind it: as many as could fail one after another with no
// hold between them, so the last of them is the one whose failure sets a
// hold. It is one once the key is past its free failures.
func checksAtOnce(failures int, blockFor func(int) time.Duration) int {
	n := 1
	for n < maxChecksAtOnce && blockFor(failures+n) <= 0 {
		n++
	}
	return n
}

// checkPlaces counts, per key, the checks in progress and the requests
// waiting for a place. The zero value is ready.
type checkPlaces struct {
	mu   sync.Mutex
	keys map[string]*checkKey
	// wait and after replace checkWait and time.After in tests; they are set
	// before the server serves and never changed.
	wait  time.Duration
	after func(time.Duration) <-chan time.Time
}

type checkKey struct {
	held, waiting int
	// freed is closed, and replaced, each time a place is given back.
	freed chan struct{}
}

// checkPatience is one request's wait budget, shared by every key it waits
// on. Its clock starts at the first wait.
type checkPatience struct{ over <-chan time.Time }

const (
	placeOut = iota
	placeHeld
	placeWaiting
)

// checkPlace is one request's standing on one key. It belongs to the
// request's goroutine.
type checkPlace struct {
	p     *checkPlaces
	key   string
	state int
	// n is how many places were held on the key, this one included, when
	// this one was taken.
	n int
	// freed is the key's channel as it was when the place was taken: a place
	// given back at any time after that closes it, so a wait on it cannot
	// miss one.
	freed <-chan struct{}
}

func (p *checkPlaces) keyLocked(key string) *checkKey {
	k := p.keys[key]
	if k == nil {
		if p.keys == nil {
			p.keys = make(map[string]*checkKey)
		}
		k = &checkKey{freed: make(chan struct{})}
		p.keys[key] = k
	}
	return k
}

func (p *checkPlaces) dropIfIdleLocked(key string, k *checkKey) {
	if k.held <= 0 && k.waiting <= 0 {
		delete(p.keys, key)
	}
}

// take counts one more check on key, whatever the count is. The caller
// defers leave at once and judges pl.n against what it then reads from the
// store: counted first, a failure recorded before the read is in what is
// read, and a check still in progress is in n.
func (p *checkPlaces) take(key string) *checkPlace {
	p.mu.Lock()
	defer p.mu.Unlock()
	k := p.keyLocked(key)
	k.held++
	return &checkPlace{p: p, key: key, state: placeHeld, n: k.held, freed: k.freed}
}

// leave gives the place back, from any state, any number of times.
func (pl *checkPlace) leave() {
	if pl.state == placeOut {
		return
	}
	pl.p.mu.Lock()
	defer pl.p.mu.Unlock()
	k := pl.p.keyLocked(pl.key)
	if pl.state == placeHeld {
		k.held--
		close(k.freed)
		k.freed = make(chan struct{})
	} else {
		k.waiting--
	}
	pl.state = placeOut
	pl.p.dropIfIdleLocked(pl.key, k)
}

// stepBack turns a held place that is over its key's allowance (allow, as
// the caller just read it) into a wait for one; false, and the place is gone,
// when the key already has its fill of waiters.
//
// A place over the allowance still counted while its request read the store,
// so it may be what put another request over. When stepping back leaves room
// under the allowance, everyone waiting is woken to count again, this
// request included (its own wait ends at once): otherwise requests that were
// only ever over because of each other would all wait on a key nobody holds.
// With no room left nobody is woken, so waiters do not spin while the key is
// full.
func (pl *checkPlace) stepBack(allow int) bool {
	pl.p.mu.Lock()
	defer pl.p.mu.Unlock()
	k := pl.p.keyLocked(pl.key)
	k.held--
	if k.held < allow {
		close(k.freed)
		k.freed = make(chan struct{})
	} else if k.waiting >= maxCheckWaiters {
		// Only a key that is full turns a waiter away: with room, this
		// request's wait is over as it starts.
		pl.state = placeOut
		pl.p.dropIfIdleLocked(pl.key, k)
		return false
	}
	k.waiting++
	pl.state = placeWaiting
	return true
}

// rejoin waits for a place to be given back on the key and takes one again
// (n is counted afresh; the caller reads the store afresh). false, and the
// place is gone, when the request's patience or its context ends first.
func (pl *checkPlace) rejoin(ctx context.Context, patience *checkPatience) bool {
	if patience.over == nil {
		wait, after := pl.p.wait, pl.p.after
		if wait <= 0 {
			wait = checkWait
		}
		if after == nil {
			after = time.After
		}
		patience.over = after(wait)
	}
	select {
	case <-pl.freed:
	case <-patience.over:
		pl.leave()
		return false
	case <-ctx.Done():
		pl.leave()
		return false
	}
	pl.p.mu.Lock()
	defer pl.p.mu.Unlock()
	k := pl.p.keyLocked(pl.key)
	k.waiting--
	k.held++
	pl.state, pl.n, pl.freed = placeHeld, k.held, k.freed
	return true
}

// turn admits the request to a key that takes one check at a time, waiting
// for the checks ahead of it; false, and no place, when its patience or its
// context ends first or the key has its fill of waiters.
func (p *checkPlaces) turn(ctx context.Context, key string, patience *checkPatience) (_ *checkPlace, ok bool) {
	place := p.take(key)
	defer func() {
		if !ok {
			place.leave()
		}
	}()
	for place.n > 1 {
		if !place.stepBack(1) || !place.rejoin(ctx, patience) {
			return nil, false
		}
	}
	return place, true
}

// reserveCheck admits one check on a throttle key and returns its place, or
// refuses it (ok=false, no place): the key is held back, or it stayed over
// its allowance for as long as the request may wait. A request that waited
// is judged on what the store says after it got its place, never on what it
// read before: the check it waited for may have just failed and set a hold.
func (s *Server) reserveCheck(ctx context.Context, key string, blockFor func(int) time.Duration, patience *checkPatience) (_ *checkPlace, ok bool) {
	place := s.checks.take(key)
	// On every way out but admission, a panic in the store included.
	defer func() {
		if !ok {
			place.leave()
		}
	}()
	for {
		failures, until, err := s.store.AuthThrottleGet(ctx, key, time.Now())
		if err != nil {
			// Fail closed: a throttle that cannot be read admits no check.
			slog.Warn("authgate: throttle read failed (refusing)", "err", err)
			return nil, false
		}
		if until.After(time.Now()) {
			return nil, false
		}
		allow := checksAtOnce(failures, blockFor)
		if place.n <= allow {
			return place, true
		}
		if !place.stepBack(allow) || !place.rejoin(ctx, patience) {
			return nil, false
		}
	}
}
