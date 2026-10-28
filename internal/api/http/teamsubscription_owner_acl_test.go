package http

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/aclres"
	"github.com/denn-gubsky/loomcycle/internal/audit"
	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/channels"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/steer"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/teamgraph"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// RFC EY, for a team SUBSCRIPTION: an armed subscription starts its team's
// walks with nobody on ctx, so the walk is bound by whoever promoted the team.
// The promote records that promoter's subject (teamdef_active.promoter_acl_owner,
// migration 0121) as a schedule records its author, and the sweep compiles the
// promoter's CURRENT set at each walk: `run team:<name>` is asked for it, and
// every member is narrowed by it and asked `spawn agent:` for it. Before, the
// promoter was recorded as two confinement bits only, and the walk's members
// ran on their agents' full grants whatever the promoter was bound to.

// subscribedTeamName is the team these tests arm: a Starter over the tenant
// channel "in" fanning each message out to worker, which probes its policy.
const subscribedTeamName = "probe-sub"

const subscribedProbeTeam = `{"entry":"wave","states":[` +
	`{"state":"wave","handler":{"kind":"starter","source":{"channel":"in"},` +
	`"fanout":{"agent":"worker","per":"message","max":1},"sink":{"channel":"out"}}},` +
	`{"state":"done","handler":{"kind":"terminal"}}],` +
	`"transitions":[{"from":"wave","to":"done","on":"success"}],` +
	`"channels":{"publish":["out"],"subscribe":["in"]}}`

// subscriptionEnv is the ACL fixture with a subscribed team authored (not
// promoted) in acme and the channels and publisher a sweep needs.
type subscriptionEnv struct {
	*aclResumeEnv
	defID string
	pub   *channels.StorePublisher
}

func subscriptionFixture(t *testing.T, bind bool, scripts map[string][][]providers.Event) *subscriptionEnv {
	t.Helper()
	e := aclResumeFixture(t, bind, scripts)
	// Before anything reads the config: the fixture's base config declares no
	// channels, and a subscribed team's source must be tenant- or global-scoped.
	e.srv.cfg().Channels = map[string]config.Channel{
		"in":  {Scope: "tenant", Semantic: "queue", MaxMessages: 100},
		"out": {Scope: "tenant", Semantic: "queue", MaxMessages: 100},
	}
	e.srv.SetSteerRegistry(steer.NewRegistry(0))
	e.srv.SetTeamDefTool(&builtin.TeamDef{Store: e.st})
	cbus := channels.NewBus()
	pub := &channels.StorePublisher{Store: e.st, Bus: cbus, Scheduler: channels.NewScheduler(cbus, 100)}
	e.srv.SetSystemPublisher(pub)
	e.srv.SetChannelBus(cbus)
	def, err := teamgraph.Parse([]byte(subscribedProbeTeam))
	if err != nil {
		t.Fatal(err)
	}
	// Written to the store directly: authoring is not under test, only who
	// promotes it.
	row, err := e.st.TeamDefCreate(context.Background(), store.TeamDefRow{
		DefID: "tdf_probe_sub", Name: subscribedTeamName, TenantID: "acme",
		Definition: json.RawMessage(subscribedProbeTeam), ContentSHA256: teamgraph.Sign(subscribedTeamName, def),
	})
	if err != nil {
		t.Fatal(err)
	}
	return &subscriptionEnv{aclResumeEnv: e, defID: row.DefID, pub: pub}
}

// promoteAsDev arms the team as dev, on the definition plane of acme.
func (e *subscriptionEnv) promoteAsDev(t *testing.T) {
	t.Helper()
	res, err := e.srv.TeamDef(e.authorCtx(t), json.RawMessage(`{"op":"promote","def_id":"`+e.defID+`"}`))
	if err != nil || res.IsError {
		t.Fatalf("promote as dev: %v %s", err, res.Text)
	}
}

// publish puts one message on the source.
func (e *subscriptionEnv) publish(t *testing.T) {
	t.Helper()
	if _, err := e.pub.PublishNow(context.Background(), "in", "acme", store.MemoryScopeTenant, "",
		json.RawMessage(`{"task":"go"}`), "_system", 0, 0); err != nil {
		t.Fatalf("publish: %v", err)
	}
}

// sweep runs one tick and returns how many walks it started.
func (e *subscriptionEnv) sweep(t *testing.T) int {
	t.Helper()
	started, err := e.srv.SweepTeamSubscriptions(context.Background())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	return started
}

// walkRun is the subscription walk's run row, or false when none started.
func (e *subscriptionEnv) walkRun(t *testing.T) (store.Run, bool) {
	t.Helper()
	run, err := e.st.GetRunByAgentID(context.Background(), teamWalkAgentPrefix+subscribedTeamName)
	return run, err == nil
}

