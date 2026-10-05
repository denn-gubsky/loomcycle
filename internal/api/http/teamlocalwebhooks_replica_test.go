package http

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/channels"
	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/runner"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// These tests run a second server over the harness's store, as a second
// replica: a team's own webhook is accepted on a replica with no walk of the
// team, through the walk's lease in the store, and only while that lease is
// live.

// replica is another server over h's store. Its channel writes go to h's bus,
// as a cluster's bus fan-out carries them, so a walk on h hears them.
func (h *hookHarness) replica() (*Server, *http.ServeMux) {
	h.t.Helper()
	other := New(h.srv.cfg(), &stubResolver{p: h.prov}, nil, concurrency.New(8, 8, 5*time.Second), h.st)
	other.SetSystemPublisher(&channels.StorePublisher{Store: h.st, Bus: h.pub.Bus, Defs: other.ChannelWriteDef})
	return other, receiverFor(other)
}

// walkRun opens a running run for user in acme, standing for a walk's own.
func (h *hookHarness) walkRun(user string) string {
	h.t.Helper()
	ctx := context.Background()
	sess, err := h.st.CreateSession(ctx, "acme", "team:hooked", user)
	if err != nil {
		h.t.Fatal(err)
	}
	run, err := h.st.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "team:hooked", UserID: user, TenantID: "acme"})
	if err != nil {
		h.t.Fatal(err)
	}
	return run.ID
}

// A walk running on one replica takes a delivery sent to another: the walk's
// Starter reads it, and once the walk is over the other replica answers as
// for a webhook that does not exist.
func TestTeamLocalWebhook_AnotherReplicaAcceptsWhileTheWalkRuns(t *testing.T) {
	h := newHookHarness(t)
	h.seed("tdf_hooked_1", "hooked", hookedTeamJSON("tenant", 30000, ""))
	_, otherMux := h.replica()

	runID := h.detach(acmeUser("alice"), "hooked")
	h.waitArmed(1)
	w := deliverToTeam(otherMux, hookPath, `{"pr": 7}`, "d-1", true)
	if w.Code != http.StatusAccepted || !strings.Contains(w.Body.String(), `"channel":"./events"`) {
		t.Fatalf("a signed delivery to the replica not running the walk: %d %s", w.Code, w.Body.String())
	}
	if st := h.waitRun(runID); st != store.RunCompleted {
		t.Fatalf("the walk ended %s, want completed (its Starter fed by the other replica's delivery)", st)
	}
	got := h.stored("_team/hooked/events", store.MemoryScopeTenant, "")
	if len(got) != 1 || string(got[0].Payload) != `{"pr": 7}` || got[0].PublishedByUserID != "alice" {
		t.Fatalf("the team's channel holds %+v, want the raw body once, attributed to the walk's user", got)
	}
	if after := deliverToTeam(otherMux, hookPath, `{"pr": 8}`, "d-2", true); !sameResponse(after, notFound(t, otherMux)) {
		t.Errorf("after the walk ended the other replica answers %d %s, want the unknown-webhook answer", after.Code, after.Body.String())
	}
	if got := h.stored("_team/hooked/events", store.MemoryScopeTenant, ""); len(got) != 1 {
		t.Errorf("a delivery after the walk ended landed: %d message(s)", len(got))
	}
}

// A live walk's lease is renewed every heartbeat, so it outlasts its first
// expiry; the walk's disarm deletes it, so another replica stops answering at
// once — while the walk's run is still recorded as running.
func TestTeamLocalWebhook_ALeaseIsRenewedWhileTheWalkRunsAndDeletedByItsDisarm(t *testing.T) {
	h := newHookHarness(t)
	raw := hookedTeamJSON("tenant", 30000, "")
	sc := h.seed("tdf_hooked_1", "hooked", raw)
	clock := newFakeClock()
	h.srv.walkClock = clock
	h.srv.walkHeartbeatEvery = 10 * time.Second // a lease lapses 30s after its last renewal
	other, otherMux := h.replica()
	other.walkClock = clock

	disarm, err := h.srv.armWalkTriggers(schedWalkCtx(sc, h.walkRun("alice"), "alice"), mustTeamDef(t, raw))
	if err != nil {
		t.Fatal(err)
	}
	defer disarm()
	clock.waitAfters(t, 1)
	for i := 1; i <= 5; i++ { // 50s: past the first lease's end
		clock.Advance(10 * time.Second)
		clock.waitAfters(t, i+1) // the renewal is written before the next wait
	}
	if w := deliverToTeam(otherMux, hookPath, `{"n":1}`, "d-1", true); w.Code != http.StatusAccepted {
		t.Fatalf("50s into a walk renewing its lease: %d %s", w.Code, w.Body.String())
	}
	disarm()
	if w := deliverToTeam(otherMux, hookPath, `{"n":2}`, "d-2", true); !sameResponse(w, notFound(t, otherMux)) {
		t.Errorf("after the disarm, its run still running: %d %s, want the unknown-webhook answer", w.Code, w.Body.String())
	}
	if got := h.stored("_team/hooked/events", store.MemoryScopeTenant, ""); len(got) != 1 {
		t.Errorf("%d message(s), want the one delivery made while the lease was held", len(got))
	}
}

