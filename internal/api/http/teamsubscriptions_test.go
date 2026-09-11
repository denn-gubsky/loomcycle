package http

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/teamgraph"
)

const subStarterGraph = `{"entry":"wave","states":[` +
	`{"state":"wave","handler":{"kind":"starter","source":{"channel":"c1"},` +
	`"fanout":{"agent":"reviewer","per":"message","max":2},"sink":{"channel":"c2"}}},` +
	`{"state":"done","handler":{"kind":"terminal"}}],` +
	`"transitions":[{"from":"wave","to":"done","on":"success"}],` +
	`"channels":{"publish":["c2"],"subscribe":["c1"]}}`

// seedTeam writes a team and optionally promotes it, bypassing the tool so a
// test can build states the tool would refuse to author.
func seedTeam(t *testing.T, srv *Server, name, defJSON string, promote, retired bool) string {
	t.Helper()
	ctx := context.Background()
	def, err := teamgraph.Parse([]byte(defJSON))
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	row, err := srv.store.TeamDefCreate(ctx, store.TeamDefRow{
		DefID: "tdf_" + name, Name: name, Definition: json.RawMessage(defJSON),
		ContentSHA256: teamgraph.Sign(name, def),
	})
	if err != nil {
		t.Fatalf("seed %s: %v", name, err)
	}
	if promote {
		if err := srv.store.TeamDefSetActive(ctx, "", name, row.DefID, "a_test"); err != nil {
			t.Fatalf("promote %s: %v", name, err)
		}
	}
	if retired {
		if err := srv.store.TeamDefSetRetired(ctx, row.DefID, true); err != nil {
			t.Fatalf("retire %s: %v", name, err)
		}
	}
	return row.DefID
}

func subNames(subs []teamSubscription) []string {
	out := make([]string, 0, len(subs))
	for _, s := range subs {
		out = append(out, s.Name)
	}
	return out
}

