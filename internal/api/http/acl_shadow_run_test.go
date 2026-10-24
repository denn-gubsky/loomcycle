package http

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/audit"
	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// Shadow mode inside a run: the run keeps every grant its definition gives
// (shadow decides nothing), and each narrowing enforce would apply at the
// run's start — a tool withheld, a volume made read-only or dropped, a
// keyspace dropped — and each spawn enforce would refuse, is recorded as an
// acl_shadow_diverged entry naming the run. These use aclResumeFixture's
// member dev of tenant acme and its list: deny invoke tool:Bash, write
// volume:scratch, read memory:tenant and spawn agent:forbidden.

// lockedSink is an audit sink the run goroutines may record into while a
// test reads it.
type lockedSink struct {
	mu     sync.Mutex
	events []audit.Event
}

func (s *lockedSink) Record(ev audit.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, ev)
	return nil
}

// diverged returns the acl_shadow_diverged entries for action on resource.
func (s *lockedSink) diverged(action, resource string) []audit.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []audit.Event
	for _, ev := range s.events {
		if ev.Action == "acl_shadow_diverged" && ev.AclAction == action && ev.TargetName == resource {
			out = append(out, ev)
		}
	}
	return out
}

func (s *lockedSink) allDiverged() []audit.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []audit.Event
	for _, ev := range s.events {
		if ev.Action == "acl_shadow_diverged" {
			out = append(out, ev)
		}
	}
	return out
}

func shadowRunEnv(t *testing.T, mode authz.Mode, bind bool, scripts map[string][][]providers.Event) (*aclResumeEnv, *lockedSink) {
	t.Helper()
	e := aclResumeFixtureIn(t, mode, bind, scripts)
	sink := &lockedSink{}
	e.srv.SetTokenAudit(sink)
	return e, sink
}

// assertOneDivergence fails unless exactly one entry records action on
// resource, in runID, for dev of acme, as a deny by dev's list.
func assertOneDivergence(t *testing.T, sink *lockedSink, action, resource, runID string) {
	t.Helper()
	evs := sink.diverged(action, resource)
	if len(evs) != 1 {
		t.Errorf("%s %s: %d acl_shadow_diverged entries, want 1: %+v", action, resource, len(evs), evs)
		return
	}
	ev := evs[0]
	if ev.AclRunID != runID || ev.ActorTenant != "acme" || ev.ActorSubject != "dev" || ev.AclOutcome != "deny" ||
		len(ev.AclRules) == 0 || !strings.HasPrefix(ev.AclRules[0], "dev-limits#") {
		t.Errorf("%s %s: entry %+v, want run %s, acme/dev, a deny by dev-limits", action, resource, ev, runID)
	}
}

func TestACLShadow_RunKeepsItsGrantsAndRecordsEachInRunNarrowing(t *testing.T) {
	e, sink := shadowRunEnv(t, authz.ModeShadow, true, map[string][][]providers.Event{
		"worker": {probeTurn("tu_live"), endTurn()},
	})
	run := e.startAs(t, "worker", "a_shadow")
	p := e.waitProbes(t, 1)[0]

	n := aclNarrowingOf(p)
	if !slices.Contains(n.Tools, "Bash") || len(n.Removed) != 0 || len(n.Volumes) != 1 || n.Volumes[0].ReadOnly || !slices.Contains(n.Memory, "tenant") {
		t.Errorf("shadow narrowed the run: %+v", n)
	}
	if offered := e.prov.toolsOf("worker"); len(offered) == 0 || !slices.Contains(offered[0], "Bash") {
		t.Errorf("the model was not offered Bash in shadow mode: %v", offered)
	}
	if hasEvent(runEvents(t, e.st, "a_shadow"), "capability_inert", `"gate":"acl"`) {
		t.Error("shadow mode reported a withheld tool to the run")
	}
	assertOneDivergence(t, sink, "invoke", "tool:Bash", run.ID)
	assertOneDivergence(t, sink, "write", "volume:scratch", run.ID)
	assertOneDivergence(t, sink, "read", "memory:tenant", run.ID)
}