// A lease nobody renews — its walk's replica crashed — stops counting when it
// lapses; one whose walk run is over never counts, nor one of a retired
// version. Each answers with the unknown-webhook bytes, as does every other
// miss on a replica with no walk of the team.
func TestTeamLocalWebhook_ALapsedEndedOrRetiredLeaseIsNotFound(t *testing.T) {
	h := newHookHarness(t)
	sc := h.seed("tdf_hooked_1", "hooked", hookedTeamJSON("tenant", 30000, ""))
	other, otherMux := h.replica()
	clock := newFakeClock()
	other.walkClock = clock
	want := notFound(t, otherMux)
	ctx := context.Background()
	plant := func(runID string, ttl time.Duration) {
		t.Helper()
		if err := h.st.TeamWebhookArmPut(ctx, []store.TeamWebhookArm{{
			TenantID: "acme", Team: "hooked", Name: "github", WalkRunID: runID, DefID: sc.DefID,
			UserID: "alice", ArmedAt: clock.Now(), ExpiresAt: clock.Now().Add(ttl),
		}}); err != nil {
			t.Fatal(err)
		}
	}

	crashed := h.walkRun("alice")
	plant(crashed, 90*time.Second)
	if w := deliverToTeam(otherMux, hookPath, `{"n":1}`, "d-1", true); w.Code != http.StatusAccepted {
		t.Fatalf("while the crashed walk's lease is live: %d %s", w.Code, w.Body.String())
	}
	clock.Advance(90 * time.Second)
	for _, sign := range []bool{true, false} {
		if w := deliverToTeam(otherMux, hookPath, `{"n":2}`, "d-2", sign); !sameResponse(w, want) {
			t.Errorf("a lapsed lease (signed=%v): %d %s", sign, w.Code, w.Body.String())
		}
	}

	ended := h.walkRun("alice")
	plant(ended, time.Hour)
	if err := h.st.FinishRun(ctx, ended, store.RunCompleted, "", store.Usage{}, ""); err != nil {
		t.Fatal(err)
	}
	if w := deliverToTeam(otherMux, hookPath, `{"n":3}`, "d-3", true); !sameResponse(w, want) {
		t.Errorf("the lease of a walk whose run is over: %d %s", w.Code, w.Body.String())
	}

	live := h.walkRun("alice")
	plant(live, time.Hour)
	for what, path := range map[string]string{
		"unknown webhook":   "/v1/_teams/acme/hooked/webhooks/gitlab",
		"another tenant":    "/v1/_teams/globex/hooked/webhooks/github",
		"the shared tenant": "/v1/_teams/hooked/webhooks/github",
		"unknown team":      "/v1/_teams/acme/nohook/webhooks/github",
	} {
		if w := deliverToTeam(otherMux, path, `{}`, "", true); !sameResponse(w, want) {
			t.Errorf("%s: %d %s", what, w.Code, w.Body.String())
		}
	}
	if err := h.st.TeamDefSetRetired(ctx, sc.DefID, true); err != nil {
		t.Fatal(err)
	}
	if w := deliverToTeam(otherMux, hookPath, `{"n":4}`, "d-4", true); !sameResponse(w, want) {
		t.Errorf("a live lease of a retired version: %d %s", w.Code, w.Body.String())
	}
	if got := h.stored("_team/hooked/events", store.MemoryScopeTenant, ""); len(got) != 1 {
		t.Errorf("%d message(s), want only the delivery made under the live lease", len(got))
	}
}

