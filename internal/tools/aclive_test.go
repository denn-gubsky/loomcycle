package tools

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// liveSource is a caller whose current set the test changes between calls;
// err, when set, is what the re-read fails with.
type liveSource struct {
	mu    sync.Mutex
	eff   *authz.Effective
	err   error
	reads atomic.Int32
}

func (s *liveSource) set(eff *authz.Effective, err error) {
	s.mu.Lock()
	s.eff, s.err = eff, err
	s.mu.Unlock()
}

func (s *liveSource) resolve(context.Context, authz.Subject) (*authz.Effective, error) {
	s.reads.Add(1)
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.eff, s.err
}

// ranTool records that it ran.
type ranTool struct {
	name string
	ran  *atomic.Int32
}

func (r ranTool) Name() string                 { return r.name }
func (r ranTool) Description() string          { return "" }
func (r ranTool) InputSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (r ranTool) Execute(context.Context, json.RawMessage) (Result, error) {
	r.ran.Add(1)
	return Result{Text: r.name + " ran"}, nil
}

// liveRun is a run's ctx whose policy holds Read and Bash, a volume bound
// read-write, user and tenant memory, with start as its caller's set and a
// live view reading src; events lands what the run reported.
func liveRun(t *testing.T, mode authz.Mode, start *authz.Effective, src *liveSource) (context.Context, *[]providers.Event) {
	t.Helper()
	src.set(start, nil)
	var mu sync.Mutex
	events := &[]providers.Event{}
	ctx := WithEventEmitter(modeCtx(mode), func(ev providers.Event) {
		mu.Lock()
		*events = append(*events, ev)
		mu.Unlock()
	})
	ctx = WithRunID(ctx, "run_live")
	return WithRunPolicy(ctx, RunPolicy{
		Tools:   []string{"Read", "Bash"},
		Volumes: VolumePolicyValue{Active: true, Bindings: []VolumeBinding{{Name: "scratch"}}},
		Memory:  MemoryPolicyValue{AllowedScopes: []string{"user", "tenant"}},
		ACL:     start,
		LiveACL: NewLiveACL(start, src.resolve),
	}), events
}

func specNames(specs []providers.ToolSpec) []string {
	var out []string
	for _, s := range specs {
		out = append(out, s.Name)
	}
	return out
}

// liveSet is a compiled set with rules, plus one that names nothing these
// tests ask about (a list needs a rule).
func liveSet(t *testing.T, rules ...authz.Rule) *authz.Effective {
	t.Helper()
	return compiledSet(t, append([]authz.Rule{{Effect: authz.Allow, Actions: []string{"read"}, Resources: []string{"volume:unrelated"}}}, rules...)...)
}

var readBashSpecs = []providers.ToolSpec{{Name: "Read"}, {Name: "Bash"}}