func TestACLShadow_DeniedSpawnStillStartsTheChildAndIsRecorded(t *testing.T) {
	e, sink := shadowRunEnv(t, authz.ModeShadow, true, map[string][][]providers.Event{
		"worker": {toolCallTurn("tu_spawn", "Agent", `{"name":"forbidden","prompt":"x"}`), endTurn()},
	})
	parent := e.startAs(t, "worker", "a_spawner")
	kids, err := e.st.ListRunsByParentAgentID(context.Background(), "a_spawner")
	if err != nil || len(kids) != 1 || kids[0].Agent != "forbidden" {
		t.Fatalf("shadow mode should start the child: %+v, %v", kids, err)
	}
	if e.prov.callsOf("forbidden") == 0 {
		t.Error("the spawned child never called its model")
	}
	assertOneDivergence(t, sink, "spawn", "agent:forbidden", parent.ID)
}

// The rules a sub-agent is narrowed by are its parent's, unchanged: what the
// parent's start already recorded is not recorded again for the child.
func TestACLShadow_ASubAgentDoesNotRecordItsParentsDivergenceAgain(t *testing.T) {
	e, sink := shadowRunEnv(t, authz.ModeShadow, true, map[string][][]providers.Event{
		"worker": {toolCallTurn("tu_child", "Agent", `{"name":"child","prompt":"work"}`), endTurn()},
		"child":  {probeTurn("tu_probe"), endTurn()},
	})
	parent := e.startAs(t, "worker", "a_tree")
	if p := aclNarrowingOf(e.waitProbes(t, 1)[0]); !slices.Contains(p.Tools, "Bash") || p.Volumes[0].ReadOnly {
		t.Errorf("shadow narrowed the child: %+v", p)
	}
	assertOneDivergence(t, sink, "invoke", "tool:Bash", parent.ID)
	assertOneDivergence(t, sink, "write", "volume:scratch", parent.ID)
}

// Guards: compat and enforce narrow the run as before and record no
// divergence; shadow with rules that allow everything the run holds records
// none either.
func TestACLShadow_CompatAndEnforceNarrowAndRecordNoDivergence(t *testing.T) {
	for _, mode := range []authz.Mode{authz.ModeCompat, authz.ModeEnforce} {
		t.Run(string(mode), func(t *testing.T) {
			e, sink := shadowRunEnv(t, mode, true, map[string][][]providers.Event{
				"worker": {probeTurn("tu_live"), endTurn()},
			})
			e.startAs(t, "worker", "a_"+string(mode))
			n := aclNarrowingOf(e.waitProbes(t, 1)[0])
			if slices.Contains(n.Tools, "Bash") || !slices.Equal(n.Removed, []string{"Bash"}) || len(n.Volumes) != 1 || !n.Volumes[0].ReadOnly || slices.Contains(n.Memory, "tenant") {
				t.Errorf("%s did not narrow the run: %+v", mode, n)
			}
			if d := sink.allDiverged(); len(d) != 0 {
				t.Errorf("%s recorded divergences: %+v", mode, d)
			}
		})
	}
}

func TestACLShadow_NoDivergenceWhenTheRulesAllow(t *testing.T) {
	e, sink := shadowRunEnv(t, authz.ModeShadow, false, map[string][][]providers.Event{
		"worker": {toolCallTurn("tu_child", "Agent", `{"name":"child","prompt":"work"}`), endTurn()},
		"child":  {probeTurn("tu_probe"), endTurn()},
	})
	e.startAs(t, "worker", "a_allowed")
	e.waitProbes(t, 1)
	if d := sink.allDiverged(); len(d) != 0 {
		t.Errorf("a caller whose rules allow the run recorded divergences: %+v", d)
	}
}

// A resumed run is narrowed again by its caller's rules as they stand then,
// so shadow mode records what enforce would take from it again, under the
// same run.
func TestACLShadow_AResumedRunRecordsItsNarrowingAgain(t *testing.T) {
	e, sink := shadowRunEnv(t, authz.ModeShadow, true, map[string][][]providers.Event{
		"worker": {probeTurn("tu_live"), endTurn(), probeTurn("tu_resumed"), endTurn()},
	})
	run := e.startAs(t, "worker", "a_shadow_resume")
	parkForResume(t, e.srv, run.ID)
	resumeOne(t, e.srv)
	seen := e.waitProbes(t, 2)
	if n := aclNarrowingOf(seen[1]); !slices.Contains(n.Tools, "Bash") || n.Volumes[0].ReadOnly {
		t.Errorf("shadow narrowed the resumed run: %+v", n)
	}
	evs := sink.diverged("invoke", "tool:Bash")
	if len(evs) != 2 || evs[0].AclRunID != run.ID || evs[1].AclRunID != run.ID {
		t.Errorf("want the live start and the resume each recorded against %s: %+v", run.ID, evs)
	}
}