// One message per delivery, whichever replica takes it: two replicas each
// running a walk and a third running none. The third publishes as the
// earliest-armed walk.
func TestTeamLocalWebhook_OneMessagePerDeliveryAcrossReplicas(t *testing.T) {
	h := newHookHarness(t)
	raw := hookedTeamJSON("tenant", 30000, "")
	sc := h.seed("tdf_hooked_1", "hooked", raw)
	def := mustTeamDef(t, raw)
	b, bMux := h.replica()
	_, cMux := h.replica()

	disarmA, err := h.srv.armWalkTriggers(schedWalkCtx(sc, h.walkRun("alice"), "alice"), def)
	if err != nil {
		t.Fatal(err)
	}
	defer disarmA()
	b.ReceiveTeamWebhooks()
	disarmB, err := b.armWalkTriggers(schedWalkCtx(sc, h.walkRun("bob"), "bob"), def)
	if err != nil {
		t.Fatal(err)
	}
	defer disarmB()

	for i, mux := range []*http.ServeMux{h.mux, bMux, cMux} {
		did := "d-" + string(rune('1'+i))
		if w := deliverToTeam(mux, hookPath, `{"n":"`+did+`"}`, did, true); w.Code != http.StatusAccepted {
			t.Fatalf("delivery %s: %d %s", did, w.Code, w.Body.String())
		}
	}
	got := h.stored("_team/hooked/events", store.MemoryScopeTenant, "")
	if len(got) != 3 {
		t.Fatalf("%d message(s) for 3 deliveries, want one each", len(got))
	}
	by := map[string]string{}
	for _, m := range got {
		by[string(m.Payload)] = m.PublishedByUserID
	}
	want := map[string]string{`{"n":"d-1"}`: "alice", `{"n":"d-2"}`: "bob", `{"n":"d-3"}`: "alice"}
	for body, user := range want {
		if by[body] != user {
			t.Errorf("%s attributed to %q, want %q (its replica's walk; the earliest-armed one on a replica with none)", body, by[body], user)
		}
	}
}

// A user-scoped channel's rules hold for a delivery taken through a lease: it
// is filed under the user it names, and one naming none is refused.
func TestTeamLocalWebhook_ALeasedUserChannelFilesUnderThePayloadsUser(t *testing.T) {
	h := newHookHarness(t)
	raw := hookedTeamJSON("user", 30000, `,"payload_mapping":{"user_id":"$.sender.login"}`)
	sc := h.seed("tdf_hooked_1", "hooked", raw)
	disarm, err := h.srv.armWalkTriggers(schedWalkCtx(sc, h.walkRun("alice"), "alice"), mustTeamDef(t, raw))
	if err != nil {
		t.Fatal(err)
	}
	defer disarm()
	_, otherMux := h.replica()

	if w := deliverToTeam(otherMux, hookPath, `{"sender":{"login":"bob"}}`, "d-1", true); w.Code != http.StatusAccepted {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if got := h.stored("_team/hooked/events", store.MemoryScopeUser, "bob"); len(got) != 1 || got[0].PublishedByUserID != "bob" {
		t.Errorf("bob's delivery under bob: %+v", got)
	}
	if w := deliverToTeam(otherMux, hookPath, `{"sender":{}}`, "d-2", true); w.Code != http.StatusBadRequest {
		t.Errorf("a delivery naming no user, to a user-scoped channel: %d %s", w.Code, w.Body.String())
	}
	if got := h.stored("_team/hooked/events", store.MemoryScopeUser, "alice"); len(got) != 0 {
		t.Errorf("a delivery naming no user was filed under the walk's user: %+v", got)
	}
}

// A delivery resolved through a lease and published after the walk's disarm
// finds the webhook gone.
func TestTeamLocalWebhook_ALeasedDeliveryPublishesNothingAfterTheDisarm(t *testing.T) {
	h := newHookHarness(t)
	raw := hookedTeamJSON("tenant", 30000, "")
	sc := h.seed("tdf_hooked_1", "hooked", raw)
	disarm, err := h.srv.armWalkTriggers(schedWalkCtx(sc, h.walkRun("alice"), "alice"), mustTeamDef(t, raw))
	if err != nil {
		t.Fatal(err)
	}
	other, _ := h.replica()
	hook, ok := other.ResolveTeamWebhook(context.Background(), "acme", "hooked", "github")
	if !ok {
		t.Fatal("the leased webhook does not resolve on the other replica")
	}
	disarm()
	if err := hook.Publish(context.Background(), "", json.RawMessage(`{"late":true}`)); !errors.Is(err, runner.ErrTeamWebhookGone) {
		t.Fatalf("a publish after the disarm: %v, want ErrTeamWebhookGone", err)
	}
	if got := h.stored("_team/hooked/events", store.MemoryScopeTenant, ""); len(got) != 0 {
		t.Errorf("%d message(s) landed after the disarm", len(got))
	}
}
