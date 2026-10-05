package http

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/pause"
	"github.com/denn-gubsky/loomcycle/internal/runner"
	"github.com/denn-gubsky/loomcycle/internal/steer"
	"github.com/denn-gubsky/loomcycle/internal/store"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// StartTeamWalk is how a schedule or a webhook starts a walk: detached, as the
// identity the trigger DEFINITION carries, through the same op=run an operator
// calls. These tests drive the real server — the real TeamDef tool, the real
// walk and member runner, a stub model — so what they assert is what a trigger
// gets.

// varsTeam declares two variables and reads both in its one member's prompt.
const varsTeam = `{"entry":"a","vars":{"repo":"no-repo","pr":"0"},"states":[` +
	`{"state":"a","handler":{"kind":"agent","agent":"writer","input_template":"Review ${var.repo} pull ${var.pr}."}},` +
	`{"state":"done","handler":{"kind":"terminal"}}],` +
	`"transitions":[{"from":"a","to":"done","on":"success"}]}`

type triggerWalkHarness struct {
	t    *testing.T
	srv  *Server
	st   store.Store
	cfg  *config.Config
	prov *numberedProvider
}

func newTriggerWalkHarness(t *testing.T) *triggerWalkHarness {
	t.Helper()
	cfg := &config.Config{
		Defaults:    config.Defaults{Provider: "stub", Model: "stub-model"},
		Agents:      map[string]config.AgentDef{"writer": {Model: "stub-model", SystemPrompt: "write"}},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 4, MaxQueueDepth: 4, QueueTimeoutMS: 1000},
	}
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "trigger-walk.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	prov := &numberedProvider{answer: "done"}
	srv := New(cfg, &stubResolver{p: prov}, []tools.Tool{}, concurrency.New(4, 4, 100*time.Millisecond), st)
	srv.SetSteerRegistry(steer.NewRegistry(0))
	srv.SetTeamDefTool(&builtin.TeamDef{Store: st})
	return &triggerWalkHarness{t: t, srv: srv, st: st, cfg: cfg, prov: prov}
}

// finished waits for a detached walk's run to end and returns its row.
func (h *triggerWalkHarness) finished(runID string) store.Run {
	h.t.Helper()
	var run store.Run
	waitFor(h.t, "the walk "+runID+" to finish", func() bool {
		got, err := h.st.GetRun(context.Background(), runID)
		if err != nil {
			return false
		}
		run = got
		return store.IsTerminalRunStatus(got.Status)
	})
	return run
}

// walkStarted reports whether any walk of the team has a run row at all.
func (h *triggerWalkHarness) walkStarted(team string) bool {
	h.t.Helper()
	_, err := h.st.GetRunByAgentID(context.Background(), teamWalkAgentPrefix+team)
	return err == nil
}

func teamRecordOf(t *testing.T, run store.Run) teamWalkRecord {
	t.Helper()
	var cfg struct {
		Team *teamWalkRecord `json:"team"`
	}
	if err := json.Unmarshal(run.RunConfig, &cfg); err != nil || cfg.Team == nil {
		t.Fatalf("the walk's run records no team (%v): %s", err, run.RunConfig)
	}
	return *cfg.Team
}