// TestListTeamSubscriptions_TheEnumerationIsTheArming is the property that
// replaces arm-on-promote / disarm-on-retire.
//
// The design warned that "an armed subscription for a retired workflow is a
// silent resource leak". There is nothing armed to leak: each tick asks the
// store what is promoted, so an unpromoted, retired or non-Starter team simply
// is not in the answer. If this test ever passes for one of those, the sweep
// has started driving something nobody asked it to.
func TestListTeamSubscriptions_TheEnumerationIsTheArming(t *testing.T) {
	srv, cleanup := channelFanFixture(t)
	defer cleanup()

	seedTeam(t, srv, "promoted", subStarterGraph, true, false)
	seedTeam(t, srv, "unpromoted", subStarterGraph, false, false) // authored, never promoted
	seedTeam(t, srv, "retired", subStarterGraph, true, true)      // taken out of service
	seedTeam(t, srv, "not-a-starter", `{"entry":"a","states":[`+  // an ordinary team
		`{"state":"a","handler":{"kind":"agent","agent":"x"}},`+
		`{"state":"done","handler":{"kind":"terminal"}}],`+
		`"transitions":[{"from":"a","to":"done","on":"success"}]}`, true, false)

	// A malformed def: kind=agent but carrying a source. Validation refuses to
	// author this, so it can only arrive from an older row or a direct write —
	// and the KIND check is the only thing standing between it and being driven
	// as though it were a Starter.
	seedTeam(t, srv, "agent-with-source", `{"entry":"a","states":[`+
		`{"state":"a","handler":{"kind":"agent","agent":"x","source":{"channel":"c1"}}},`+
		`{"state":"done","handler":{"kind":"terminal"}}],`+
		`"transitions":[{"from":"a","to":"done","on":"success"}]}`, true, false)

	subs, err := srv.listTeamSubscriptions(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	got := subNames(subs)
	if len(got) != 1 || got[0] != "promoted" {
		t.Fatalf("subscriptions = %v, want just [promoted]", got)
	}
	if subs[0].Source != "c1" {
		t.Errorf("source = %q, want c1 (the ENTRY starter's channel)", subs[0].Source)
	}
}

// TestListTeamSubscriptions_RetiringDisarms: the same team, before and after
// retire. This is the leak the design was worried about, made executable.
func TestListTeamSubscriptions_RetiringDisarms(t *testing.T) {
	srv, cleanup := channelFanFixture(t)
	defer cleanup()
	defID := seedTeam(t, srv, "triage", subStarterGraph, true, false)

	subs, _ := srv.listTeamSubscriptions(context.Background())
	if len(subs) != 1 {
		t.Fatalf("want 1 subscription before retire, got %v", subNames(subs))
	}
	if err := srv.store.TeamDefSetRetired(context.Background(), defID, true); err != nil {
		t.Fatalf("retire: %v", err)
	}
	subs, _ = srv.listTeamSubscriptions(context.Background())
	if len(subs) != 0 {
		t.Errorf("a RETIRED team is still driven: %v — retire must take a workflow out of service", subNames(subs))
	}
}

// TestSubscriptionScope_RefusesAPerUserSource: a sweep has no user and no
// agent. Driving an agent- or user-scoped channel would mean picking ONE of its
// per-user queues and silently ignoring every other — a workflow that looks
// like it is running while most of its work accumulates unread.
func TestSubscriptionScope_RefusesAPerUserSource(t *testing.T) {
	for _, tc := range []struct {
		scope   string
		wantErr string
	}{
		{"global", ""},
		{"tenant", ""},
		{"user", "separate queue per user"},
		{"agent", "separate queue per agent"},
		{"nonsense", "unknown scope"},
	} {
		gotScope, gotID, err := subscriptionScope(tc.scope, "c1")
		if tc.wantErr == "" {
			if err != nil {
				t.Errorf("scope %q: unexpected refusal %v", tc.scope, err)
			}
			if gotID != "" {
				t.Errorf("scope %q: scope_id = %q, want empty (a shared queue)", tc.scope, gotID)
			}
			if string(gotScope) != tc.scope {
				t.Errorf("scope %q resolved to %q", tc.scope, gotScope)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
			t.Errorf("scope %q: err = %v, want one mentioning %q", tc.scope, err, tc.wantErr)
		}
	}
}

// TestSweepTeamSubscriptions_IdleSourceStartsNothing: the sweep must cost a
// query, not a run. An autonomous driver that started a walk per tick on an
// empty channel would burn tokens forever on a workflow with nothing to do.
func TestSweepTeamSubscriptions_IdleSourceStartsNothing(t *testing.T) {
	srv, cleanup := channelFanFixture(t)
	defer cleanup()
	seedTeam(t, srv, "triage", subStarterGraph, true, false)

	started, err := srv.SweepTeamSubscriptions(context.Background())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if started != 0 {
		t.Errorf("started %d walk(s) on an EMPTY source, want 0", started)
	}
}

// TestSweepTeamSubscriptions_OneBadTeamDoesNotStopTheRest: a team whose source
// is undeclared fails its own drive. The sweep must keep going — one broken
// workflow silencing every other one is how an operator loses a fleet to a
// typo.
func TestSweepTeamSubscriptions_OneBadTeamDoesNotStopTheRest(t *testing.T) {
	srv, cleanup := channelFanFixture(t)
	defer cleanup()
	// c1 is declared by the fixture; "nowhere" is not.
	seedTeam(t, srv, "broken", strings.Replace(subStarterGraph, `"channel":"c1"`, `"channel":"nowhere"`, 1), true, false)
	seedTeam(t, srv, "healthy", subStarterGraph, true, false)

	if _, err := srv.SweepTeamSubscriptions(context.Background()); err != nil {
		t.Fatalf("one undeclared source must not fail the whole sweep: %v", err)
	}
	subs, _ := srv.listTeamSubscriptions(context.Background())
	if len(subs) != 2 {
		t.Errorf("both teams should still be enumerated: %v", subNames(subs))
	}
}

// TestDriveTeamSubscription_WorkOnTheSourceStartsAWalk closes the loop the
// other tests leave open: a message on the source must actually reach the run
// path. With no TeamDef tool wired the run refuses by name, which is exactly
// the evidence wanted — the sweep got past the peek and tried to walk.
func TestDriveTeamSubscription_WorkOnTheSourceStartsAWalk(t *testing.T) {
	srv, cleanup := channelFanFixture(t)
	defer cleanup()
	seedTeam(t, srv, "triage", subStarterGraph, true, false)
	ctx := context.Background()

	subs, _ := srv.listTeamSubscriptions(ctx)
	if len(subs) != 1 {
		t.Fatalf("want one subscription, got %v", subNames(subs))
	}

	// Empty source: no attempt at all.
	started, err := srv.driveTeamSubscription(ctx, subs[0])
	if started || err != nil {
		t.Fatalf("empty source: started=%v err=%v, want false/nil", started, err)
	}

	// Put work on it. c1 is scope:global in the fixture, so the sweep can read
	// it without a user.
	if _, err := srv.systemPublisher.PublishNow(ctx, "c1", "",
		store.MemoryScopeGlobal, "", json.RawMessage(`{"pr":1}`), "_system", 0, 0); err != nil {
		t.Fatalf("publish: %v", err)
	}

	started, err = srv.driveTeamSubscription(ctx, subs[0])
	if !started {
		t.Fatal("a message on the source did not start a walk — the peek gate never opened")
	}
	if err == nil || !strings.Contains(err.Error(), "SetTeamDefTool") {
		t.Errorf("err = %v, want the not-configured refusal proving the run path was reached", err)
	}

	// And the peek did NOT consume it — the walk owns the cursor, and a sweep
	// that ate the message would start a wave with nothing to work on.
	msgs, perr := srv.store.ChannelPeek(ctx, "", "c1", store.MemoryScopeGlobal, "", "", 10)
	if perr != nil || len(msgs) != 1 {
		t.Errorf("after the sweep the source holds %d message(s) (err=%v), want 1 — the peek must not consume", len(msgs), perr)
	}
}

// TestSubscriptionBackoff_PacesAFailingTeam: a failed walk does NOT ack its
// source, so without a backoff the same message drives the same failure on the
// very next tick — a poison message burning tokens at tick rate until someone
// notices. This is the guard an autonomous driver needs and a human-triggered
// run does not.
func TestSubscriptionBackoff_PacesAFailingTeam(t *testing.T) {
	var b subscriptionBackoff // the ZERO value must work — Servers are built without a constructor here
	if !b.ready("t1") {
		t.Fatal("a team with no history must be ready")
	}
	b.record("t1", context.DeadlineExceeded)
	if b.ready("t1") {
		t.Error("a team that just failed was immediately retried")
	}
	// Another team is unaffected — one broken workflow must not pace the rest.
	if !b.ready("t2") {
		t.Error("a failure on one team held back another")
	}
	// Success clears it, so a fixed workflow recovers without a restart.
	b.record("t1", nil)
	if !b.ready("t1") {
		t.Error("a success did not clear the backoff")
	}
	// And it is bounded: repeated failures must not grow without limit.
	for i := 0; i < 40; i++ {
		b.record("t3", context.DeadlineExceeded)
	}
	b.mu.Lock()
	wait := time.Until(b.until["t3"])
	b.mu.Unlock()
	if wait > maxSubscriptionBackoff {
		t.Errorf("backoff grew to %s, past the %s cap — a team must stay recoverable", wait, maxSubscriptionBackoff)
	}
}

// TestListTeamSubscriptions_AnyReplicaCanDriveAnyTeam: the enumeration is
// derived entirely from the shared store, so every replica sees the same
// driveable set. That is what makes losing a replica a non-event — there is no
// ownership to hand over, and the next replica to tick simply picks the team
// up. Two Servers over one store stand in for two replicas.
func TestListTeamSubscriptions_AnyReplicaCanDriveAnyTeam(t *testing.T) {
	srvA, cleanup := channelFanFixture(t)
	defer cleanup()
	seedTeam(t, srvA, "triage", subStarterGraph, true, false)

	// A second Server over the SAME store — what a second replica is.
	srvB := &Server{store: srvA.store, cfgHolder: srvA.cfgHolder}

	subsA, err := srvA.listTeamSubscriptions(context.Background())
	if err != nil {
		t.Fatalf("A: %v", err)
	}
	subsB, err := srvB.listTeamSubscriptions(context.Background())
	if err != nil {
		t.Fatalf("B: %v", err)
	}
	if len(subsA) != 1 || len(subsB) != 1 || subsA[0].DefID != subsB[0].DefID {
		t.Fatalf("replicas disagree on what is driveable: A=%v B=%v", subNames(subsA), subNames(subsB))
	}
}
