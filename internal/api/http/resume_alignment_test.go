package http

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/aclres"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/runner"
	"github.com/denn-gubsky/loomcycle/internal/store"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// A resumed run starts its loop again. Where RunOnce's start and the resume
// did the same thing differently, the owner ruled (RFC GH D7, 2026-11-11)
// that the resume does what a start does. Each test here pins one of those
// rulings on a resumed run.

// pausedRunRow writes a paused top-level run of agent in a new session of
// alice's, its transcript one user turn, as a restart or a restore leaves one.
func pausedRunRow(t *testing.T, srv *Server, agent, agentID string, interactive bool, segs []loop.PromptSegment) store.Run {
	t.Helper()
	ctx := context.Background()
	sess, err := srv.store.CreateSession(ctx, "", agent, "alice")
	if err != nil {
		t.Fatal(err)
	}
	run, err := srv.store.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: agentID, UserID: "alice", Interactive: interactive})
	if err != nil {
		t.Fatal(err)
	}
	appendResumeEvent(t, srv, run.ID, "user_input", segs)
	if err := srv.store.SetRunPauseState(ctx, run.ID, store.PauseStatePaused); err != nil {
		t.Fatal(err)
	}
	if run, err = srv.store.GetRun(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	return run
}

// waitLoopGone waits until the run's loop has let its registration go.
func waitLoopGone(t *testing.T, srv *Server, run store.Run) {
	t.Helper()
	waitFor(t, "the resumed run to end", func() bool {
		_, live := srv.cancelReg.Get(run.AgentID)
		return !live
	})
}

func resumeAlignServer(t *testing.T, prov providers.Provider) *Server {
	t.Helper()
	cfg := makeBaseConfig()
	cfg.Agents = map[string]config.AgentDef{"worker": {Model: "stub-model", Tools: []string{}, SystemPrompt: "role:worker."}}
	srv, _ := makeServer(t, prov, cfg)
	return srv
}

// resumePhaseOrder is the order a resume runs its phases in. The rules that
// make it so are asserted one by one in the test below.
var resumePhaseOrder = []string{
	"resume.conditions", "resume.agent", "resume.acl", "resume.provider",
	"resume.tools", "tools.filter", "tools.recall", "tools.acl", "tools.hosts", "resume.prompt",
	"resume.session", "resume.slot", "resume.budget", "resume.loop",
}

func TestResume_PhasesRunInTheirSecurityOrder(t *testing.T) {
	srv := resumeAlignServer(t, &recordingScriptedProvider{defaultS: endTurn()})
	var mu sync.Mutex
	var got []string
	srv.runPhaseHook = func(phase string) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, phase)
	}
	run := pausedRunRow(t, srv, "worker", "a_resume_order", false, oneUserSeg("go"))
	resumeOne(t, srv)
	waitRunStatus(t, srv.store, run.ID, store.RunCompleted)
	mu.Lock()
	defer mu.Unlock()

	for _, rule := range []struct{ before, after, why string }{
		{"resume.conditions", "resume.provider", "the condition facts are stamped before the model is asked of the caller's rules"},
		{"resume.acl", "resume.provider", "the caller's rules are compiled before the model is resolved"},
		{"resume.tools", "resume.prompt", "the ACL removes tools before the system prompt is built from them"},
		{"tools.filter", "tools.recall", "the caller's tools list narrows before Recall is granted"},
		{"tools.recall", "tools.acl", "the ACL can withhold the Recall the context grants"},
		{"tools.acl", "tools.hosts", "the caller's host list narrows only what the ACL left"},
		{"tools.acl", "resume.prompt", "the prompt is built from what the ACL left"},
		{"resume.session", "resume.slot", "the session is taken before the run goes live and waits"},
		{"resume.slot", "resume.budget", "the budget is asked when the run starts, after its wait"},
		{"resume.budget", "resume.loop", "a run over its budget makes no model call"},
	} {
		b, a := slices.Index(got, rule.before), slices.Index(got, rule.after)
		if b < 0 || a < 0 || b > a {
			t.Errorf("%s must run before %s (%s); order was %s", rule.before, rule.after, rule.why, strings.Join(got, " > "))
		}
	}
	if !slices.Equal(got, resumePhaseOrder) {
		t.Errorf("resume phase order\n got: %s\nwant: %s", strings.Join(got, " > "), strings.Join(resumePhaseOrder, " > "))
	}
}