func TestStartTeamWalk_StartsADetachedWalkAsTheInputsIdentityWithItsVars(t *testing.T) {
	h := newTriggerWalkHarness(t)
	seedTenantTeam(t, h.st, "acme", "pr-review", varsTeam)

	runID, err := h.srv.StartTeamWalk(context.Background(), runner.TeamWalkInput{
		Team: "pr-review", Input: "the payload",
		Vars:     map[string]string{"repo": "loomcycle"}, // pr is left to the team's default
		TenantID: "acme", UserID: "u-42",
		OperatorKeyRestricted: true, Isolated: true,
	})
	if err != nil {
		t.Fatalf("StartTeamWalk: %v", err)
	}
	run := h.finished(runID)
	if run.Status != store.RunCompleted {
		t.Fatalf("walk status = %s (%s), want completed", run.Status, run.ErrorMsg)
	}
	if run.TenantID != "acme" || run.UserID != "u-42" || run.Agent != "team:pr-review" {
		t.Errorf("walk row tenant=%q user=%q agent=%q, want acme / u-42 / team:pr-review", run.TenantID, run.UserID, run.Agent)
	}
	if !run.OperatorKeyRestricted || !run.Isolated {
		t.Errorf("walk row operator_key_restricted=%v isolated=%v, want the definition's bits on it", run.OperatorKeyRestricted, run.Isolated)
	}
	rec := teamRecordOf(t, run)
	if rec.Name != "pr-review" || rec.Mode != "detach" || rec.Input != "the payload" || rec.DefTenant != "acme" {
		t.Errorf("recorded spec = %+v", rec)
	}
	if len(rec.Vars) != 1 || rec.Vars["repo"] != "loomcycle" {
		t.Errorf("recorded vars = %v, want only the supplied repo=loomcycle", rec.Vars)
	}
	seen := h.prov.seen()
	if len(seen) != 1 || seen[0] != "Review loomcycle pull 0." {
		t.Errorf("the member's model was sent %q, want the supplied repo and the default pr", seen)
	}

	// The member ran in the walk's tenant, as its user, under its bits.
	members, _, err := h.st.ListRunsByWalk(context.Background(), "acme", runID, 10, "")
	if err != nil {
		t.Fatal(err)
	}
	sawMember := false
	for _, m := range members {
		if m.ID == runID {
			continue
		}
		sawMember = true
		if m.TenantID != "acme" || m.UserID != "u-42" || !m.OperatorKeyRestricted || !m.Isolated {
			t.Errorf("member %s tenant=%q user=%q restricted=%v isolated=%v, want the walk's identity",
				m.ID, m.TenantID, m.UserID, m.OperatorKeyRestricted, m.Isolated)
		}
	}
	if !sawMember {
		t.Error("the walk spawned no member in tenant acme")
	}
}

// The walk's tenant is the input's and nothing else's. A team that exists only
// in another tenant, or only in the shared layer, is not this trigger's to
// start — and a principal that happens to be on the ctx, even an admin of the
// tenant that does own it, changes neither the lookup nor where it would run.
func TestStartTeamWalk_OnlyTheInputsTenantIsSearchedWhateverIsOnCtx(t *testing.T) {
	h := newTriggerWalkHarness(t)
	seedTenantTeam(t, h.st, "globex", "pr-review", varsTeam)
	seedTenantTeam(t, h.st, "", "pr-review", varsTeam)

	globexAdmin := auth.WithPrincipal(
		tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{TenantID: "globex", UserID: "root"}),
		auth.Principal{TenantID: "globex", Subject: "root", Scopes: []string{auth.ScopeAdmin}})
	for name, ctx := range map[string]context.Context{"a bare ctx": context.Background(), "a globex admin on ctx": globexAdmin} {
		_, err := h.srv.StartTeamWalk(ctx, runner.TeamWalkInput{Team: "pr-review", TenantID: "acme"})
		if !errors.Is(err, runner.ErrTeamNotStartable) {
			t.Errorf("%s: err = %v, want ErrTeamNotStartable for a team acme does not have", name, err)
		}
	}
	if h.walkStarted("pr-review") {
		t.Fatal("a walk started for a tenant that has no such team")
	}

	// The same call naming the owning tenant runs there, not in the ctx's.
	seedTenantTeam(t, h.st, "acme", "pr-review", varsTeam)
	runID, err := h.srv.StartTeamWalk(globexAdmin, runner.TeamWalkInput{Team: "pr-review", TenantID: "acme", UserID: "u-1"})
	if err != nil {
		t.Fatalf("StartTeamWalk: %v", err)
	}
	if run := h.finished(runID); run.TenantID != "acme" || run.UserID != "u-1" {
		t.Errorf("walk row tenant=%q user=%q, want the input's acme / u-1 — not the principal's globex / root", run.TenantID, run.UserID)
	}
}

