package http

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/aclres"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/authz"
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
// with a host list, a fan-out cap, key scopes, shell credentials, a Git
// network grant, read-only ephemeral volumes and an interruption narrowing it
// inherits as the same definition — and the parent's offer, which the child
// passes on to its own children (H5).
func TestResume_SubRunPolicyMatchesItsLiveSpawn(t *testing.T) {
	h := newReviewHarness(t)
	locked := false
	target := 20
	// The child declares a shell credential its parent withholds; live it
	// gets only the one both allow, and a resume must not hand the other back.
	// Likewise it may push by its definition while its parent may only fetch:
	// live it only fetches, and a resume must not hand push back.
	childShellCreds := config.CredentialNames{"DEPLOY_KEY", "NPM_TOKEN"}
	recallOn := true
	def := config.AgentDef{
		Model: "stub-model", SystemPrompt: "c", Tools: []string{"Agent", "Interruption"},
		AllowOverrides:        &locked,
		MaxConcurrentChildren: 2,
		Interruption:          config.AgentInterruptionACL{Enabled: true, Kinds: []string{"question"}, MaxPending: 5},
		CryptoScopes:          []string{"run", "user"},
		ShellCredentials:      &childShellCreds,
		GitRemote:             []string{"fetch", "push"},
		Compaction:            &config.Compaction{TargetPercentage: &target},
		// Recall is granted by the setting, not listed in tools, and the
		// caller's rules deny it: live and resumed must both withhold it.
		Context: &config.Context{Recall: &recallOn},
	}
	h.srv.SetEmbedder(&fakeEmbedder{provider: "openai", model: "text-embedding-3-small", dim: 4})
	h.srv.tools = append(h.srv.tools, namedStubTool{"Recall"})
	h.srv.cfg().Agents["child"] = def
	keep := 3
	// RFC EY: the originating caller's set, which binds the child live — it
	// withholds one of the child's tools and drops its keyspace — and must
	// bind it the same once the child resumes with no parent to inherit from.
	callerACL := subRunParityCallerACL(t, h)
	parent := tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{UserID: "u1", AgentID: "a_parent", RootRunID: "r_parent"})
	parent = authz.WithTransport(authz.WithMode(parent, authz.ModeCompat), "http")
	parent = tools.WithRunID(parent, "r_parent")
	parent = tools.WithRunPolicy(parent, tools.RunPolicy{
		ACL:               callerACL,
		Hosts:             tools.HostPolicyValue{AllowedHosts: []string{"api.example.com"}, HasList: true},
		FanoutCap:         8,
		Crypto:            tools.CryptoPolicyValue{Scopes: []string{"run"}},
		ShellCreds:        tools.ShellCredsPolicyValue{Set: true, Names: []string{"GITHUB_TOKEN", "NPM_TOKEN"}},
		GitRemote:         tools.GitRemotePolicyValue{Fetch: true},
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
	// The caller's set, compiled again from the run's record, and the tool
	// set narrowed by it as resume narrows the definition's.
	resumeCtx, acl, err := h.srv.resumedACL(resumeCtx, run, runCfg)
	if err != nil {
		t.Fatal(err)
	}
	src := h.srv.resumedPolicySource(resumeCtx, run, runCfg, resolved)
	// Resume re-derives these from the definition, as the spawn did.
	resumedTools := filterTools(h.srv.candidateTools(resumeCtx, run.TenantID, resolved.Tools), resolved.Tools, nil)
	resumedTools = h.srv.grantRecallTool(resumedTools, runCfg.Context)
	src.Tools, src.ACLRemovedTools = tools.NarrowToolsByACL(resumeCtx, acl, resumedTools)
	src.ACL = acl
	src.CoreBlocks = live.CoreBlocks.Blocks
	resumed, _ := h.srv.agentRunPolicy(resumeCtx, resolved, run.Agent, src)

	// The values the fixture is built to exercise, so a fixture that stops
	// exercising them fails rather than comparing two zeros.
	if !live.Hosts.NarrowOnly || live.FanoutCap != 2 || live.Interruption.Load().MaxPending != 1 ||
		!slices.Equal(live.Crypto.Scopes, []string{"run"}) || live.EphemeralWritable ||
		!live.ShellCreds.Set || !slices.Equal(live.ShellCreds.Names, []string{"NPM_TOKEN"}) ||
		live.GitRemote != (tools.GitRemotePolicyValue{Fetch: true}) ||
		live.Compaction == nil || live.Compaction.KeepLastN != nil ||
		live.ACL == nil || !slices.Equal(live.ACLRemovedTools, []string{"Agent", "Recall"}) || len(live.Memory.AllowedScopes) != 0 {
		t.Fatalf("fixture: the live child's policy is not the one this test is built around: %+v", live)
	}
	if got, want := resumed.Interruption.Load(), live.Interruption.Load(); !reflect.DeepEqual(got, want) {
		t.Errorf("interruption: resumed %+v, live %+v", got, want)
	}
	// Every other field compares as it is, Overrides included: live, a child
	// keeps its PARENT's offer (its own children of the parent's definition
	// take the parent's overrides), and its record keeps that offer for a
	// resume to restore. The holder is compared by what it holds, above.
	if len(live.Overrides.Record) == 0 {
		t.Fatal("fixture: the live child holds no offer from its parent, so Overrides compares two zeros")
	}
	for _, p := range []*tools.RunPolicy{&live, &resumed} {
		p.Interruption = nil
	}
	if !reflect.DeepEqual(resumed, live) {
		t.Errorf("the resumed sub-run's policy differs from the one it ran with live:\n resumed %+v\n live    %+v", resumed, live)
	}
}

// subRunParityCallerACL is the set of member u1 (tenant ""), compiled by the
// server's resolver and bound to a list that denies the child's Agent and
// its auto-granted Recall
// tool and its user keyspace.
func subRunParityCallerACL(t *testing.T, h *reviewHarness) *authz.Effective {
	t.Helper()
	ctx := context.Background()
	if _, err := h.st.AclDefPut(ctx, store.AclDefRow{Name: "u1-limits", Rules: json.RawMessage(`[
		{"effect":"deny","actions":["invoke"],"resources":["tool:Agent","tool:Recall"]},
		{"effect":"deny","actions":["read"],"resources":["memory:user"]}]`)}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.st.AclBindingPut(ctx, store.AclBindingRow{ACLName: "u1-limits", SubjectKind: "user", SubjectID: "u1"}); err != nil {
		t.Fatal(err)
	}
	h.srv.SetACLResolver(aclres.New(h.st, authz.ModeCompat, 0))
	p, err := h.srv.ACLResolver().Resolve(ctx, auth.Principal{Subject: "u1", TokenDefID: "def_u1", Scopes: []string{auth.ScopeRunsCreate, auth.ScopeRunsRead}})
	if err != nil || p.Effective == nil {
		t.Fatalf("resolve u1's set: %v", err)
	}
	return p.Effective
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

// resumedRunCtx is the ctx a resume runs runID's loop on, as far as a spawn
// from it reads one: the run's identity, its agent name, its run id and the
// policy rebuilt from its row and record (resumePausedRun). The tool set is
// left out: it is the run's own (ruleOwn), and no child reads it.
func resumedRunCtx(t *testing.T, h *reviewHarness, runID string) context.Context {
	t.Helper()
	run, err := h.st.GetRun(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	runCfg, ok := decodeRunConfig(run.RunConfig)
	if !ok {
		t.Fatalf("run %s wrote no configuration record", runID)
	}
	ctx := tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{
		UserID: run.UserID, TenantID: run.TenantID, AgentID: run.AgentID, SessionID: run.SessionID, Isolated: run.Isolated,
	})
	ctx = tools.WithAgentName(ctx, run.Agent)
	def, _, err := h.srv.resolveAgentName(ctx, run.TenantID, run.Agent, nameFromAgentTool)
	if err != nil {
		t.Fatal(err)
	}
	policy, _ := h.srv.agentRunPolicy(ctx, def, run.Agent, h.srv.resumedPolicySource(ctx, run, runCfg, def))
	ctx = tools.WithRunPolicy(ctx, policy)
	ctx = tools.WithParentRunID(ctx, run.ParentRunID)
	return tools.WithRunID(ctx, run.ID)
}

// H5, the case recomputing from the parent chain would get wrong: root A
// narrows its interruptions, spawns B (another definition), and B spawns A′ (A
// again). Live, B holds A's offer and hands it to A′, which is A's definition
// and takes the narrowing. A resume of B used to offer B's own record instead,
// which A′ is not the definition of — so an A′ spawned after B resumed asked
// a person as often as its definition allows, where its live twin could not.
func TestResume_ChainGrandchildOfTheRootsDefinitionKeepsItsNarrowing(t *testing.T) {
	h := newReviewHarness(t)
	ctx := context.Background()
	cfg := h.srv.cfg()
	cfg.Agents["A"] = config.AgentDef{
		Model: "stub-model", SystemPrompt: "a", Tools: []string{"Agent", "Interruption"},
		Interruption: config.AgentInterruptionACL{Enabled: true, Kinds: []string{"question"}, MaxPending: 5},
	}
	cfg.Agents["B"] = config.AgentDef{Model: "stub-model", SystemPrompt: "b", Tools: []string{"Agent"}}

	// The root's row and record, as a top-level run of A writes them: a spawn
	// reads the offering run's record as it is now.
	rootRec := runConfigRecord{Interruption: &config.AgentInterruptionACL{Enabled: true, MaxPending: 1}}
	sess, err := h.st.CreateSession(ctx, "", "A", "u1")
	if err != nil {
		t.Fatal(err)
	}
	root, err := h.st.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_root", UserID: "u1", RunConfig: rootRec.marshal()})
	if err != nil {
		t.Fatal(err)
	}
	rootCtx := tools.WithRunIdentity(ctx, tools.RunIdentityValue{UserID: "u1", AgentID: "a_root", RootRunID: root.ID, SessionID: sess.ID})
	rootCtx = tools.WithAgentName(tools.WithRunID(rootCtx, root.ID), "A")
	rootCtx = tools.WithRunPolicy(rootCtx, tools.RunPolicy{
		Overrides: tools.RunOverridesValue{Record: rootRec.marshal(), AgentName: "A", RunID: root.ID},
	})

	spawn := func(parent context.Context, name string) *subRunPrep {
		t.Helper()
		prep, err := h.srv.prepareSubRunValues(parent, name, nameFromAgentTool, "", "go", "", false, func(providers.Event) {}, nil, nil, false, false)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(prep.cleanup)
		return prep
	}
	b := spawn(rootCtx, "B")
	aLive := spawn(b.LoopCtx, "A")
	live := tools.PolicyOf(aLive.LoopCtx).Interruption.Load()
	// What live does, pinned so a change to it is seen: A′ is A's definition,
	// so it takes the root's narrowing through B, which is not.
	if live.MaxPending != 1 {
		t.Fatalf("live: A′ under B allows %d pending interruptions; the root narrowed A to 1", live.MaxPending)
	}

	// B resumed, then spawning A′.
	aUnderResumedB := spawn(resumedRunCtx(t, h, b.RunID), "A")
	if got := tools.PolicyOf(aUnderResumedB.LoopCtx).Interruption.Load(); !reflect.DeepEqual(got, live) {
		t.Errorf("A′ spawned by a resumed B: interruption %+v; spawned by the live B: %+v", got, live)
	}
	// A′ itself resumed.
	if got := tools.PolicyOf(resumedRunCtx(t, h, aLive.RunID)).Interruption.Load(); !reflect.DeepEqual(got, live) {
		t.Errorf("A′ resumed: interruption %+v; live: %+v", got, live)
	}
}

// A sub-run recorded before the offer was (its spawn record has none) resumes
// as it always did: offering its own record to its children.
func TestResume_SubRunWithNoRecordedOfferOffersItsOwnRecord(t *testing.T) {
	h := newReviewHarness(t)
	run := store.Run{ID: "r_child", Agent: "child", ParentRunID: "r_parent", UserID: "u1"}
	rec := runConfigRecord{Spawn: &spawnRecord{}, Interruption: &config.AgentInterruptionACL{Enabled: true, MaxPending: 1}}
	ctx := tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{UserID: "u1"})
	got := h.srv.resumedPolicySource(ctx, run, rec, config.AgentDef{Model: "stub-model"}).Overrides
	want := tools.RunOverridesValue{Record: rec.marshal(), AgentName: "child", RunID: "r_child"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("offer = %+v (record %s), want its own record %s", got, got.Record, want.Record)
	}
}

// The recorded offer reads back as the offer it was, and an offer whose record
// is not JSON is recorded without it rather than costing the child its whole
// configuration record (json.Marshal refuses a RawMessage that is not JSON).
func TestOfferedOverridesRecord_RoundTripsAndNeverCostsTheChildItsRecord(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   tools.RunOverridesValue
		want tools.RunOverridesValue
	}{
		{"json", tools.RunOverridesValue{Record: []byte(`{"interruption":{"enabled":true,"max_pending":1}}`), DefID: "d1", AgentName: "A", RunID: "r_root"},
			tools.RunOverridesValue{Record: []byte(`{"interruption":{"enabled":true,"max_pending":1}}`), DefID: "d1", AgentName: "A", RunID: "r_root"}},
		{"not json", tools.RunOverridesValue{Record: []byte(`not json`), AgentName: "A", RunID: "r_root"},
			tools.RunOverridesValue{AgentName: "A", RunID: "r_root"}},
		{"no offer", tools.RunOverridesValue{}, tools.RunOverridesValue{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := runConfigRecord{Spawn: &spawnRecord{OfferedOverrides: offeredOverridesRecordOf(tc.in)}}.marshal()
			if raw == nil {
				t.Fatal("the child's record did not marshal")
			}
			back, ok := decodeRunConfig(raw)
			if !ok || back.Spawn == nil || back.Spawn.OfferedOverrides == nil {
				t.Fatalf("the recorded offer did not read back from %s", raw)
			}
			if got := back.Spawn.OfferedOverrides.value(); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("offer read back as %+v, want %+v", got, tc.want)
			}
		})
	}
}
