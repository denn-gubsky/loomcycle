package http

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
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
	if err := hook.Publish(context.Background(), "", []string{"late"}, json.RawMessage(`{"late":true}`)); !errors.Is(err, runner.ErrTeamWebhookGone) {
		t.Fatalf("a publish after the disarm: %v, want ErrTeamWebhookGone", err)
	}
	if got := h.stored("_team/hooked/events", store.MemoryScopeTenant, ""); len(got) != 0 {
		t.Errorf("%d message(s) landed after the disarm", len(got))
	}
}

// deliverStripe POSTs body to path, signed in the Stripe envelope at instant
// ts, with delivery id did when not "".
func deliverStripe(mux *http.ServeMux, path, body, did string, ts time.Time) *httptest.ResponseRecorder {
	t := strconv.FormatInt(ts.Unix(), 10)
	mac := hmac.New(sha256.New, []byte(hookSecret))
	mac.Write([]byte(t + "." + body))
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader([]byte(body)))
	req.Header.Set("X-Hub-Signature-256", "t="+t+", v1="+hex.EncodeToString(mac.Sum(nil)))
	if did != "" {
		req.Header.Set("X-Delivery", did)
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	return w
}

// A delivery accepted on one replica is a duplicate on every other — and on
// the same one after a restart, with its in-process replay guard empty — and
// under a new delivery id too, as its signature covers only the body: one
// message, and an idempotent ack for each replay.
func TestTeamLocalWebhook_ADeliveryIsAcceptedOnceAcrossReplicasAndRestarts(t *testing.T) {
	h := newHookHarness(t)
	raw := hookedTeamJSON("tenant", 30000, "")
	sc := h.seed("tdf_hooked_1", "hooked", raw)
	disarm, err := h.srv.armWalkTriggers(schedWalkCtx(sc, h.walkRun("alice"), "alice"), mustTeamDef(t, raw))
	if err != nil {
		t.Fatal(err)
	}
	defer disarm()
	_, otherMux := h.replica()

	if w := deliverToTeam(h.mux, hookPath, `{"pr":1}`, "d-1", true); w.Code != http.StatusAccepted {
		t.Fatalf("the first delivery: %d %s", w.Code, w.Body.String())
	}
	for what, w := range map[string]*httptest.ResponseRecorder{
		"replayed to the other replica":         deliverToTeam(otherMux, hookPath, `{"pr":1}`, "d-1", true),
		"replayed to a restarted receiver":      deliverToTeam(receiverFor(h.srv), hookPath, `{"pr":1}`, "d-1", true),
		"replayed elsewhere under a new id":     deliverToTeam(otherMux, hookPath, `{"pr":1}`, "d-other", true),
		"replayed under a new id after restart": deliverToTeam(receiverFor(h.srv), hookPath, `{"pr":1}`, "d-new", true),
	} {
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"deduped":"true"`) {
			t.Errorf("%s: %d %s, want the idempotent ack", what, w.Code, w.Body.String())
		}
	}
	if got := h.stored("_team/hooked/events", store.MemoryScopeTenant, ""); len(got) != 1 {
		t.Errorf("%d message(s) for one delivery and its replays, want 1", len(got))
	}
}

// The same delivery sent to two replicas at once publishes once.
func TestTeamLocalWebhook_ConcurrentDuplicatesOnTwoReplicasPublishOnce(t *testing.T) {
	h := newHookHarness(t)
	raw := hookedTeamJSON("tenant", 30000, "")
	sc := h.seed("tdf_hooked_1", "hooked", raw)
	disarm, err := h.srv.armWalkTriggers(schedWalkCtx(sc, h.walkRun("alice"), "alice"), mustTeamDef(t, raw))
	if err != nil {
		t.Fatal(err)
	}
	defer disarm()
	_, otherMux := h.replica()

	const racers = 8
	codes := make(chan int, racers)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < racers; i++ {
		mux := h.mux
		if i%2 == 1 {
			mux = otherMux
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			codes <- deliverToTeam(mux, hookPath, `{"pr":1}`, "d-1", true).Code
		}()
	}
	close(start)
	wg.Wait()
	close(codes)
	accepted := 0
	for c := range codes {
		switch c {
		case http.StatusAccepted:
			accepted++
		case http.StatusOK:
		default:
			t.Errorf("a racing duplicate answered %d", c)
		}
	}
	if accepted != 1 {
		t.Errorf("%d of %d racing duplicates accepted, want exactly 1", accepted, racers)
	}
	if got := h.stored("_team/hooked/events", store.MemoryScopeTenant, ""); len(got) != 1 {
		t.Errorf("%d message(s), want 1", len(got))
	}
}

// A Stripe-style signature outside the replay window is the opaque 401 on
// both paths — the replica running the walk and one answering from its lease —
// and a fresh one is accepted on both.
func TestTeamLocalWebhook_AStaleSignatureIsRefusedOnEveryReplica(t *testing.T) {
	h := newHookHarness(t)
	raw := hookedTeamJSON("tenant", 30000, "")
	sc := h.seed("tdf_hooked_1", "hooked", raw)
	disarm, err := h.srv.armWalkTriggers(schedWalkCtx(sc, h.walkRun("alice"), "alice"), mustTeamDef(t, raw))
	if err != nil {
		t.Fatal(err)
	}
	defer disarm()
	_, otherMux := h.replica()

	for what, mux := range map[string]*http.ServeMux{"the walk's replica": h.mux, "another replica": otherMux} {
		if w := deliverStripe(mux, hookPath, `{"stale":"`+what+`"}`, "", time.Now().Add(-10*time.Minute)); w.Code != http.StatusUnauthorized || w.Body.String() != "{\"error\":\"unauthorized\"}\n" {
			t.Errorf("%s, a signature 10 minutes old: %d %s", what, w.Code, w.Body.String())
		}
		if w := deliverStripe(mux, hookPath, `{"fresh":"`+what+`"}`, "", time.Now()); w.Code != http.StatusAccepted {
			t.Errorf("%s, a fresh signature: %d %s", what, w.Code, w.Body.String())
		}
	}
	if got := h.stored("_team/hooked/events", store.MemoryScopeTenant, ""); len(got) != 2 {
		t.Errorf("%d message(s), want the two fresh deliveries", len(got))
	}
}

// A delivery whose publish fails is not remembered as accepted: the sender's
// retry of it publishes.
func TestTeamLocalWebhook_AFailedPublishLeavesTheDeliveryRetryable(t *testing.T) {
	h := newHookHarness(t)
	raw := hookedTeamJSON("tenant", 30000, "")
	sc := h.seed("tdf_hooked_1", "hooked", raw)
	disarm, err := h.srv.armWalkTriggers(schedWalkCtx(sc, h.walkRun("alice"), "alice"), mustTeamDef(t, raw))
	if err != nil {
		t.Fatal(err)
	}
	defer disarm()
	// A replica whose channel writer is not wired yet: its publish fails.
	other := New(h.srv.cfg(), &stubResolver{p: h.prov}, nil, concurrency.New(8, 8, 5*time.Second), h.st)
	otherMux := receiverFor(other)
	if w := deliverToTeam(otherMux, hookPath, `{"pr":1}`, "d-1", true); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("a publish that fails: %d %s", w.Code, w.Body.String())
	}
	if w := deliverToTeam(h.mux, hookPath, `{"pr":1}`, "d-1", true); w.Code != http.StatusAccepted {
		t.Errorf("the sender's retry, to another replica: %d %s", w.Code, w.Body.String())
	}
	if got := h.stored("_team/hooked/events", store.MemoryScopeTenant, ""); len(got) != 1 {
		t.Errorf("%d message(s), want the retried delivery once", len(got))
	}
}

// Only a request that is authenticated and would publish holds a delivery's
// keys: a badly signed one carrying a genuine delivery's id, and a signed one
// refused for naming no user, leave nothing that drops the genuine delivery.
func TestTeamLocalWebhook_ARefusedRequestHoldsNoKey(t *testing.T) {
	h := newHookHarness(t)
	raw := hookedTeamJSON("user", 30000, `,"payload_mapping":{"user_id":"$.sender.login"}`)
	sc := h.seed("tdf_hooked_1", "hooked", raw)
	disarm, err := h.srv.armWalkTriggers(schedWalkCtx(sc, h.walkRun("alice"), "alice"), mustTeamDef(t, raw))
	if err != nil {
		t.Fatal(err)
	}
	defer disarm()
	_, otherMux := h.replica()
	req := httptest.NewRequest(http.MethodPost, hookPath, strings.NewReader(`{"sender":{"login":"bob"},"n":"d-1"}`))
	req.Header.Set("X-Hub-Signature-256", "t="+strconv.FormatInt(time.Now().Unix(), 10)+", v1="+strings.Repeat("0", 64))
	req.Header.Set("X-Delivery", "d-1")
	w := httptest.NewRecorder()
	otherMux.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("badly signed, carrying the genuine id: %d %s", w.Code, w.Body.String())
	}
	if w := deliverStripe(otherMux, hookPath, `{"sender":{}}`, "d-2", time.Now()); w.Code != http.StatusBadRequest {
		t.Fatalf("signed, naming no user: %d %s", w.Code, w.Body.String())
	}
	for _, did := range []string{"d-1", "d-2"} {
		// Two genuine deliveries: distinct bodies, as a signed payload seen
		// twice is a replay whatever its id.
		body := `{"sender":{"login":"bob"},"n":"` + did + `"}`
		if w := deliverStripe(h.mux, hookPath, body, did, time.Now().Add(-time.Second)); w.Code != http.StatusAccepted {
			t.Errorf("the genuine delivery %s after a refused request with its id: %d %s", did, w.Code, w.Body.String())
		}
	}
	if got := h.stored("_team/hooked/events", store.MemoryScopeUser, "bob"); len(got) != 2 {
		t.Errorf("%d message(s), want the two genuine deliveries", len(got))
	}
}

// A body-only signature never expires, and its sender's id is not signed: a
// capture replayed once its key has lapsed must not be able to file itself
// under the id of a delivery not yet sent. The genuine delivery is accepted.
func TestTeamLocalWebhook_AReplayCannotHoldAnUnsignedDeliveryID(t *testing.T) {
	h := newHookHarness(t)
	raw := hookedTeamJSON("tenant", 30000, "")
	sc := h.seed("tdf_hooked_1", "hooked", raw)
	clock := newFakeClock()
	h.srv.walkClock = clock
	other, _ := h.replica()
	other.walkClock = clock
	disarm, err := h.srv.armWalkTriggers(schedWalkCtx(sc, h.walkRun("alice"), "alice"), mustTeamDef(t, raw))
	if err != nil {
		t.Fatal(err)
	}
	defer disarm()

	if w := deliverToTeam(receiverFor(other), hookPath, `{"pr":1}`, "d-1", true); w.Code != http.StatusAccepted {
		t.Fatalf("the captured delivery, first sent: %d %s", w.Code, w.Body.String())
	}
	clock.Advance(25 * time.Hour)
	// The lease is renewed on this clock too; let it catch up.
	clock.waitAfters(t, 2)
	if w := deliverToTeam(receiverFor(other), hookPath, `{"pr":1}`, "d-victim", true); w.Code != http.StatusAccepted {
		t.Fatalf("the capture, replayed a day later under another id: %d %s", w.Code, w.Body.String())
	}
	if w := deliverToTeam(receiverFor(h.srv), hookPath, `{"pr":2}`, "d-victim", true); w.Code != http.StatusAccepted {
		t.Errorf("the genuine delivery d-victim: %d %s, want it accepted", w.Code, w.Body.String())
	}
}

// A delivery's keys are its team webhook's: the same signed body and id sent
// to two teams' webhooks of one name publishes into each.
func TestTeamLocalWebhook_KeysAreScopedToTheTeamsWebhook(t *testing.T) {
	h := newHookHarness(t)
	raw := hookedTeamJSON("tenant", 30000, "")
	for _, team := range []string{"hooked", "hooked2"} {
		sc := h.seed("tdf_"+team+"_1", team, raw)
		disarm, err := h.srv.armWalkTriggers(schedWalkCtx(sc, h.walkRun("alice"), "alice"), mustTeamDef(t, raw))
		if err != nil {
			t.Fatal(err)
		}
		defer disarm()
	}
	_, otherMux := h.replica()
	for _, path := range []string{hookPath, "/v1/_teams/acme/hooked2/webhooks/github"} {
		if w := deliverToTeam(otherMux, path, `{"pr":1}`, "d-1", true); w.Code != http.StatusAccepted {
			t.Errorf("%s: %d %s", path, w.Code, w.Body.String())
		}
	}
	for _, ch := range []string{"_team/hooked/events", "_team/hooked2/events"} {
		if got := h.stored(ch, store.MemoryScopeTenant, ""); len(got) != 1 {
			t.Errorf("%s holds %d message(s), want 1", ch, len(got))
		}
	}
}