func TestStartTeamWalk_ATeamOrVarsThatCannotStartAreRefusedAndStartNothing(t *testing.T) {
	cases := []struct {
		name  string
		setup func(h *triggerWalkHarness)
		in    runner.TeamWalkInput
		want  string
	}{
		{"no such team", func(*triggerWalkHarness) {},
			runner.TeamWalkInput{Team: "missing", TenantID: "acme"}, "no active team"},
		{"a retired team", func(h *triggerWalkHarness) {
			seedTenantTeam(h.t, h.st, "acme", "pr-review", varsTeam)
			if err := h.st.TeamDefSetRetired(context.Background(), "tdf_acme_pr-review", true); err != nil {
				h.t.Fatal(err)
			}
		}, runner.TeamWalkInput{Team: "pr-review", TenantID: "acme"}, "retired"},
		{"a variable the team does not declare", func(h *triggerWalkHarness) { seedTenantTeam(h.t, h.st, "acme", "pr-review", varsTeam) },
			runner.TeamWalkInput{Team: "pr-review", TenantID: "acme", Vars: map[string]string{"branch": "main"}}, "not a variable of this team"},
		{"a value carrying a placeholder", func(h *triggerWalkHarness) { seedTenantTeam(h.t, h.st, "acme", "pr-review", varsTeam) },
			runner.TeamWalkInput{Team: "pr-review", TenantID: "acme", Vars: map[string]string{"repo": "x {{thread.output}}"}}, "{{ or }}"},
		{"a value over the size bound", func(h *triggerWalkHarness) { seedTenantTeam(h.t, h.st, "acme", "pr-review", varsTeam) },
			runner.TeamWalkInput{Team: "pr-review", TenantID: "acme", Vars: map[string]string{"repo": strings.Repeat("x", 4097)}}, "more than the maximum"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newTriggerWalkHarness(t)
			c.setup(h)
			_, err := h.srv.StartTeamWalk(context.Background(), c.in)
			if !errors.Is(err, runner.ErrTeamNotStartable) {
				t.Fatalf("err = %v, want ErrTeamNotStartable", err)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("err = %v, want the reason %q for the operator's log", err, c.want)
			}
			if h.walkStarted(c.in.Team) {
				t.Error("a refused start still opened a walk run")
			}
			if n := len(h.prov.seen()); n != 0 {
				t.Errorf("a refused start called the model %d times", n)
			}
		})
	}
}

// The refusals a trigger answers differently from "broken": a spent budget, a
// paused runtime, and a user id no run could carry.
func TestStartTeamWalk_AdmissionRefusalsKeepTheirSentinels(t *testing.T) {
	t.Run("a spent hard budget", func(t *testing.T) {
		h := newTriggerWalkHarness(t)
		seedTenantTeam(t, h.st, "acme", "pr-review", varsTeam)
		one := int64(1)
		h.srv.limits.PutLimit(store.TokenLimitRow{TenantID: "acme", Scope: "tenant", HardLimit: &one})
		h.srv.limits.Add("acme", "u-1", 5)
		_, err := h.srv.StartTeamWalk(context.Background(), runner.TeamWalkInput{Team: "pr-review", TenantID: "acme", UserID: "u-1"})
		if !errors.Is(err, runner.ErrTokenLimitExceeded) {
			t.Fatalf("err = %v, want ErrTokenLimitExceeded", err)
		}
		if h.walkStarted("pr-review") {
			t.Error("an over-budget start still opened a walk run")
		}
	})
	t.Run("a paused runtime", func(t *testing.T) {
		h := newTriggerWalkHarness(t)
		seedTenantTeam(t, h.st, "acme", "pr-review", varsTeam)
		mgr := pause.NewManager(h.st, 200*time.Millisecond)
		h.srv.SetPauseManager(mgr)
		if _, err := mgr.Pause(context.Background(), time.Second); err != nil {
			t.Fatalf("pause: %v", err)
		}
		_, err := h.srv.StartTeamWalk(context.Background(), runner.TeamWalkInput{Team: "pr-review", TenantID: "acme"})
		if !errors.Is(err, runner.ErrRuntimePaused) {
			t.Fatalf("err = %v, want ErrRuntimePaused", err)
		}
		if h.walkStarted("pr-review") {
			t.Error("a start while paused still opened a walk run")
		}
	})
	t.Run("a user id no run could carry", func(t *testing.T) {
		h := newTriggerWalkHarness(t)
		seedTenantTeam(t, h.st, "acme", "pr-review", varsTeam)
		_, err := h.srv.StartTeamWalk(context.Background(), runner.TeamWalkInput{Team: "pr-review", TenantID: "acme", UserID: "../../etc"})
		if !errors.Is(err, runner.ErrInvalidArgument) {
			t.Fatalf("err = %v, want ErrInvalidArgument", err)
		}
	})
}