// A `use model:` deny conditioned on the run's own agent, bound while it was
// paused, refuses its resume: the resume asks the model of the caller's rules
// with the run's facts on ctx, as RunOnce does. Asked without them, the
// agent condition read no agent, did not hold, and the deny was skipped — the
// run resumed on a model its caller's rules now deny it.
func TestResume_AConditionedModelDenyReadsTheRunsFacts(t *testing.T) {
	e := keyspaceWriteFixture(t, authz.ModeCompat, `[{"effect":"deny","actions":["invoke"],"resources":["tool:Nope"]}]`,
		[][]providers.Event{endTurn(), endTurn()})
	run := e.run(t, "a_keeper_cond_resume")
	bindTenantWide(t, e.st, "no-models-for-keeper", `[{"effect":"deny","actions":["use"],"resources":["model:*"],"when":{"agent":"keeper"}}]`)
	parkForResume(t, e.srv, run.ID)
	n, warnings := e.srv.ResumePausedRuns(context.Background())
	if n != 0 || !strings.Contains(strings.Join(warnings, "\n"), "acl_denied") {
		t.Fatalf("resumed %d (warnings %v), want the resume refused with acl_denied: the deny names this run's agent", n, warnings)
	}
}

// The system prompt a resumed run runs on is re-derived, and recorded on its
// transcript with its provenance, as a start records its own.
func TestResume_RecordsTheSystemPromptItRunsOn(t *testing.T) {
	e := aclResumeFixture(t, false, map[string][][]providers.Event{"worker": {endTurn(), endTurn()}})
	run := e.startAs(t, "worker", "a_prompt_resume")
	before := eventsOf(t, e.st, run, "system_prompt")
	if len(before) != 1 {
		t.Fatalf("fixture: the live run recorded %d system prompts, want 1", len(before))
	}
	parkForResume(t, e.srv, run.ID)
	resumeOne(t, e.srv)
	waitLoopGone(t, e.srv, run)
	after := eventsOf(t, e.st, run, "system_prompt")
	if len(after) != 2 || !strings.Contains(after[1], "role:worker.") {
		t.Errorf("system_prompt events after the resume = %v, want the live one and the resumed one", after)
	}
}

// A tool the caller's ACL withholds is said once at the resumed run's start,
// as at the live run's: the set is compiled anew and may withhold others.
func TestResume_EmitsTheStartTimeWarnings(t *testing.T) {
	e := aclResumeFixture(t, true, map[string][][]providers.Event{"worker": {endTurn(), endTurn()}})
	run := e.startAs(t, "worker", "a_warn_resume")
	inert := string(providers.EventCapabilityInert)
	if got := eventsOf(t, e.st, run, inert); len(got) != 1 || !strings.Contains(got[0], "Bash") {
		t.Fatalf("fixture: the live run's warnings = %v, want Bash withheld", got)
	}
	parkForResume(t, e.srv, run.ID)
	resumeOne(t, e.srv)
	waitLoopGone(t, e.srv, run)
	if got := eventsOf(t, e.st, run, inert); len(got) != 2 || !strings.Contains(got[1], "Bash") {
		t.Errorf("warnings after the resume = %v, want the resumed run to say Bash is withheld too", got)
	}
}

// defOpProbe asks the caller's rules whether it may author agent x, as an
// AgentDef op does.
type defOpProbe struct{}

func (defOpProbe) Name() string                 { return "DefProbe" }
func (defOpProbe) Description() string          { return "asks a definition op" }
func (defOpProbe) InputSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (defOpProbe) Execute(ctx context.Context, _ json.RawMessage) (tools.Result, error) {
	if res, refused := tools.AuthorizeDefOp(ctx, "agent", "create", "x"); refused {
		return res, nil
	}
	return tools.Result{Text: "allowed"}, nil
}

