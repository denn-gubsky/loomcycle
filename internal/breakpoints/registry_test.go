package breakpoints

import (
	"strings"
	"sync"
	"testing"
)

func TestParseSpec(t *testing.T) {
	for _, tc := range []struct {
		in, state, phase string
		ok               bool
	}{
		{"wave", "wave", "", true},
		{"wave:before_dispatch", "wave", BeforeDispatch, true},
		{"wave:after_collection", "wave", AfterCollection, true},
		// A state id may itself contain a colon, so only the LAST segment is a
		// phase candidate.
		{"team:wave:after_collection", "team:wave", AfterCollection, true},
		{"wave:typo", "", "", false},
		{"", "", "", false},
		{":before_dispatch", "", "", false},
	} {
		state, phase, err := ParseSpec(tc.in)
		if (err == nil) != tc.ok || state != tc.state || phase != tc.phase {
			t.Errorf("ParseSpec(%q) = (%q,%q,err=%v), want (%q,%q,ok=%v)",
				tc.in, state, phase, err, tc.state, tc.phase, tc.ok)
		}
	}
}

// TestSet_BareStateArmsBothPhases: "wave" means "stop at this state", which is
// what an operator means when they click one node.
func TestSet_BareStateArmsBothPhases(t *testing.T) {
	s, err := NewSet([]string{"wave"})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{BeforeDispatch, AfterCollection} {
		if !s.Armed("wave", p) {
			t.Errorf("a bare state did not arm %s", p)
		}
	}
	if s.Armed("other", BeforeDispatch) {
		t.Error("armed a state nobody named")
	}
}

// TestSet_ReplaceIsAtomicAndValidatesFirst: a rejected replace must leave the
// previous arming exactly as it was. An operator fixing a typo must not
// discover they have also disarmed everything that was working.
func TestSet_ReplaceIsAtomicAndValidatesFirst(t *testing.T) {
	s, err := NewSet([]string{"wave:before_dispatch"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Replace([]string{"review", "wave:typo"}); err == nil {
		t.Fatal("a malformed entry must refuse the whole list")
	}
	if !s.Armed("wave", BeforeDispatch) {
		t.Error("a REJECTED replace disarmed the previous set")
	}
	if s.Armed("review", BeforeDispatch) {
		t.Error("a REJECTED replace partially applied")
	}
	// And a good one does swap wholesale — this is a replace, not a merge.
	if err := s.Replace([]string{"review"}); err != nil {
		t.Fatal(err)
	}
	if s.Armed("wave", BeforeDispatch) {
		t.Error("Replace merged instead of replacing")
	}
	if !s.Armed("review", AfterCollection) {
		t.Error("Replace did not apply the new set")
	}
	// Disarming everything is the empty list — how a canvas turns Debug off.
	if err := s.Replace(nil); err != nil {
		t.Fatal(err)
	}
	if s.Armed("review", AfterCollection) || len(s.List()) != 0 {
		t.Errorf("the empty list must disarm everything, got %v", s.List())
	}
}

// TestSet_ListIsCanonical: a caller reading back its own arming should see what
// the walk will do, not an echo of its shorthand.
func TestSet_ListIsCanonical(t *testing.T) {
	s, _ := NewSet([]string{"zeta", "alpha:after_collection"})
	got := strings.Join(s.List(), " ")
	want := "alpha:after_collection zeta:after_collection zeta:before_dispatch"
	if got != want {
		t.Errorf("List() = %q, want %q", got, want)
	}
}

// TestSet_NilIsNotArmed: the walk holds a source that may be nil on paths where
// nothing was opened; it must answer false rather than panic.
func TestSet_NilIsNotArmed(t *testing.T) {
	var s *Set
	if s.Armed("wave", BeforeDispatch) {
		t.Error("a nil Set claimed to be armed")
	}
}

func TestRegistry_OpenGetRelease(t *testing.T) {
	r := NewRegistry()
	set, release, err := r.Open("run_1", []string{"wave"})
	if err != nil {
		t.Fatal(err)
	}
	got, ok := r.Get("run_1")
	if !ok || got != set {
		t.Fatal("Open did not register the set under its run")
	}
	release()
	if _, ok := r.Get("run_1"); ok {
		t.Error("the set outlived the walk — a breakpoint surviving into a later run would pause a workflow nobody is watching")
	}
}

// TestRegistry_TwoWalksShareOneRunsSet: the second walk to finish must not
// delete a set the first is still consulting, and a walk starting later must
// not silently drop an operator's arming.
func TestRegistry_TwoWalksShareOneRunsSet(t *testing.T) {
	r := NewRegistry()
	first, releaseFirst, _ := r.Open("run_1", []string{"wave"})
	second, releaseSecond, _ := r.Open("run_1", nil)
	if first != second {
		t.Fatal("two walks under one run must share the set")
	}
	if !second.Armed("wave", BeforeDispatch) {
		t.Error("the later walk replaced the arming instead of joining it")
	}
	releaseFirst()
	if _, ok := r.Get("run_1"); !ok {
		t.Fatal("releasing one walk removed a set the other is still using")
	}
	releaseSecond()
	if _, ok := r.Get("run_1"); ok {
		t.Error("the set outlived the last walk")
	}
}

// TestRegistry_NoRunIDIsDetached: a walk started outside a run has no handle to
// address, so it gets a working-but-unreachable set rather than a refusal.
func TestRegistry_NoRunIDIsDetached(t *testing.T) {
	r := NewRegistry()
	set, release, err := r.Open("", []string{"wave"})
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if !set.Armed("wave", BeforeDispatch) {
		t.Error("the dispatch-time arming must still work with no run id")
	}
	if _, ok := r.Get(""); ok {
		t.Error("a detached set must not be registered under the empty run id")
	}
}

// TestRegistry_OpenRefusesABadSeed: the run argument goes through the same
// parser as a mid-run arm, so a typo cannot enter by the other door.
func TestRegistry_OpenRefusesABadSeed(t *testing.T) {
	r := NewRegistry()
	if _, _, err := r.Open("run_1", []string{"wave:typo"}); err == nil {
		t.Fatal("a malformed seed was accepted")
	}
	if _, ok := r.Get("run_1"); ok {
		t.Error("a refused Open left an entry behind")
	}
}

// TestSet_ConcurrentArmAndReplace is the property the whole design rests on:
// the walk reads while an operator writes. Run with -race.
func TestSet_ConcurrentArmAndReplace(t *testing.T) {
	s, _ := NewSet(nil)
	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() { // the walk
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				_ = s.Armed("wave", BeforeDispatch)
			}
		}
	}()
	for i := 0; i < 200; i++ { // the operator
		if err := s.Replace([]string{"wave", "review:after_collection"}); err != nil {
			t.Fatal(err)
		}
		if err := s.Replace(nil); err != nil {
			t.Fatal(err)
		}
	}
	close(stop)
	wg.Wait()
}