// A delivery's dedup keys land on the walk's run, so a redelivery finds the
// walk it already started, and two that race open one walk between them.
func TestStartTeamWalk_ADeliveryKeyStartsOneWalk(t *testing.T) {
	h := newTriggerWalkHarness(t)
	seedTenantTeam(t, h.st, "acme", "pr-review", varsTeam)
	in := runner.TeamWalkInput{Team: "pr-review", TenantID: "acme", IdempotencyKey: "wh:acme/pr:d-1", DeliveryAltKey: "wh:acme/pr:body-1"}

	first, err := h.srv.StartTeamWalk(context.Background(), in)
	if err != nil {
		t.Fatalf("StartTeamWalk: %v", err)
	}
	h.finished(first)
	for _, key := range []string{in.IdempotencyKey, in.DeliveryAltKey} {
		got, ok, err := h.st.RunByDeliveryKeys(context.Background(), []string{key})
		if err != nil || !ok || got.ID != first {
			t.Errorf("RunByDeliveryKeys(%q) = %q, %v, %v; want the walk %s", key, got.ID, ok, err, first)
		}
	}

	if _, err := h.srv.StartTeamWalk(context.Background(), in); !errors.Is(err, store.ErrDuplicateIdempotencyKey) {
		t.Fatalf("second start with the same keys: err = %v, want ErrDuplicateIdempotencyKey", err)
	}
	if n := len(h.prov.seen()); n != 1 {
		t.Errorf("the model was called %d times across both starts, want once", n)
	}
}

// The keys name the triggered walk's own row. The walk's ctx — which every
// member runs under, and a member may start a walk of its own — must not carry
// them on, or that nested walk would claim the same keys and be refused.
func TestOpenTeamWalkRun_TheWalkCtxDoesNotCarryTheDeliveryKeysOn(t *testing.T) {
	h := newTriggerWalkHarness(t)
	start := &triggerWalkStart{idempotencyKey: "wh:acme/pr:d-9"}
	ctx := triggerWalkCtx(context.Background(), runner.TeamWalkInput{Team: "pr-review", TenantID: "acme"}, start)
	if triggerWalkStartFrom(ctx) != start {
		t.Fatal("the start is not on the trigger's own ctx")
	}
	walkCtx, runID, finish, err := h.srv.openTeamWalkRun(ctx, builtin.WalkRunSpec{Name: "pr-review", Detach: true})
	if err != nil {
		t.Fatalf("openTeamWalkRun: %v", err)
	}
	defer finish(builtin.WalkEnd{})
	if got, ok, _ := h.st.RunByDeliveryKeys(context.Background(), []string{"wh:acme/pr:d-9"}); !ok || got.ID != runID {
		t.Errorf("the walk's row does not carry the delivery key")
	}
	if triggerWalkStartFrom(walkCtx) != nil {
		t.Error("the walk ctx still carries the delivery keys, so a nested walk would claim them again")
	}
}
