// Package breakpoints implements the in-memory per-run debug arming for a team
// walk. It mirrors internal/turncancel's run-keyed shape: a map (run_id → the
// live armed set) that vanishes when the process exits.
//
// WHY IT IS MUTABLE AND NOT A RUN ARGUMENT. Breakpoints arrived as an argument
// fixed at dispatch, which serves the case where you already know the workflow
// is broken. It does not serve the common one: you start a run expecting it to
// work, watch a wave go wrong, and want to stop before the next one. There is
// nothing to pass at that point — the run is already going — so the armed set
// has to be something the walk RE-READS rather than something it captured.
//
// That is the whole design: the walk consults a Set at every pause point, and
// an operator can change what that Set says while the walk is running.
//
// WHY IT IS NOT PERSISTED. The arming is live debugger state, meaningful only
// while a particular walk is in flight. Persisting it would create a worse
// problem than it solves — a breakpoint surviving into a later run of the same
// team, pausing a workflow nobody is watching, with the ask timing out and
// aborting the walk. The set dies with the walk, deliberately.
package breakpoints

import (
	"fmt"
	"sort"
	"strings"
	"sync"
)

// Phase mirrors teamrun.BreakpointPhase. It is duplicated as a string constant
// rather than imported because this package must stay a leaf — teamrun consumes
// it through an interface, and an import in the other direction would put the
// HTTP registry inside the graph-walker's dependency set.
//
// ParseSpec below is the single parser both sides use, so the two spellings
// cannot drift into accepting different arguments.
const (
	BeforeDispatch  = "before_dispatch"
	AfterCollection = "after_collection"
)

// Set is one run's armed breakpoints: state id → the phases armed on it.
//
// Safe for concurrent use by the walk goroutine (Armed, at every pause) and an
// API goroutine (Replace, when an operator changes the arming mid-run).
type Set struct {
	mu sync.RWMutex
	at map[string]map[string]bool
}

// NewSet builds a Set from breakpoint specs, refusing the whole list if any
// entry is malformed — a partially-applied arming is how an operator ends up
// watching a walk that never pauses at the state they typed.
func NewSet(specs []string) (*Set, error) {
	s := &Set{at: map[string]map[string]bool{}}
	if err := s.Replace(specs); err != nil {
		return nil, err
	}
	return s, nil
}

// Armed reports whether this state pauses at this phase. Called by the walk at
// every pause point, so it is the read that makes mid-run arming work.
func (s *Set) Armed(state, phase string) bool {
	if s == nil {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.at[state][phase]
}

// Replace swaps the whole armed set atomically.
//
// A whole-set replace rather than arm/disarm deltas: the caller holds the
// desired configuration and pushes it, so there is no read-modify-write race
// between two operators and no ordering question about which delta won.
// Disarming everything is the empty list, which is also how a canvas turns
// Debug off.
//
// It VALIDATES BEFORE it mutates, so a rejected call leaves the previous arming
// exactly as it was — an operator fixing a typo must not discover they have
// also disarmed everything that was working.
func (s *Set) Replace(specs []string) error {
	next := make(map[string]map[string]bool, len(specs))
	for _, spec := range specs {
		state, phase, err := ParseSpec(spec)
		if err != nil {
			return err
		}
		if next[state] == nil {
			next[state] = map[string]bool{}
		}
		if phase == "" {
			next[state][BeforeDispatch] = true
			next[state][AfterCollection] = true
			continue
		}
		next[state][phase] = true
	}
	s.mu.Lock()
	s.at = next
	s.mu.Unlock()
	return nil
}

// List renders the armed set back as canonical specs, sorted, always
// phase-qualified. Canonical rather than echoing what was sent: a caller
// reading back its own arming should see exactly what the walk will do, not its
// own shorthand.
func (s *Set) List() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []string
	for state, phases := range s.at {
		for phase, on := range phases {
			if on {
				out = append(out, state+":"+phase)
			}
		}
	}
	sort.Strings(out)
	return out
}

// ParseSpec splits one breakpoint spec into a state id and an optional phase.
//
//	"review"                   both phases
//	"review:after_collection"  that phase only
//
// A state id may itself contain a colon, so only the LAST segment is a phase
// candidate; if it is not a phase, the whole spec is refused rather than
// silently read as a state id that does not exist.
func ParseSpec(spec string) (state, phase string, err error) {
	state = spec
	if i := strings.LastIndex(spec, ":"); i >= 0 {
		state, phase = spec[:i], spec[i+1:]
		if phase != BeforeDispatch && phase != AfterCollection {
			return "", "", fmt.Errorf("breakpoint %q: expected \"<state>\" or \"<state>:%s\" or \"<state>:%s\"",
				spec, BeforeDispatch, AfterCollection)
		}
	}
	if strings.TrimSpace(state) == "" {
		return "", "", fmt.Errorf("breakpoint %q: missing the state id", spec)
	}
	return state, phase, nil
}

// Registry maps a live run_id → the armed set of the team walk running under
// it. An entry exists only while a walk is in flight.
//
// Keyed by RUN, not by walk: "debug this run" is what an operator means when
// they hit the button, and the run id is the handle every other run-scoped
// surface already uses (cancel, steer, interrupts). A single run driving two
// walks concurrently shares one set — the state ids come from different
// definitions, so they do not collide in practice, and one arming for one run
// is the behaviour an operator expects either way.
type Registry struct {
	mu   sync.Mutex
	sets map[string]*entry
}

type entry struct {
	set *Set
	// refs counts the walks sharing this run's set, so the second walk to
	// finish does not delete a set the first is still consulting.
	refs int
}

func NewRegistry() *Registry {
	return &Registry{sets: map[string]*entry{}}
}

// Open registers a walk's armed set under runID, seeded with the run argument,
// and returns it plus the release to call when the walk ends.
//
// An empty runID gets a detached Set: the arming still works for whatever was
// passed at dispatch, but nothing can reach it to change it. That is the honest
// outcome for a walk started outside a run (there is no handle to address), and
// it fails by being un-armable rather than by refusing to run.
func (r *Registry) Open(runID string, seed []string) (*Set, func(), error) {
	set, err := NewSet(seed)
	if err != nil {
		return nil, nil, err
	}
	// A nil registry (a Server assembled without one) degrades to a detached
	// set: the dispatch-time arming still works and nothing can reach it. That
	// is the documented fallback, and it must not be a panic inside a live walk.
	if r == nil || runID == "" {
		return set, func() {}, nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if e, ok := r.sets[runID]; ok {
		// A second walk under the same run joins the existing set rather than
		// replacing it, so an operator's arming is not silently dropped by a
		// walk that started later.
		e.refs++
		return e.set, func() { r.release(runID) }, nil
	}
	r.sets[runID] = &entry{set: set, refs: 1}
	return set, func() { r.release(runID) }, nil
}

func (r *Registry) release(runID string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.sets[runID]
	if !ok {
		return
	}
	e.refs--
	if e.refs <= 0 {
		delete(r.sets, runID)
	}
}

// Get returns the live set for a run, or false when no walk is in flight under
// it on this replica.
func (r *Registry) Get(runID string) (*Set, bool) {
	if r == nil {
		return nil, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.sets[runID]
	if !ok {
		return nil, false
	}
	return e.set, true
}