// The core behaviour: a deny bound while the run is in flight withholds the
// tool from the next model call's schema, refuses a call to it (the model
// may name it from an earlier turn) before it runs, narrows the volume and
// the keyspace it names, and is reported once on the run's transcript.
// Read, which nothing names, still runs.
func TestLiveACL_ADenyBoundMidRunWithdrawsOnTheNextCall(t *testing.T) {
	start := liveSet(t)
	src := &liveSource{}
	ctx, events := liveRun(t, authz.ModeCompat, start, src)
	var readRan, bashRan atomic.Int32
	d := NewDispatcher([]Tool{ranTool{"Read", &readRan}, ranTool{"Bash", &bashRan}})

	if r := d.Execute(ctx, "Bash", json.RawMessage(`{}`)); r.IsError || bashRan.Load() != 1 {
		t.Fatalf("before the change Bash should run: %+v", r)
	}
	src.set(liveSet(t,
		authz.Rule{Effect: authz.Deny, Actions: []string{"invoke"}, Resources: []string{"tool:Bash"}},
		authz.Rule{Effect: authz.Deny, Actions: []string{"write"}, Resources: []string{"volume:scratch"}},
		authz.Rule{Effect: authz.Deny, Actions: []string{"read"}, Resources: []string{"memory:tenant"}},
		authz.Rule{Effect: authz.Deny, Actions: []string{"write"}, Resources: []string{"memory:user"}}), nil)

	if got := specNames(LiveToolSpecs(ctx, readBashSpecs)); !reflect.DeepEqual(got, []string{"Read"}) {
		t.Errorf("next schema = %v, want [Read]", got)
	}
	r := d.Execute(ctx, "Bash", json.RawMessage(`{}`))
	if !r.IsError || refusalCode(t, r) != "acl_denied" || bashRan.Load() != 1 {
		t.Errorf("a call to Bash after the deny should be refused acl_denied before it runs: %+v (ran %d)", r, bashRan.Load())
	}
	if r := d.Execute(ctx, "Read", json.RawMessage(`{}`)); r.IsError || readRan.Load() != 1 {
		t.Errorf("Read should still run: %+v", r)
	}
	p := PolicyOf(ctx)
	if !reflect.DeepEqual(p.Tools, []string{"Read"}) || !reflect.DeepEqual(p.ACLRemovedTools, []string{"Bash"}) {
		t.Errorf("policy tools %v removed %v", p.Tools, p.ACLRemovedTools)
	}
	if len(p.Volumes.Bindings) != 1 || !p.Volumes.Bindings[0].ReadOnly {
		t.Errorf("scratch should be read-only now: %+v", p.Volumes)
	}
	if !reflect.DeepEqual(p.Memory.AllowedScopes, []string{"user"}) || !reflect.DeepEqual(p.Memory.ReadOnlyScopes, []string{"user"}) {
		t.Errorf("memory scopes %v read-only %v, want [user] read-only", p.Memory.AllowedScopes, p.Memory.ReadOnlyScopes)
	}
	var bashInert, otherInert int
	for _, ev := range *events {
		if ev.Type != providers.EventCapabilityInert || ev.CapabilityInert.Gate != "acl" {
			continue
		}
		if ev.CapabilityInert.Tool == "Bash" {
			bashInert++
		} else if strings.Contains(ev.Text, "volume scratch read-only") && strings.Contains(ev.Text, "memory scope tenant dropped") && strings.Contains(ev.Text, "memory scope user read-only") {
			otherInert++
		}
	}
	if bashInert != 1 || otherInert != 1 {
		t.Errorf("want one acl inert event for Bash and one for the volume+scope, got %d and %d: %+v", bashInert, otherInert, *events)
	}
}

// Narrow-only: a run never regains what its start set withheld — a deny
// lifted mid-run leaves Bash out of the policy and refused per call, and
// every per-call question still sees the start set's deny.
func TestLiveACL_LiftingADenyMidRunDoesNotWidenPastTheStart(t *testing.T) {
	denyBash := authz.Rule{Effect: authz.Deny, Actions: []string{"invoke"}, Resources: []string{"tool:Bash"}}
	start := liveSet(t, denyBash)
	src := &liveSource{}
	ctx, _ := liveRun(t, authz.ModeCompat, start, src)
	// The run started without Bash: the start narrowing took it.
	ctx = UpdateRunPolicy(ctx, func(p *RunPolicy) { p.Tools = []string{"Read"} })
	src.set(liveSet(t), nil) // the deny binding is gone

	if got := specNames(LiveToolSpecs(ctx, []providers.ToolSpec{{Name: "Read"}})); !reflect.DeepEqual(got, []string{"Read"}) {
		t.Errorf("schema = %v", got)
	}
	p := PolicyOf(ctx)
	if !reflect.DeepEqual(p.Tools, []string{"Read"}) {
		t.Errorf("tools %v, want [Read]", p.Tools)
	}
	if aclPermits(ctx, p.ACL, "invoke", authz.ResourceForTool("Bash")) {
		t.Error("the live set allows Bash, which the run started without")
	}
	if p.ACL.Version != src.eff.Version || p.ACL == start {
		t.Error("the live set should be the current reading held to the start")
	}
}

