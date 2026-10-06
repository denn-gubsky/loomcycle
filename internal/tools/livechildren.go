package tools

import (
	"fmt"
	"sync"
)

// LiveChildren bounds how many children each run has alive at once, across
// every way a run starts one: a spawn or parallel_spawn child, a resident
// child, and anything else that registers here. "Alive" is admitted and not
// yet finished — a parallel_spawn child queued behind its call's concurrency
// counts, because the call has committed to running it.
//
// It is per run, not per tree: a child's own children count against the
// child. The depth cap is what bounds the tree.
//
// A nil *LiveChildren admits everything, as does a run with no id (nothing to
// count against).
type LiveChildren struct {
	limit func() int

	mu    sync.Mutex
	alive map[string]int
}

// NewLiveChildren returns a registry whose limit is read on every admission,
// so a config reload applies to the next spawn.
func NewLiveChildren(limit func() int) *LiveChildren {
	return &LiveChildren{limit: limit, alive: map[string]int{}}
}

// LiveChildLimitError is a refused admission: the run already has Alive
// children and Want more would take it past Limit.
type LiveChildLimitError struct {
	Alive, Want, Limit int
}

func (e *LiveChildLimitError) Error() string {
	return fmt.Sprintf("this run has %d children alive and may have at most %d at once; starting %d more would exceed that",
		e.Alive, e.Limit, e.Want)
}

// Admit reserves n children for runID, all or none. On success it returns n
// release funcs, one per child; each frees its slot once and is a no-op after.
// Call each when its child ends.
func (l *LiveChildren) Admit(runID string, n int) ([]func(), error) {
	releases := make([]func(), n)
	if l == nil || runID == "" || n <= 0 {
		for i := range releases {
			releases[i] = func() {}
		}
		return releases, nil
	}
	l.mu.Lock()
	limit, alive := l.limit(), l.alive[runID]
	if limit > 0 && alive+n > limit {
		l.mu.Unlock()
		return nil, &LiveChildLimitError{Alive: alive, Want: n, Limit: limit}
	}
	l.alive[runID] = alive + n
	l.mu.Unlock()
	for i := range releases {
		var once sync.Once
		releases[i] = func() { once.Do(func() { l.release(runID) }) }
	}
	return releases, nil
}

func (l *LiveChildren) release(runID string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.alive[runID]--; l.alive[runID] <= 0 {
		delete(l.alive, runID)
	}
}

// Alive is how many children runID has alive now.
func (l *LiveChildren) Alive(runID string) int {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.alive[runID]
}
