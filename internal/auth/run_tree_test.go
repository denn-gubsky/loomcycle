package auth

import (
	"errors"
	"fmt"
	"testing"
)

// treeReader serves runs from a map and counts its reads.
type treeReader struct {
	runs  map[string]RunLink
	reads int
}

func (r *treeReader) read(id string) (RunLink, error) {
	r.reads++
	l, ok := r.runs[id]
	if !ok {
		return RunLink{}, errors.New("no such run")
	}
	return l, nil
}

func restrictedTo(agents ...string) Principal {
	return Principal{TenantID: "acme", Subject: "dev", Restrictions: Restrictions{Agents: agents}}
}

func TestRunTree_RootAgentDecides(t *testing.T) {
	r := &treeReader{runs: map[string]RunLink{
		"root_a":  {Agent: "allowed"},
		"child_a": {Agent: "helper", ParentRunID: "root_a"},
		"root_o":  {Agent: "other"},
		"child_o": {Agent: "allowed", ParentRunID: "root_o"},
	}}
	tree := NewRunTree(restrictedTo("allowed"), true, r.read)
	for id, want := range map[string]bool{"root_a": true, "child_a": true, "root_o": false, "child_o": false, "missing": false} {
		if got := tree.Reaches(id); got != want {
			t.Errorf("Reaches(%s) = %v, want %v", id, got, want)
		}
	}
}

func TestRunTree_UnrestrictedReachesEverythingWithoutReading(t *testing.T) {
	r := &treeReader{}
	for name, tree := range map[string]*RunTree{
		"no principal":     NewRunTree(Principal{}, false, r.read),
		"no allowlist":     NewRunTree(Principal{TenantID: "acme"}, true, r.read),
		"no reader either": NewRunTree(Principal{TenantID: "acme"}, true, nil),
	} {
		if !tree.Reaches("anything") || !tree.ReachesKnown(RunLink{}) || !tree.ReachesSession("x", nil) {
			t.Errorf("%s: an answer was false", name)
		}
	}
	if r.reads != 0 {
		t.Errorf("read %d runs, want none", r.reads)
	}
}

func TestRunTree_FailsClosedOnLoopsDepthAndNoReader(t *testing.T) {
	loop := &treeReader{runs: map[string]RunLink{
		"a": {Agent: "allowed", ParentRunID: "b"},
		"b": {Agent: "allowed", ParentRunID: "a"},
	}}
	if NewRunTree(restrictedTo("allowed"), true, loop.read).Reaches("a") {
		t.Error("a parent loop reached")
	}
	deep := &treeReader{runs: map[string]RunLink{"r0": {Agent: "allowed"}}}
	for i := 1; i <= MaxAllowlistTreeHops+1; i++ {
		deep.runs[fmt.Sprintf("r%d", i)] = RunLink{Agent: "allowed", ParentRunID: fmt.Sprintf("r%d", i-1)}
	}
	tree := NewRunTree(restrictedTo("allowed"), true, deep.read)
	if !tree.Reaches(fmt.Sprintf("r%d", MaxAllowlistTreeHops)) {
		t.Error("a chain at the hop bound did not reach")
	}
	if tree.Reaches(fmt.Sprintf("r%d", MaxAllowlistTreeHops+1)) {
		t.Error("a chain past the hop bound reached")
	}
	if NewRunTree(restrictedTo("allowed"), true, nil).Reaches("r0") {
		t.Error("a tree with no reader reached")
	}
}

func TestRunTree_ReachesKnownStartsFromTheRowAndRemembersIt(t *testing.T) {
	r := &treeReader{runs: map[string]RunLink{"root": {Agent: "allowed"}}}
	tree := NewRunTree(restrictedTo("allowed"), true, r.read)
	if !tree.ReachesKnown(RunLink{ID: "top", Agent: "allowed"}) {
		t.Error("a top-level run of the allowed agent did not reach")
	}
	if r.reads != 0 {
		t.Errorf("a known top-level row cost %d reads, want 0", r.reads)
	}
	for i := 0; i < 3; i++ {
		if !tree.ReachesKnown(RunLink{ID: fmt.Sprintf("c%d", i), Agent: "helper", ParentRunID: "root"}) {
			t.Errorf("child %d of the allowed root did not reach", i)
		}
	}
	if r.reads != 1 {
		t.Errorf("three siblings read their shared root %d times, want once", r.reads)
	}
	if tree.ReachesKnown(RunLink{Agent: "allowed"}) {
		t.Error("a row with no id reached")
	}
}

func TestRunTree_SessionNeedsEveryRun(t *testing.T) {
	r := &treeReader{runs: map[string]RunLink{"root_a": {Agent: "allowed"}, "root_o": {Agent: "other"}}}
	tree := NewRunTree(restrictedTo("allowed"), true, r.read)
	runs := func(ls ...RunLink) func() ([]RunLink, error) {
		return func() ([]RunLink, error) { return ls, nil }
	}
	cases := []struct {
		name  string
		agent string
		runs  func() ([]RunLink, error)
		want  bool
	}{
		{"own agent", "allowed", runs(RunLink{ID: "s1", Agent: "allowed"}), true},
		{"other agent", "other", runs(RunLink{ID: "s2", Agent: "other"}), false},
		{"opened by a sub-run of the allowed agent", "helper", runs(RunLink{ID: "s3", Agent: "helper", ParentRunID: "root_a"}), true},
		{"allowed agent opened under another root", "allowed", runs(RunLink{ID: "s4", Agent: "allowed", ParentRunID: "root_o"}), false},
		{"one of its runs out of reach", "helper", runs(
			RunLink{ID: "s5", Agent: "helper", ParentRunID: "root_a"}, RunLink{ID: "s6", Agent: "helper"}), false},
		{"no runs, allowed agent", "allowed", runs(), true},
		{"no runs, other agent", "other", runs(), false},
		{"runs unreadable", "allowed", func() ([]RunLink, error) { return nil, errors.New("db down") }, false},
		{"no lister", "allowed", nil, false},
	}
	for _, c := range cases {
		if got := tree.ReachesSession(c.agent, c.runs); got != c.want {
			t.Errorf("%s: ReachesSession = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestRunTree_ReaderAnswersForTheAskedID(t *testing.T) {
	// A reader that returns a row filed under another id must not let that id
	// stand in for the one asked about in the memory.
	tree := NewRunTree(restrictedTo("allowed"), true, func(string) (RunLink, error) {
		return RunLink{ID: "decoy", Agent: "other"}, nil
	})
	if tree.Reaches("x") {
		t.Error("an other-agent run reached")
	}
	if tree.ReachesKnown(RunLink{ID: "decoy", Agent: "allowed"}) != true {
		t.Error("the caller's own row was overridden by the reader's mislabelled one")
	}
}