// A binding restored after a mid-run removal gives back what it took — up to
// the start set — and the schema follows.
func TestLiveACL_ARestoredBindingGivesBackWithinTheStart(t *testing.T) {
	start := liveSet(t)
	src := &liveSource{}
	ctx, _ := liveRun(t, authz.ModeCompat, start, src)
	src.set(liveSet(t, authz.Rule{Effect: authz.Deny, Actions: []string{"invoke"}, Resources: []string{"tool:Bash"}}), nil)
	if got := specNames(LiveToolSpecs(ctx, readBashSpecs)); !reflect.DeepEqual(got, []string{"Read"}) {
		t.Fatalf("after the deny: %v", got)
	}
	src.set(liveSet(t), nil)
	if got := specNames(LiveToolSpecs(ctx, readBashSpecs)); !reflect.DeepEqual(got, []string{"Read", "Bash"}) {
		t.Errorf("after the deny was lifted: %v, want both back", got)
	}
}

// Enforce mode: an allow binding removed mid-run leaves the tool with no rule
// that allows it, which enforce refuses.
func TestLiveACL_EnforceRemovingTheAllowWithdrawsTheTool(t *testing.T) {
	allowBoth := authz.Rule{Effect: authz.Allow, Actions: []string{"invoke"}, Resources: []string{"tool:Read", "tool:Bash"}}
	start := liveSet(t, allowBoth)
	src := &liveSource{}
	ctx, _ := liveRun(t, authz.ModeEnforce, start, src)
	src.set(liveSet(t, authz.Rule{Effect: authz.Allow, Actions: []string{"invoke"}, Resources: []string{"tool:Read"}}), nil)
	if got := specNames(LiveToolSpecs(ctx, readBashSpecs)); !reflect.DeepEqual(got, []string{"Read"}) {
		t.Errorf("enforce schema = %v, want [Read]", got)
	}
	var ran atomic.Int32
	r := NewDispatcher([]Tool{ranTool{"Bash", &ran}}).Execute(ctx, "Bash", json.RawMessage(`{}`))
	if !r.IsError || refusalCode(t, r) != "acl_denied" || ran.Load() != 0 {
		t.Errorf("enforce: Bash should be refused: %+v", r)
	}
}

// A re-read that fails: compat and shadow keep the last reading (the call
// runs, nothing is withheld); enforce refuses every tool call with
// acl_unavailable, retryable, until a re-read succeeds — and then runs again.
func TestLiveACL_AFailedReReadFailsClosedOnlyInEnforce(t *testing.T) {
	for _, mode := range []authz.Mode{authz.ModeCompat, authz.ModeShadow, authz.ModeEnforce} {
		t.Run(string(mode), func(t *testing.T) {
			start := liveSet(t, authz.Rule{Effect: authz.Allow, Actions: []string{"invoke"}, Resources: []string{"tool:*"}})
			src := &liveSource{}
			ctx, _ := liveRun(t, mode, start, src)
			var ran atomic.Int32
			d := NewDispatcher([]Tool{ranTool{"Read", &ran}})
			src.set(nil, errors.New("store down"))
			r := d.Execute(ctx, "Read", json.RawMessage(`{}`))
			if mode == authz.ModeEnforce {
				if !r.IsError || refusalCode(t, r) != "acl_unavailable" || r.Error == nil || !r.Error.Retryable || ran.Load() != 0 {
					t.Fatalf("enforce: want a retryable acl_unavailable refusal, got %+v", r)
				}
			} else if r.IsError || ran.Load() != 1 {
				t.Fatalf("%s: the last reading should stand and the call run: %+v", mode, r)
			}
			if got := specNames(LiveToolSpecs(ctx, readBashSpecs)); len(got) != 2 {
				t.Errorf("a failed re-read must not change the schema: %v", got)
			}
			src.set(start, nil)
			if r := d.Execute(ctx, "Read", json.RawMessage(`{}`)); r.IsError {
				t.Errorf("after the re-read recovers the call should run: %+v", r)
			}
		})
	}
}