// A definition op the caller's rules refuse inside a resumed run is recorded
// in the audit log, as inside a live run.
func TestResume_RecordsARefusedDefinitionOp(t *testing.T) {
	cfg := makeBaseConfig()
	cfg.Defaults.Provider = "scripted"
	cfg.Env.AuthToken = "legacy-admin-secret"
	cfg.Env.OperatorTokenPepper = testTokenPepper
	cfg.Agents = map[string]config.AgentDef{"worker": {Model: "stub-model", Tools: []string{"DefProbe"}, SystemPrompt: "role:worker."}}
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "defop.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	if _, err := st.OperatorTokenDefCreate(ctx, store.OperatorTokenDefRow{
		DefID: "def_worker", Name: "u-def_worker", TenantID: "acme", Subject: "dev",
		TokenHash: auth.HashToken(testTokenPepper, workerBearer), AllowedScopes: []string{auth.ScopeRunsCreate, auth.ScopeRunsRead},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AclDefPut(ctx, store.AclDefRow{TenantID: "acme", Name: "no-x", Rules: json.RawMessage(`[{"effect":"deny","actions":["author"],"resources":["def:agent/x"]}]`)}); err != nil {
		t.Fatal(err)
	}
	bindACL(t, st, "no-x")
	prov := &roleProvider{roles: []string{"worker"}, calls: map[string]int{}, scripts: map[string][][]providers.Event{
		"worker": {toolCallTurn("tu_live", "DefProbe", `{}`), endTurn(), toolCallTurn("tu_resumed", "DefProbe", `{}`), endTurn()},
	}}
	srv := New(cfg, &stubResolver{p: prov}, []tools.Tool{defOpProbe{}}, concurrency.New(8, 8, 5*time.Second), st)
	srv.SetACLResolver(aclres.New(st, authz.ModeCompat, 0))
	sink := &recordingSink{}
	srv.SetTokenAudit(sink)
	ts := httptest.NewServer(srv.Mux())
	t.Cleanup(ts.Close)
	e := &aclResumeEnv{srv: srv, st: st, ts: ts}

	run := e.startAs(t, "worker", "a_defop_resume")
	refusals := func() int {
		n := 0
		for _, ev := range sink.events {
			if ev.TargetName == "def:agent/x" {
				n++
			}
		}
		return n
	}
	live := refusals()
	if live != 1 {
		t.Fatalf("fixture: the live run's refusal was recorded %d times, want 1", live)
	}
	parkForResume(t, srv, run.ID)
	resumeOne(t, srv)
	waitLoopGone(t, srv, run)
	if res := resumedToolResult(t, srv, run, "tu_resumed"); !strings.Contains(res, "acl_denied") {
		t.Fatalf("fixture: the resumed op was not refused: %s", res)
	}
	if got := refusals(); got != live+1 {
		t.Errorf("audit records of the refusal after the resume = %d, want %d: the resumed run's refusal is not recorded", got, live+1)
	}
}

// A resumed run's turn calls carry its run slot, so they can lend it while
// they wait for a provider lane, as a fresh run's do.
func TestResume_TurnCallsCarryTheRunSlot(t *testing.T) {
	prov := &slotSeeingProvider{recordingScriptedProvider: recordingScriptedProvider{defaultS: endTurn()}}
	srv := resumeAlignServer(t, prov)
	run := pausedRunRow(t, srv, "worker", "a_slot_resume", false, oneUserSeg("go"))
	resumeOne(t, srv)
	waitRunStatus(t, srv.store, run.ID, store.RunCompleted)
	prov.mu.Lock()
	defer prov.mu.Unlock()
	if len(prov.seen) == 0 || !prov.seen[0] {
		t.Errorf("the resumed run's calls carried a run slot: %v, want true", prov.seen)
	}
}