// sourceStillHasWork reports whether the message the refused sweep found is
// still there for the next one: a refusal must not ack it.
func (e *subscriptionEnv) sourceStillHasWork(t *testing.T) bool {
	t.Helper()
	has, err := e.srv.sourceHasWork(context.Background(), teamSubscription{TenantID: "acme", Name: subscribedTeamName, Source: "in"})
	if err != nil {
		t.Fatal(err)
	}
	return has
}

// The promoter is recorded with the pointer, and a subscribed walk's member is
// narrowed by the promoter's list — Bash, which worker's own definition
// grants, is withheld — and records the promoter for a resume. This is the
// laundering the change closes: dev may not use Bash, and arming a team whose
// member holds Bash used not to get it back.
func TestTeamSubscription_WalkMembersAreNarrowedByThePromotersACL(t *testing.T) {
	e := subscriptionFixture(t, true, map[string][][]providers.Event{"worker": {probeTurn("tu_sub"), endTurn()}})
	e.promoteAsDev(t)
	e.publish(t)
	if n := e.sweep(t); n != 1 {
		t.Fatalf("the sweep started %d walk(s), want 1", n)
	}
	e.assertNarrowedByDev(t, e.waitProbes(t, 1)[0])
	walk, ok := e.walkRun(t)
	if !ok {
		t.Fatal("no walk run row")
	}
	// No transport: a sweep arrives on none of authz.Transports.
	assertMemberRecordsOwner(t, e.memberOf(t, walk.ID), "")
}

// A deny on running the team, bound to the promoter, refuses the walk: no
// walk row, no model call, the refusal audited against dev, and the message
// left on the source for the walk the next sweep may start.
func TestTeamSubscription_ADenyOnRunningTheTeamRefusesTheWalk(t *testing.T) {
	e := subscriptionFixture(t, false, map[string][][]providers.Event{"worker": {endTurn()}})
	e.promoteAsDev(t)
	if _, err := e.st.AclDefPut(context.Background(), store.AclDefRow{TenantID: "acme", Name: "no-sub-team",
		Rules: json.RawMessage(`[{"effect":"deny","actions":["run"],"resources":["team:` + subscribedTeamName + `"]}]`)}); err != nil {
		t.Fatal(err)
	}
	bindACL(t, e.st, "no-sub-team")
	e.publish(t)
	if n := e.sweep(t); n != 0 {
		t.Errorf("the sweep started %d walk(s) for a denied promoter", n)
	}
	if _, ok := e.walkRun(t); ok {
		t.Error("a denied promoter's subscription started a walk")
	}
	if n := e.prov.callsOf("worker"); n != 0 {
		t.Errorf("the refused walk made %d model call(s)", n)
	}
	if !slices.ContainsFunc(e.audit.events, func(ev audit.Event) bool {
		return ev.Action == "acl_denied" && ev.ActorSubject == "dev" && ev.TargetName == "team:"+subscribedTeamName
	}) {
		t.Errorf("no acl_denied audit event for dev on the team; events: %+v", e.audit.events)
	}
	if !e.sourceStillHasWork(t) {
		t.Error("the refused walk acked its source")
	}
}

// The promoter's set is asked `spawn agent:` for each member: a deny on
// spawning worker fails the walk before the member makes a model call.
func TestTeamSubscription_ADenyOnSpawningTheMemberFailsTheWalk(t *testing.T) {
	e := subscriptionFixture(t, false, map[string][][]providers.Event{"worker": {endTurn()}})
	e.promoteAsDev(t)
	if _, err := e.st.AclDefPut(context.Background(), store.AclDefRow{TenantID: "acme", Name: "no-spawn",
		Rules: json.RawMessage(`[{"effect":"deny","actions":["spawn"],"resources":["agent:worker"]}]`)}); err != nil {
		t.Fatal(err)
	}
	bindACL(t, e.st, "no-spawn")
	e.publish(t)
	e.sweep(t)
	if n := e.prov.callsOf("worker"); n != 0 {
		t.Errorf("the denied member made %d model call(s)", n)
	}
	if !slices.ContainsFunc(e.audit.events, func(ev audit.Event) bool {
		return ev.Action == "acl_denied" && ev.ActorSubject == "dev" && ev.TargetName == "agent:worker" && ev.AclAction == "spawn"
	}) {
		t.Errorf("no spawn acl_denied audit event for dev; events: %+v", e.audit.events)
	}
}

