package http

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// What a spawned child keeps of its parent's run is decided by
// tools.RunPolicy.Inherit. These are the hazards the RFC ET P4 census found
// where the spawn path used to leave a parent's value on the child's ctx.

func serverAgentTool(t *testing.T, s *Server) *builtin.AgentTool {
	t.Helper()
	for _, tl := range s.tools {
		if a, ok := tl.(*builtin.AgentTool); ok {
			return a
		}
	}
	t.Fatal("no AgentTool on the server")
	return nil
}

// H4: a parent run's fan-out cap rode the child's ctx unchanged, and the cap
// lookup prefers ctx — so a child whose own definition allows 2 concurrent
// children fanned out 8 wide under a parent allowed 8.
func TestPrepareSubRun_ChildFansOutNoWiderThanItsOwnDefinition(t *testing.T) {
	h := newReviewHarness(t)
	h.srv.cfg().Agents["child"] = config.AgentDef{Model: "stub-model", SystemPrompt: "c", Tools: []string{"Agent"}, MaxConcurrentChildren: 2}
	parent := tools.UpdateRunPolicy(tools.WithRunID(context.Background(), "r_parent"), func(rp *tools.RunPolicy) { rp.FanoutCap = 8 })
	prep, err := h.srv.prepareSubRunValues(parent, "child", nameFromAgentTool, "", "go", "", false, func(providers.Event) {}, nil, nil, false, false)
	if err != nil {
		t.Fatal(err)
	}
	defer prep.cleanup()
	if got := serverAgentTool(t, h.srv).CapLookup(prep.LoopCtx, "child"); got != 2 {
		t.Errorf("the child's parallel_spawn cap is %d; its own definition says 2 (its parent's run allowed 8)", got)
	}
}

// H6: a spawn's compaction override stayed on the child's ctx, so every
// descendant the child spawned without one of its own received it — and a
// grandchild that runs on its definition only was refused an override nobody
// passed it.
func TestPrepareSubRun_SpawnCompactionOverrideIsConsumedByTheChild(t *testing.T) {
	h := newReviewHarness(t)
	cfg := h.srv.cfg()
	locked := false
	cfg.Agents["child"] = config.AgentDef{Model: "stub-model", SystemPrompt: "c", Tools: []string{"Agent"}}
	cfg.Agents["locked"] = config.AgentDef{Model: "stub-model", SystemPrompt: "l", AllowOverrides: &locked}
	keep := 3
	parent := tools.UpdateRunPolicy(tools.WithRunID(context.Background(), "r_parent"), func(rp *tools.RunPolicy) { rp.CompactionOverride = &config.Compaction{KeepLastN: &keep} })
	prep, err := h.srv.prepareSubRunValues(parent, "child", nameFromAgentTool, "", "go", "", false, func(providers.Event) {}, nil, nil, false, false)
	if err != nil {
		t.Fatal(err)
	}
	defer prep.cleanup()
	if c := tools.PolicyOf(prep.LoopCtx).CompactionOverride; c != nil {
		t.Errorf("the child's ctx still carries its own spawn's compaction override %+v; its spawns would pass it on", *c)
	}
	// The override is the child's: it runs with it.
	if c := prep.Opts.Compaction; c == nil || c.KeepLastN == nil || *c.KeepLastN != 3 {
		t.Errorf("the child does not run with its spawn's compaction override: %+v", c)
	}
	// The consequence: the child spawns a locked agent with no override at all.
	grand, err := h.srv.prepareSubRunValues(prep.LoopCtx, "locked", nameFromAgentTool, "", "go", "", false, func(providers.Event) {}, nil, nil, false, false)
	if err != nil {
		t.Errorf("a grandchild spawned with no override was refused: %v", err)
	} else {
		grand.cleanup()
	}
}