// An interactive top-level run's turn can be stopped after a resume, as it can
// on a fresh start: its turn-cancel is armed while the turn runs.
func TestResume_InteractiveRunArmsTurnCancel(t *testing.T) {
	prov := &pausableProvider{release: make(chan struct{}), finalText: "done"}
	srv := resumeAlignServer(t, prov)
	run := pausedRunRow(t, srv, "worker", "a_turncancel_resume", true, oneUserSeg("go"))
	t.Cleanup(func() {
		close(prov.release)
		srv.cancelReg.CancelLocalRun(run.AgentID, run.ID, "test over")
		waitLoopGone(t, srv, run)
	})
	resumeOne(t, srv)
	waitFor(t, "the resumed turn's cancel to be armed", func() bool { return srv.turnCancelReg.IsArmed(run.ID) })
}

// A resumed top-level run holds its session as a continuation does: another
// continuation of the session is refused busy while it runs.
func TestResume_HoldsItsSessionAgainstAContinuation(t *testing.T) {
	prov := &pausableProvider{release: make(chan struct{}), finalText: "done"}
	srv := resumeAlignServer(t, prov)
	run := pausedRunRow(t, srv, "worker", "a_session_resume", false, oneUserSeg("go"))
	released := false
	release := func() {
		if !released {
			released = true
			close(prov.release)
		}
	}
	t.Cleanup(func() {
		release()
		waitLoopGone(t, srv, run)
	})
	resumeOne(t, srv)
	done := make(chan error, 1)
	go func() {
		done <- srv.RunOnce(context.Background(), runner.RunInput{SessionID: run.SessionID, UserID: "alice", Segments: oneUserSeg("again")}, runner.RunCallbacks{})
	}()
	select {
	case err := <-done:
		if !errors.Is(err, runner.ErrSessionBusy) {
			t.Errorf("a continuation beside the resumed run = %v, want %v", err, runner.ErrSessionBusy)
		}
	case <-time.After(3 * time.Second):
		t.Error("a continuation of the session ran beside the resumed run: it was not refused busy")
		release()
		<-done
	}
}

// A session another request holds is busy for a resume as for a continuation:
// the run stays paused, and is resumed in this process once the session is
// free — on a single replica no later pass would.
func TestResume_ABusySessionLeavesTheRunPaused(t *testing.T) {
	srv := resumeAlignServer(t, &recordingScriptedProvider{defaultS: endTurn()})
	run := pausedRunRow(t, srv, "worker", "a_busy_resume", false, oneUserSeg("go"))
	hold, ok := srv.holdSession(run.SessionID, false)
	if !ok {
		t.Fatal("fixture: could not hold the session")
	}
	n, warnings := srv.ResumePausedRuns(context.Background())
	got, err := srv.store.GetRun(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	hold.release()
	if n != 0 || !strings.Contains(strings.Join(warnings, "\n"), "in flight") {
		t.Fatalf("resumed %d (warnings %v), want the resume refused: the session is busy", n, warnings)
	}
	if got.PauseState != store.PauseStatePaused {
		t.Errorf("the refused run's pause_state = %q, want %q", got.PauseState, store.PauseStatePaused)
	}
	// Nothing else resumes it on one replica: the retry does, once the
	// session is free.
	waitFor(t, "the refused run to be resumed once its session is free", func() bool {
		r, err := srv.store.GetRun(context.Background(), run.ID)
		return err == nil && r.Status == store.RunCompleted
	})
}

// A soft budget crossing is said once at a resumed run's start, as at a fresh
// run's.
func TestResume_SaysASoftBudgetCrossingAtItsStart(t *testing.T) {
	srv, _, runID, envelope := restoreResumeServer(t)
	ctx := context.Background()
	seedOperatorUsage(t, srv, 1000)
	if err := srv.store.TokenLimitPut(ctx, store.TokenLimitRow{Scope: "operator", SoftLimit: i64p(500), UpdatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := srv.SeedLimits(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.RestoreSnapshot(ctx, connector.RestoreSnapshotRequest{RawJSON: envelope}); err != nil {
		t.Fatalf("RestoreSnapshot: %v", err)
	}
	run := awaitRunCompleted(t, srv.store, runID)
	if got := eventsOf(t, srv.store, run, string(providers.EventLimit)); len(got) != 1 {
		t.Errorf("limit events of the resumed run = %v, want the soft crossing said once", got)
	}
}