// A promoter whose set cannot be read starts no walk — acl_unavailable,
// audited — rather than walking on the members' full grants, and the message
// stays for a later sweep.
func TestTeamSubscription_UnreadablePromoterACLRefusesTheWalk(t *testing.T) {
	e := subscriptionFixture(t, true, map[string][][]providers.Event{"worker": {endTurn()}})
	e.promoteAsDev(t)
	e.srv.SetACLResolver(aclres.New(failingACLStore{e.st}, authz.ModeCompat, 0))
	e.publish(t)
	if n := e.sweep(t); n != 0 {
		t.Errorf("the sweep started %d walk(s) with an unreadable promoter set", n)
	}
	if _, ok := e.walkRun(t); ok {
		t.Error("an unreadable promoter set started a walk")
	}
	if n := e.prov.callsOf("worker"); n != 0 {
		t.Errorf("the refused walk made %d model call(s)", n)
	}
	if !slices.ContainsFunc(e.audit.events, func(ev audit.Event) bool { return ev.Action == "acl_unavailable" && ev.ActorSubject == "dev" }) {
		t.Errorf("no acl_unavailable audit event for dev; events: %+v", e.audit.events)
	}
	if !e.sourceStillHasWork(t) {
		t.Error("the refused walk acked its source")
	}
}

// A team promoted before the promoter was recorded (its pointer carries the
// two confinement bits and no owner) walks as it did — unnarrowed, as a
// schedule written before its acl_owner existed fires — and promoting it
// again binds the next walk by the new promoter's rules.
func TestTeamSubscription_APointerPromotedBeforeTheOwnerWasRecordedWalksAsBeforeUntilRepromoted(t *testing.T) {
	e := subscriptionFixture(t, true, map[string][][]providers.Event{
		"worker": {probeTurn("tu_legacy"), endTurn(), probeTurn("tu_repromoted"), endTurn()},
	})
	// The pointer as a build before migration 0121 left it.
	if _, err := e.st.SnapshotRestoreTeamDefActive(context.Background(), store.TeamDefActiveEntry{
		Name: subscribedTeamName, TenantID: "acme", DefID: e.defID, PromotedAt: time.Now(),
		Promoter: &store.TeamDefPromoter{ACLOwnerUnrecorded: true},
	}); err != nil {
		t.Fatal(err)
	}
	subs, err := e.srv.listTeamSubscriptions(context.Background())
	if err != nil || len(subs) != 1 || subs[0].Promoter == nil || !subs[0].Promoter.ACLOwnerUnrecorded {
		t.Fatalf("fixture: the sweep does not see an unrecorded owner: %+v %v", subs, err)
	}
	e.publish(t)
	if n := e.sweep(t); n != 1 {
		t.Fatalf("the legacy subscription started %d walk(s), want 1", n)
	}
	legacy := e.waitProbes(t, 1)[0]
	if legacy.ACL != nil || !slices.Contains(legacy.Tools, "Bash") {
		t.Errorf("the legacy subscription's member is not as it was before the owner existed: acl %+v tools %v", legacy.ACL, legacy.Tools)
	}

	e.promoteAsDev(t)
	e.publish(t)
	if n := e.sweep(t); n != 1 {
		t.Fatalf("the re-promoted subscription started %d walk(s), want 1", n)
	}
	got := aclNarrowingOf(e.waitProbes(t, 2)[1])
	if got.ACLSubject != "acme/dev" || slices.Contains(got.Tools, "Bash") {
		t.Errorf("after re-promoting, the member is not narrowed by dev's list: %+v", got)
	}
}

// A promote with no subject on ctx (open mode) records "nobody", not
// "unknown": the walk runs with no set, and the sweep does not warn the
// operator to promote again — that would not change anything.
func TestTeamSubscription_APromoteWithNoSubjectRecordsNobody(t *testing.T) {
	e := subscriptionFixture(t, true, map[string][][]providers.Event{"worker": {probeTurn("tu_open"), endTurn()}})
	ctx := tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{TenantID: "acme"})
	if res, err := e.srv.TeamDef(ctx, json.RawMessage(`{"op":"promote","def_id":"`+e.defID+`"}`)); err != nil || res.IsError {
		t.Fatalf("promote: %v %s", err, res.Text)
	}
	subs, err := e.srv.listTeamSubscriptions(context.Background())
	if err != nil || len(subs) != 1 || subs[0].Promoter == nil || subs[0].Promoter.ACLOwner != nil || subs[0].Promoter.ACLOwnerUnrecorded {
		t.Fatalf("the open-mode promote recorded %+v, want a recorded promoter with no owner (%v)", subs, err)
	}
	e.publish(t)
	if n := e.sweep(t); n != 1 {
		t.Fatalf("the sweep started %d walk(s), want 1", n)
	}
	if p := e.waitProbes(t, 1)[0]; p.ACL != nil {
		t.Errorf("a walk promoted by nobody carries a set: %+v", p.ACL)
	}
	if !e.srv.subUncaptured.first("acme\x00" + subscribedTeamName + "\x00acl_owner") {
		t.Error("the sweep warned to promote again a team whose promoter had no subject")
	}
}