// H8: a parallel_spawn child's ledger index and timeout rode its ctx into its
// own spawns; with the spawn ledger on, a SEQUENTIAL spawn the child made wrote
// a spawn_child_started row naming the child's own index.
func TestPrepareSubRun_ParallelSpawnIndexStopsAtTheChild(t *testing.T) {
	h := newReviewHarness(t)
	h.srv.cfg().Env.ResumeFanout = true
	h.srv.cfg().Agents["child"] = config.AgentDef{Model: "stub-model", SystemPrompt: "c", Tools: []string{"Agent"}}
	parent := tools.UpdateRunPolicy(tools.UpdateRunPolicy(tools.WithRunID(context.Background(), "r_parent"), func(rp *tools.RunPolicy) { rp.SpawnIndex = 3 + 1 }), func(rp *tools.RunPolicy) { rp.SpawnTimeoutMs = 500 })
	prep, err := h.srv.prepareSubRunValues(parent, "child", nameFromAgentTool, "", "go", "", false, func(providers.Event) {}, nil, nil, false, false)
	if err != nil {
		t.Fatal(err)
	}
	defer prep.cleanup()
	if idx, ok := tools.PolicyOf(prep.LoopCtx).SpawnChildIndex(); ok {
		t.Errorf("the child's ctx carries its own parallel_spawn index %d", idx)
	}
	if ms := tools.PolicyOf(prep.LoopCtx).SpawnTimeoutMs; ms != 0 {
		t.Errorf("the child's ctx carries its own spawn timeout %dms", ms)
	}
	// The consequence: the child makes a plain (sequential) Agent call.
	var mu sync.Mutex
	var ledger []string
	childCall := tools.WithToolUseID(prep.LoopCtx, "tu_child_seq")
	childCall = tools.WithEventEmitter(childCall, func(ev providers.Event) {
		if ev.Type == providers.EventSpawnChildStarted {
			mu.Lock()
			ledger = append(ledger, fmt.Sprintf("%+v", *ev.SpawnChild))
			mu.Unlock()
		}
	})
	grand, err := h.srv.prepareSubRunValues(childCall, "writer", nameFromAgentTool, "", "go", "", false, func(providers.Event) {}, nil, nil, false, false)
	if err != nil {
		t.Fatal(err)
	}
	grand.cleanup()
	mu.Lock()
	defer mu.Unlock()
	if len(ledger) != 0 {
		t.Errorf("a sequential spawn wrote a parallel_spawn ledger row on the child's transcript: %s", strings.Join(ledger, "; "))
	}
}

// H3: a locked child applies its parent's host list as a narrowing — its record
// and its own tools said so — but its ctx kept the parent's list as it was, so
// its own children re-derived from the un-narrowed list (and a resume of the
// child, which reads the record, disagreed with the live tree).
func TestPrepareSubRun_LockedChildKeepsTheHostListNarrowOnly(t *testing.T) {
	h := newReviewHarness(t)
	locked := false
	h.srv.cfg().Agents["child"] = config.AgentDef{Model: "stub-model", SystemPrompt: "c", Tools: []string{"Agent"}, AllowOverrides: &locked}
	parent := tools.UpdateRunPolicy(tools.WithRunID(context.Background(), "r_parent"), func(rp *tools.RunPolicy) {
		rp.Hosts = tools.HostPolicyValue{AllowedHosts: []string{"api.example.com"}, HasList: true}
	})
	prep, err := h.srv.prepareSubRunValues(parent, "child", nameFromAgentTool, "", "go", "", false, func(providers.Event) {}, nil, nil, false, false)
	if err != nil {
		t.Fatal(err)
	}
	defer prep.cleanup()
	if hp := tools.PolicyOf(prep.LoopCtx).Hosts; !hp.NarrowOnly {
		t.Errorf("the locked child's ctx carries its parent's host list as a replacing one (%+v); its own children would widen past it", hp)
	}
}

