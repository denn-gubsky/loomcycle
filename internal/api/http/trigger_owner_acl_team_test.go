package http

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/aclres"
	"github.com/denn-gubsky/loomcycle/internal/api/webhook"
	"github.com/denn-gubsky/loomcycle/internal/audit"
	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/scheduler"
	"github.com/denn-gubsky/loomcycle/internal/steer"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// RFC EZ §1.5, for a `delivery: team` trigger: a ScheduleDef or WebhookDef
// that starts a team walk records its author (acl_owner) exactly as one that
// starts an agent run does, and the walk it starts carries that author's
// CURRENT set — `run team:<name>` is asked for the author, and every member
// the walk spawns is narrowed by the set and asked `spawn agent:` for it, as
// a member of a walk an agent started is narrowed by that agent's caller.
// Before, a triggered walk had no set at all: its members ran on their
// agents' full grants whatever the author was bound to.

// probeTeam is one state running worker, which probes its policy.
const probeTeam = `{"entry":"a","states":[` +
	`{"state":"a","handler":{"kind":"agent","agent":"worker","input_template":"go"}},` +
	`{"state":"done","handler":{"kind":"terminal"}}],` +
	`"transitions":[{"from":"a","to":"done","on":"success"}]}`

// withProbeTeam wires walks into the fixture and seeds acme's probe-team.
func (e *aclResumeEnv) withProbeTeam(t *testing.T) {
	t.Helper()
	e.srv.SetSteerRegistry(steer.NewRegistry(0))
	e.srv.SetTeamDefTool(&builtin.TeamDef{Store: e.st})
	seedTenantTeam(t, e.st, "acme", "probe-team", probeTeam)
}