// With no live view (LOOMCYCLE_ACL_LIVE off) nothing re-reads: the policy,
// the schema and the dispatch are exactly the frozen start's.
func TestLiveACL_OffLeavesTheStartSetFrozen(t *testing.T) {
	start := liveSet(t)
	src := &liveSource{}
	ctx, events := liveRun(t, authz.ModeCompat, start, src)
	ctx = UpdateRunPolicy(ctx, func(p *RunPolicy) { p.LiveACL = nil })
	src.set(liveSet(t, authz.Rule{Effect: authz.Deny, Actions: []string{"invoke"}, Resources: []string{"tool:Bash"}}), nil)
	before := rawPolicy(ctx)
	if got := LiveToolSpecs(ctx, readBashSpecs); len(got) != 2 {
		t.Errorf("schema changed with no live view: %v", specNames(got))
	}
	if !reflect.DeepEqual(PolicyOf(ctx), before) {
		t.Error("PolicyOf changed with no live view")
	}
	var ran atomic.Int32
	if r := NewDispatcher([]Tool{ranTool{"Bash", &ran}}).Execute(ctx, "Bash", json.RawMessage(`{}`)); r.IsError || ran.Load() != 1 {
		t.Errorf("Bash should run on the frozen set: %+v", r)
	}
	if src.reads.Load() != 0 || len(*events) != 0 {
		t.Errorf("no live view must read nothing and report nothing: %d reads, %d events", src.reads.Load(), len(*events))
	}
	if NewLiveACL(nil, src.resolve) != nil {
		t.Error("a run with no set needs no live view")
	}
}

// A child spawned from a run follows the caller's live set: its own view,
// built from the set its parent held, narrows its own tools on the change,
// and reports on its own transcript.
func TestLiveACL_AChildFollowsTheCallersLiveSet(t *testing.T) {
	start := liveSet(t)
	src := &liveSource{}
	parentCtx, _ := liveRun(t, authz.ModeCompat, start, src)
	parent := PolicyOf(parentCtx)
	own := RunPolicy{Tools: []string{"Bash"}, ACL: parent.ACL, LiveACL: NewLiveACL(parent.ACL, src.resolve)}
	child := parent.Inherit(ChildGrant{Own: own})
	if child.LiveACL == nil || child.LiveACL == parent.LiveACL {
		t.Fatal("a child should carry its own live view")
	}
	var childEvents []providers.Event
	childCtx := WithEventEmitter(WithRunPolicy(modeCtx(authz.ModeCompat), child), func(ev providers.Event) { childEvents = append(childEvents, ev) })
	src.set(liveSet(t, authz.Rule{Effect: authz.Deny, Actions: []string{"invoke"}, Resources: []string{"tool:Bash"}}), nil)
	if got := LiveToolSpecs(childCtx, []providers.ToolSpec{{Name: "Bash"}}); len(got) != 0 {
		t.Errorf("the child's schema kept Bash: %v", specNames(got))
	}
	if len(childEvents) != 1 || childEvents[0].CapabilityInert == nil || childEvents[0].CapabilityInert.Tool != "Bash" {
		t.Errorf("the child should report Bash withheld on its own transcript: %+v", childEvents)
	}
}

// An unchanged set (the resolver's cached pointer) changes nothing and
// reports nothing, however often it is read.
func TestLiveACL_AnUnchangedSetIsANoOp(t *testing.T) {
	start := liveSet(t)
	src := &liveSource{}
	ctx, events := liveRun(t, authz.ModeCompat, start, src)
	raw := rawPolicy(ctx)
	for i := 0; i < 5; i++ {
		RefreshLiveACL(ctx)
	}
	if !reflect.DeepEqual(PolicyOf(ctx), raw) || len(*events) != 0 || src.reads.Load() != 5 {
		t.Errorf("unchanged set: policy changed=%v events=%d reads=%d", !reflect.DeepEqual(PolicyOf(ctx), raw), len(*events), src.reads.Load())
	}
}