// A sub-run resumed after a pause runs with the policy it had live: the record
// its spawn wrote carries what it inherited, and resume rebuilds the policy
// from that record and the definition. Checked on a locked child of a parent
// with a host list, a fan-out cap, key scopes, read-only ephemeral volumes and
// an interruption narrowing it inherits as the same definition.
func TestResume_SubRunPolicyMatchesItsLiveSpawn(t *testing.T) {
	h := newReviewHarness(t)
	locked := false
	target := 20
	def := config.AgentDef{
		Model: "stub-model", SystemPrompt: "c", Tools: []string{"Agent", "Interruption"},
		AllowOverrides:        &locked,
		MaxConcurrentChildren: 2,
		Interruption:          config.AgentInterruptionACL{Enabled: true, Kinds: []string{"question"}, MaxPending: 5},
		CryptoScopes:          []string{"run", "user"},
		Compaction:            &config.Compaction{TargetPercentage: &target},
	}
	h.srv.cfg().Agents["child"] = def
	keep := 3
	parent := tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{UserID: "u1", AgentID: "a_parent", RootRunID: "r_parent"})
	parent = tools.WithRunID(parent, "r_parent")
	parent = tools.WithRunPolicy(parent, tools.RunPolicy{
		Hosts:             tools.HostPolicyValue{AllowedHosts: []string{"api.example.com"}, HasList: true},
		FanoutCap:         8,
		Crypto:            tools.CryptoPolicyValue{Scopes: []string{"run"}},
		EphemeralWritable: false, // read-only, which the child keeps
		Compaction:        &config.Compaction{KeepLastN: &keep},
		// The parent is the same definition, so its interruption narrowing
		// reaches the child, locked or not.
		Overrides: tools.RunOverridesValue{Record: []byte(`{"interruption":{"enabled":true,"max_pending":1}}`), AgentName: "child"},
	})
	prep, err := h.srv.prepareSubRunValues(parent, "child", nameFromAgentTool, "", "go", "", false, func(providers.Event) {}, nil, nil, false, false)
	if err != nil {
		t.Fatal(err)
	}
	defer prep.cleanup()
	live := tools.PolicyOf(prep.LoopCtx)

	run, err := h.st.GetRun(context.Background(), prep.RunID)
	if err != nil {
		t.Fatal(err)
	}
	runCfg, ok := decodeRunConfig(run.RunConfig)
	if !ok {
		t.Fatal("the sub-run wrote no configuration record")
	}
	resumeCtx := tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{UserID: run.UserID, TenantID: run.TenantID, Isolated: run.Isolated})
	// Resolved by name, as resume resolves it.
	resolved, _, err := h.srv.resolveAgentName(resumeCtx, run.TenantID, run.Agent, nameFromAgentTool)
	if err != nil {
		t.Fatal(err)
	}
	src := h.srv.resumedPolicySource(resumeCtx, run, runCfg, resolved)
	// Resume re-derives these from the definition, as the spawn did.
	src.Tools = prep.Opts.Tools
	src.CoreBlocks = live.CoreBlocks.Blocks
	resumed, _ := h.srv.agentRunPolicy(resumeCtx, resolved, run.Agent, src)

	// The values the fixture is built to exercise, so a fixture that stops
	// exercising them fails rather than comparing two zeros.
	if !live.Hosts.NarrowOnly || live.FanoutCap != 2 || live.Interruption.Load().MaxPending != 1 ||
		!slices.Equal(live.Crypto.Scopes, []string{"run"}) || live.EphemeralWritable ||
		live.Compaction == nil || live.Compaction.KeepLastN != nil {
		t.Fatalf("fixture: the live child's policy is not the one this test is built around: %+v", live)
	}
	if got, want := resumed.Interruption.Load(), live.Interruption.Load(); !reflect.DeepEqual(got, want) {
		t.Errorf("interruption: resumed %+v, live %+v", got, want)
	}
	// Overrides is the one field that differs, and on purpose for now: live, a
	// child keeps its PARENT's offer on ctx (its own children of the parent's
	// definition take the parent's overrides); a resumed child offers its own
	// record. Census hazard H5, left as it was by this change.
	for _, p := range []*tools.RunPolicy{&live, &resumed} {
		p.Interruption, p.Overrides = nil, tools.RunOverridesValue{}
	}
	if !reflect.DeepEqual(resumed, live) {
		t.Errorf("the resumed sub-run's policy differs from the one it ran with live:\n resumed %+v\n live    %+v", resumed, live)
	}
}

// A child under a parent with no run cap of its own fans out at its OWN
// definition's width, the one it was spawned with. The cap used to be dropped
// to 0 there, which sends the lookup to whatever the agent's NAME resolves to
// at spawn time: a version promoted since, or one without the pinned
// version's cap.
func TestPrepareSubRun_ChildKeepsItsOwnFanoutCapUnderAnUncappedParent(t *testing.T) {
	h := newReviewHarness(t)
	cfg := h.srv.cfg()
	cfg.Agents["child"] = config.AgentDef{Model: "stub-model", SystemPrompt: "c", Tools: []string{"Agent"}, MaxConcurrentChildren: 2}
	prep, err := h.srv.prepareSubRunValues(tools.WithRunID(context.Background(), "r_parent"), "child", nameFromAgentTool, "", "go", "", false, func(providers.Event) {}, nil, nil, false, false)
	if err != nil {
		t.Fatal(err)
	}
	defer prep.cleanup()
	// The name now resolves to a definition with no cap of its own.
	cfg.Agents["child"] = config.AgentDef{Model: "stub-model", SystemPrompt: "c", Tools: []string{"Agent"}}
	if got := serverAgentTool(t, h.srv).CapLookup(prep.LoopCtx, "child"); got != 2 {
		t.Errorf("the child's parallel_spawn cap is %d; it was spawned under a definition allowing 2", got)
	}
}

// The same on resume: a sub-run whose record says its ancestors set no cap
// resumes at its definition's width, not at whatever its name resolves to.
func TestResume_SubRunKeepsItsOwnFanoutCapUnderAnUncappedParent(t *testing.T) {
	h := newReviewHarness(t)
	run := store.Run{ID: "r_child", Agent: "child", ParentRunID: "r_parent", UserID: "u1"}
	rec := runConfigRecord{Spawn: &spawnRecord{}} // FanoutCap 0: the parent had none
	def := config.AgentDef{Model: "stub-model", MaxConcurrentChildren: 2}
	ctx := tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{UserID: "u1"})
	if got := h.srv.resumedPolicySource(ctx, run, rec, def).FanoutCap; got != 2 {
		t.Errorf("the resumed sub-run's fan-out cap is %d; its definition allows 2", got)
	}
}