// createTeamScheduleAsDev writes a team schedule of probe-team as dev and
// makes it due now.
func (e *aclResumeEnv) createTeamScheduleAsDev(t *testing.T, name string) string {
	t.Helper()
	e.srv.SetScheduleDefTool(&builtin.ScheduleDef{Store: e.st, Cfg: e.srv.cfg()})
	ctx := tools.WithScheduleDefPolicy(e.authorCtx(t), tools.ScheduleDefPolicyValue{Scopes: []string{"any"}, SelfName: "author"})
	res, err := e.srv.ScheduleDef(ctx, json.RawMessage(`{"op":"create","name":"`+name+`","overlay":{"schedule":"0 9 * * 1","delivery":"team","team":"probe-team"}}`))
	if err != nil || res.IsError {
		t.Fatalf("create team schedule: %v %s", err, res.Text)
	}
	var created struct {
		DefID string `json:"def_id"`
	}
	if err := json.Unmarshal([]byte(res.Text), &created); err != nil || created.DefID == "" {
		t.Fatalf("create result %s: %v", res.Text, err)
	}
	if o := scheduleOwnerOf(t, e.st, created.DefID); o == nil || o.Subject != "dev" {
		t.Fatalf("fixture: the team schedule records owner %+v, want dev", o)
	}
	if err := e.st.ScheduleRunStateSeed(context.Background(), created.DefID, time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	return created.DefID
}

// startTeamScheduler is startScheduler with the server as its walk starter.
func (e *aclResumeEnv) startTeamScheduler(t *testing.T) (stop func()) {
	t.Helper()
	sched := scheduler.New(scheduler.Config{TickInterval: 20 * time.Millisecond}, e.st, e.srv, nil, nil, schedulerTestLogger(t, nil))
	sched.SetTeamWalkStarter(e.srv)
	sched.Start(context.Background())
	var stopped bool
	stop = func() {
		if !stopped {
			stopped = true
			sched.Stop()
		}
	}
	t.Cleanup(stop)
	return stop
}

// memberOf waits for the walk's one member run to end and returns its row.
func (e *aclResumeEnv) memberOf(t *testing.T, walkID string) store.Run {
	t.Helper()
	var member store.Run
	waitFor(t, "the walk's member to end", func() bool {
		kids, err := e.st.ListRunsByParentRunID(context.Background(), walkID)
		if err != nil || len(kids) != 1 || !store.IsTerminalRunStatus(kids[0].Status) {
			return false
		}
		member = kids[0]
		return true
	})
	return member
}

// assertMemberRecordsOwner fails unless the member's record names dev under
// transport, so a resume of the member narrows it by dev's set again.
func assertMemberRecordsOwner(t *testing.T, member store.Run, transport string) {
	t.Helper()
	if rec, ok := decodeRunConfig(member.RunConfig); !ok || rec.ACLOrigin == nil || rec.ACLOrigin.Subject != "dev" || rec.ACLOrigin.Transport != transport {
		t.Errorf("the walk's member does not record its owner under the %s transport: %s", transport, member.RunConfig)
	}
}

// bindTeamRunDeny binds dev to a list that denies running probe-team.
func (e *aclResumeEnv) bindTeamRunDeny(t *testing.T) {
	t.Helper()
	if _, err := e.st.AclDefPut(context.Background(), store.AclDefRow{TenantID: "acme", Name: "no-team",
		Rules: json.RawMessage(`[{"effect":"deny","actions":["run"],"resources":["team:probe-team"]}]`)}); err != nil {
		t.Fatal(err)
	}
	bindACL(t, e.st, "no-team")
}

// A team tick's walk spawns its member narrowed by the schedule owner's list,
// and the member records its owner under the schedule transport.
func TestScheduleTeamFire_WalkMembersAreNarrowedByTheOwnersACL(t *testing.T) {
	e := aclResumeFixture(t, true, map[string][][]providers.Event{"worker": {probeTurn("tu_st"), endTurn()}})
	e.withProbeTeam(t)
	defID := e.createTeamScheduleAsDev(t, "nightly-team")
	e.startTeamScheduler(t)
	state := e.waitScheduleStatus(t, defID, "completed")
	e.assertNarrowedByDev(t, e.waitProbes(t, 1)[0])
	assertMemberRecordsOwner(t, e.memberOf(t, state.LastRunID), "schedule")
}

// A deny on running the team, bound to the owner, refuses the tick as a deny
// on running an agent refuses an agent tick: no walk, no model call, the
// schedule records the failure as acl_denied (counted, as any failed fire),
// and the refusal is audited.
func TestScheduleTeamFire_ADenyOnRunningTheTeamRefusesTheFire(t *testing.T) {
	e := aclResumeFixture(t, false, map[string][][]providers.Event{"worker": {endTurn()}})
	e.withProbeTeam(t)
	defID := e.createTeamScheduleAsDev(t, "denied-team")
	e.bindTeamRunDeny(t)
	stop := e.startTeamScheduler(t)
	state := e.waitScheduleStatus(t, defID, "failed")
	stop()
	if !strings.Contains(state.LastError, "acl_denied") || state.LastRunID != "" {
		t.Errorf("last_error = %q, run %q; want acl_denied and no walk", state.LastError, state.LastRunID)
	}
	if state.FireCount != 1 {
		t.Errorf("fire_count = %d; a denied fire counts as a failed one", state.FireCount)
	}
	if n := e.prov.callsOf("worker"); n != 0 {
		t.Errorf("the refused fire made %d model call(s)", n)
	}
	if !slices.ContainsFunc(e.audit.events, func(ev audit.Event) bool {
		return ev.Action == "acl_denied" && ev.ActorSubject == "dev" && ev.TargetName == "team:probe-team"
	}) {
		t.Errorf("no acl_denied audit event for dev on team:probe-team; events: %+v", e.audit.events)
	}
}

// A tick whose owner's set cannot be read starts no walk: acl_unavailable,
// the slot given back to the next tick, max_fires untouched.
func TestScheduleTeamFire_UnreadableOwnerACLRefusesTheFire(t *testing.T) {
	e := aclResumeFixture(t, true, map[string][][]providers.Event{"worker": {endTurn()}})
	e.withProbeTeam(t)
	defID := e.createTeamScheduleAsDev(t, "held-team")
	e.srv.SetACLResolver(aclres.New(failingACLStore{e.st}, authz.ModeCompat, 0))
	stop := e.startTeamScheduler(t)
	state := e.waitScheduleStatus(t, defID, "failed")
	stop()
	if !strings.Contains(state.LastError, "acl_unavailable") || !strings.Contains(state.LastError, "fired again on the next tick") {
		t.Errorf("last_error = %q; want acl_unavailable, retried", state.LastError)
	}
	if state.FireCount != 0 {
		t.Errorf("fire_count = %d; a refused-for-now fire must not use up max_fires", state.FireCount)
	}
	if n := e.prov.callsOf("worker"); n != 0 {
		t.Errorf("the refused fire made %d model call(s)", n)
	}
	if !slices.ContainsFunc(e.audit.events, func(ev audit.Event) bool { return ev.Action == "acl_unavailable" && ev.ActorSubject == "dev" }) {
		t.Errorf("no acl_unavailable audit event for dev; events: %+v", e.audit.events)
	}
}

// createTeamWebhookAsDev writes a team webhook of probe-team as dev and
// returns a mux serving the receiver with walks wired.
func (e *aclResumeEnv) createTeamWebhookAsDev(t *testing.T, name string) *http.ServeMux {
	t.Helper()
	cfg := e.srv.cfg()
	e.srv.SetWebhookDefTool(&builtin.WebhookDef{Store: e.st, Cfg: cfg})
	ctx := tools.WithWebhookDefPolicy(e.authorCtx(t), tools.WebhookDefPolicyValue{Scopes: []string{"any"}, SelfName: "author"})
	res, err := e.srv.WebhookDef(ctx, json.RawMessage(`{"op":"create","name":"`+name+`","overlay":{"enabled":true,"delivery":"team","team":"probe-team","auth":{"kind":"hmac","header":"X-Test-Signature","signing_secret_env":"LOOMCYCLE_TEST_WH_SECRET"}}}`))
	if err != nil || res.IsError {
		t.Fatalf("create team webhook: %v %s", err, res.Text)
	}
	rec := webhook.New(webhook.Deps{
		Store: e.st, Cfg: cfg, Runner: e.srv, TeamWalks: e.srv,
		EnvAllowlist: map[string]bool{"LOOMCYCLE_TEST_WH_SECRET": true},
		Getenv: func(k string) string {
			if k == "LOOMCYCLE_TEST_WH_SECRET" {
				return ownerWebhookSecret
			}
			return ""
		},
	})
	mux := http.NewServeMux()
	rec.Mount(mux)
	return mux
}

// A team delivery's walk spawns its member narrowed by the webhook owner's
// list, recorded under the webhook transport.
func TestWebhookTeamDelivery_WalkMembersAreNarrowedByTheOwnersACL(t *testing.T) {
	e := aclResumeFixture(t, true, map[string][][]providers.Event{"worker": {probeTurn("tu_wt"), endTurn()}})
	e.withProbeTeam(t)
	mux := e.createTeamWebhookAsDev(t, "owned-team")
	w := deliverSigned(mux, "owned-team")
	if w.Code != http.StatusAccepted {
		t.Fatalf("delivery status = %d: %s", w.Code, w.Body.String())
	}
	var accepted struct {
		RunID string `json:"run_id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &accepted); err != nil || accepted.RunID == "" {
		t.Fatalf("delivery response %s: %v", w.Body.String(), err)
	}
	e.assertNarrowedByDev(t, e.waitProbes(t, 1)[0])
	assertMemberRecordsOwner(t, e.memberOf(t, accepted.RunID), "webhook")
}

// A deny on running the team refuses the delivery with the 403 an agent
// delivery's deny gets; an unreadable set, with a retryable 503. Neither
// starts a walk.
func TestWebhookTeamDelivery_OwnerRefusalsAnswerAsAnAgentDeliverys(t *testing.T) {
	for name, tc := range map[string]struct {
		refuse func(t *testing.T, e *aclResumeEnv)
		code   int
		body   string
	}{
		"deny": {func(t *testing.T, e *aclResumeEnv) { e.bindTeamRunDeny(t) }, http.StatusForbidden, "agent_not_allowed"},
		"unreadable": {func(_ *testing.T, e *aclResumeEnv) {
			e.srv.SetACLResolver(aclres.New(failingACLStore{e.st}, authz.ModeCompat, 0))
		}, http.StatusServiceUnavailable, ""},
	} {
		t.Run(name, func(t *testing.T) {
			e := aclResumeFixture(t, false, map[string][][]providers.Event{"worker": {endTurn()}})
			e.withProbeTeam(t)
			mux := e.createTeamWebhookAsDev(t, "refused-team")
			tc.refuse(t, e)
			w := deliverSigned(mux, "refused-team")
			if w.Code != tc.code || !strings.Contains(w.Body.String(), tc.body) {
				t.Fatalf("delivery status = %d: %s; want %d %s", w.Code, w.Body.String(), tc.code, tc.body)
			}
			if _, err := e.st.GetRunByAgentID(context.Background(), teamWalkAgentPrefix+"probe-team"); err == nil {
				t.Error("a refused delivery started a walk")
			}
			if n := e.prov.callsOf("worker"); n != 0 {
				t.Errorf("the refused delivery made %d model call(s)", n)
			}
		})
	}
}

// The owner's set is asked `spawn agent:` for each member, as for a child an
// agent of the owner's starts: a deny on spawning the member fails the walk
// before the member makes a model call.
func TestScheduleTeamFire_ADenyOnSpawningTheMemberFailsTheWalk(t *testing.T) {
	e := aclResumeFixture(t, false, map[string][][]providers.Event{"worker": {endTurn()}})
	e.withProbeTeam(t)
	defID := e.createTeamScheduleAsDev(t, "no-member")
	if _, err := e.st.AclDefPut(context.Background(), store.AclDefRow{TenantID: "acme", Name: "no-spawn",
		Rules: json.RawMessage(`[{"effect":"deny","actions":["spawn"],"resources":["agent:worker"]}]`)}); err != nil {
		t.Fatal(err)
	}
	bindACL(t, e.st, "no-spawn")
	e.startTeamScheduler(t)
	state := e.waitScheduleStatus(t, defID, "failed")
	if n := e.prov.callsOf("worker"); n != 0 {
		t.Errorf("the denied member made %d model call(s)", n)
	}
	if !slices.ContainsFunc(e.audit.events, func(ev audit.Event) bool {
		return ev.Action == "acl_denied" && ev.ActorSubject == "dev" && ev.TargetName == "agent:worker" && ev.AclAction == "spawn"
	}) {
		t.Errorf("no spawn acl_denied audit event for dev; events: %+v (last_error %q)", e.audit.events, state.LastError)
	}
}
