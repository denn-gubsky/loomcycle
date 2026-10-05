package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/lookup"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// A channel-delivery WebhookDef starts no run, so no runs.idempotency_key
// remembers what it accepted. These tests run several receivers over one REAL
// sqlite store — replicas, or one replica before and after a restart — and
// pin that a delivery any of them accepted publishes once, while only what a
// signature covers is held durably.

var channelT0 = time.Unix(stripeTS, 0)

// channelMode is a sigMode with the auth block a webhook signed that way
// declares.
type channelMode struct {
	sigMode
	auth config.WebhookAuth
}

func hmacChannelMode(m sigMode) channelMode {
	return channelMode{m, config.WebhookAuth{Kind: "hmac", Header: m.header, SigningSecretEnv: "WH_SECRET", DeliveryIDHeader: "X-Delivery-Id"}}
}

var (
	githubChannel = hmacChannelMode(githubMode)
	stripeChannel = hmacChannelMode(stripeMode)
	bearerChannel = channelMode{
		sigMode{"bearer", "Authorization", func([]byte) string { return "Bearer " + scopeSecret }},
		config.WebhookAuth{Kind: "bearer", BearerTokenEnv: "WH_SECRET", DeliveryIDHeader: "X-Delivery-Id"},
	}
	channelModes = []channelMode{githubChannel, hmacChannelMode(bareHexMode), stripeChannel, bearerChannel}
)

func channelHook(m channelMode) config.Webhook {
	return config.Webhook{Enabled: true, Delivery: "channel", Channel: "events", Auth: m.auth}
}

// countingPublisher counts the messages that landed; fail, when set, refuses
// every write.
type countingPublisher struct {
	mu   sync.Mutex
	n    int
	fail error
}

func (p *countingPublisher) Publish(_ context.Context, _, _ string, _ store.MemoryScope, _ string,
	_ json.RawMessage, _ time.Time, _ string, _, _ int,
) (store.ChannelMessage, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.fail != nil {
		return store.ChannelMessage{}, p.fail
	}
	p.n++
	return store.ChannelMessage{ID: "msg"}, nil
}

func (p *countingPublisher) PublishNow(ctx context.Context, channel, tenantID string, scope store.MemoryScope, scopeID string,
	payload json.RawMessage, by string, maxMessages, ttl int,
) (store.ChannelMessage, error) {
	return p.Publish(ctx, channel, tenantID, scope, scopeID, payload, time.Time{}, by, maxMessages, ttl)
}

func (p *countingPublisher) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.n
}

func (p *countingPublisher) setFail(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.fail = err
}

// channelReplica is one receiver over st, at clock now.
func channelReplica(st lookup.WebhookStore, pub *countingPublisher, now time.Time, hooks map[string]config.Webhook) *Receiver {
	return New(Deps{
		Cfg:          &config.Config{Webhooks: hooks},
		Store:        st,
		Publisher:    pub,
		EnvAllowlist: map[string]bool{"WH_SECRET": true},
		Now:          fixedClock(now),
		Getenv:       mapGetenv(map[string]string{"WH_SECRET": scopeSecret}),
	})
}

// deliver POSTs body to /v1/_webhooks/<name>, signed in mode m, with id in
// X-Delivery-Id. It takes no *testing.T, so it is safe off the test goroutine.
func deliver(rec *Receiver, name string, m channelMode, body []byte, id string) *httptest.ResponseRecorder {
	return deliverSigned(rec, name, m.header, m.sign(body), body, id)
}

func deliverSigned(rec *Receiver, name, header, sig string, body []byte, id string) *httptest.ResponseRecorder {
	mux := http.NewServeMux()
	rec.Mount(mux)
	req := httptest.NewRequest(http.MethodPost, "/v1/_webhooks/"+name, bytesReader(body))
	req.Header.Set(header, sig)
	if id != "" {
		req.Header.Set("X-Delivery-Id", id)
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	return w
}

func wantStatus(t *testing.T, label string, w *httptest.ResponseRecorder, code int) {
	t.Helper()
	if w.Code != code {
		t.Fatalf("%s = %d %s, want %d", label, w.Code, w.Body.String(), code)
	}
}

func wantPublished(t *testing.T, pub *countingPublisher, n int) {
	t.Helper()
	if got := pub.count(); got != n {
		t.Fatalf("%d message(s) published, want %d", got, n)
	}
}

// The same signed delivery sent to a second replica is answered exactly as
// the first replica answers its own repeat, recorded the same way, and does
// not publish again.
func TestReceiver_ChannelDeliveryToTwoReplicas_PublishesOnce(t *testing.T) {
	for _, m := range channelModes {
		t.Run(m.name, func(t *testing.T) {
			st := openScopeStore(t)
			pub := &countingPublisher{}
			hooks := map[string]config.Webhook{"gh": channelHook(m)}
			a := channelReplica(st, pub, channelT0, hooks)
			b := channelReplica(st, pub, channelT0, hooks)
			body := []byte(`{"event":"ping"}`)

			wantStatus(t, "replica A", deliver(a, "gh", m, body, "evt-1"), http.StatusAccepted)
			onB := deliver(b, "gh", m, body, "evt-1")
			wantStatus(t, "replica B", onB, http.StatusOK)
			wantPublished(t, pub, 1)

			onA := deliver(a, "gh", m, body, "evt-1") // A's own, in-process, repeat
			if onB.Body.String() != onA.Body.String() {
				t.Errorf("replica B answered %s, want what A answers its own repeat: %s", onB.Body.String(), onA.Body.String())
			}
			recs, ok := b.recentSnapshot(webhookKey(lookup.WebhookOwner{Static: true}, "gh"), 1)
			if !ok || len(recs) != 1 || recs[0].Verdict != verdictAcceptedReplay || recs[0].DeliveryID != "evt-1" {
				t.Errorf("replica B recorded %+v, want one %s for evt-1", recs, verdictAcceptedReplay)
			}
			// B now knows the delivery itself: its repeat never reaches the store.
			wantStatus(t, "replica B repeat", deliver(b, "gh", m, body, "evt-1"), http.StatusOK)
			wantPublished(t, pub, 1)
		})
	}
}

// A fresh receiver over the same store — the replica after a restart, its
// in-process guard empty — does not publish a delivery accepted before.
func TestReceiver_ChannelDeliveryAfterARestart_PublishesOnce(t *testing.T) {
	for _, m := range []channelMode{githubChannel, stripeChannel} {
		t.Run(m.name, func(t *testing.T) {
			st := openScopeStore(t)
			pub := &countingPublisher{}
			hooks := map[string]config.Webhook{"gh": channelHook(m)}
			body := []byte(`{"event":"ping"}`)

			wantStatus(t, "before the restart", deliver(channelReplica(st, pub, channelT0, hooks), "gh", m, body, "evt-1"), http.StatusAccepted)
			w := deliver(channelReplica(st, pub, channelT0, hooks), "gh", m, body, "evt-1")
			wantStatus(t, "after the restart", w, http.StatusOK)
			if !strings.Contains(w.Body.String(), `"deduped":"true"`) {
				t.Errorf("after the restart: %s, want the deduped ack", w.Body.String())
			}
			wantPublished(t, pub, 1)
		})
	}
}

// Copies of one delivery racing on two replicas publish once: the claim
// grants exactly one, and every other copy is the idempotent ack.
func TestReceiver_ChannelDeliveryRacingOnTwoReplicas_PublishesOnce(t *testing.T) {
	st := openScopeStore(t)
	pub := &countingPublisher{}
	hooks := map[string]config.Webhook{"gh": channelHook(githubChannel)}
	replicas := []*Receiver{channelReplica(st, pub, channelT0, hooks), channelReplica(st, pub, channelT0, hooks)}
	body := []byte(`{"event":"race"}`)

	const copies = 8
	codes := make(chan int, copies)
	var wg sync.WaitGroup
	for i := 0; i < copies; i++ {
		wg.Add(1)
		go func(rec *Receiver) {
			defer wg.Done()
			codes <- deliver(rec, "gh", githubChannel, body, "evt-1").Code
		}(replicas[i%2])
	}
	wg.Wait()
	close(codes)
	accepted, acked := 0, 0
	for c := range codes {
		switch c {
		case http.StatusAccepted:
			accepted++
		case http.StatusOK:
			acked++
		default:
			t.Errorf("a copy answered %d", c)
		}
	}
	if accepted != 1 || acked != copies-1 {
		t.Errorf("%d accepted and %d acked, want 1 and %d", accepted, acked, copies-1)
	}
	wantPublished(t, pub, 1)
}

// A delivery the receiver refuses — unauthenticated, unparseable, rate
// limited — claims nothing, so a genuine delivery carrying the same id is
// accepted on another replica.
func TestReceiver_ChannelDeliveryRefusedBeforeThePublish_HoldsNoKey(t *testing.T) {
	genuine := []byte(`{"event":"genuine"}`)
	for _, tc := range []struct {
		name    string
		m       channelMode
		limited bool
		refuse  func(a *Receiver, m channelMode) *httptest.ResponseRecorder
		code    int
	}{
		{"unauthenticated bearer", bearerChannel, false, func(a *Receiver, m channelMode) *httptest.ResponseRecorder {
			return deliverSigned(a, "gh", m.header, "Bearer wrong", genuine, "d-1")
		}, http.StatusUnauthorized},
		{"unauthenticated stripe", stripeChannel, false, func(a *Receiver, m channelMode) *httptest.ResponseRecorder {
			return deliverSigned(a, "gh", m.header, "t=1700000000, v1=00", genuine, "d-1")
		}, http.StatusUnauthorized},
		{"unparseable", bearerChannel, false, func(a *Receiver, m channelMode) *httptest.ResponseRecorder {
			return deliver(a, "gh", m, []byte(`not json`), "d-1")
		}, http.StatusBadRequest},
		{"rate limited", bearerChannel, true, func(a *Receiver, m channelMode) *httptest.ResponseRecorder {
			if w := deliver(a, "gh", m, []byte(`{"event":"first"}`), "d-0"); w.Code != http.StatusAccepted {
				return w
			}
			return deliver(a, "gh", m, genuine, "d-1")
		}, http.StatusTooManyRequests},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := openScopeStore(t)
			pub := &countingPublisher{}
			hook := channelHook(tc.m)
			if tc.limited {
				hook.RateLimit = config.WebhookRateLimit{RequestsPerMinute: 60, Burst: 1}
			}
			hooks := map[string]config.Webhook{"gh": hook}
			before := 0
			if tc.limited {
				before = 1
			}

			wantStatus(t, "the refused delivery", tc.refuse(channelReplica(st, pub, channelT0, hooks), tc.m), tc.code)
			wantPublished(t, pub, before)
			wantStatus(t, "the genuine delivery on another replica", deliver(channelReplica(st, pub, channelT0, hooks), "gh", tc.m, genuine, "d-1"), http.StatusAccepted)
			wantPublished(t, pub, before+1)
		})
	}
}

// A body-only signature does not cover the delivery-id header. A captured
// body replayed under a new id is a duplicate on every replica, and the new id
// stays free for the delivery the sender will make under it.
func TestReceiver_ChannelBodyOnlySignedReplayUnderANewID_IsDedupedAndLeavesTheIDFree(t *testing.T) {
	for _, m := range []channelMode{githubChannel, hmacChannelMode(bareHexMode)} {
		t.Run(m.name, func(t *testing.T) {
			st := openScopeStore(t)
			pub := &countingPublisher{}
			hooks := map[string]config.Webhook{"gh": channelHook(m)}
			a := channelReplica(st, pub, channelT0, hooks)
			b := channelReplica(st, pub, channelT0, hooks)
			captured := []byte(`{"event":"captured"}`)

			wantStatus(t, "the original", deliver(a, "gh", m, captured, "evt-1"), http.StatusAccepted)
			wantStatus(t, "the replay under evt-9 on another replica", deliver(b, "gh", m, captured, "evt-9"), http.StatusOK)
			wantPublished(t, pub, 1)
			wantStatus(t, "the sender's own evt-9", deliver(b, "gh", m, []byte(`{"event":"victim"}`), "evt-9"), http.StatusAccepted)
			wantPublished(t, pub, 2)
		})
	}
}

// Once a captured body-only-signed delivery's key has lapsed, its replay is
// accepted again (the signature never expires) — but it holds only the body,
// so filing it under a delivery id not yet sent cannot get the genuine
// delivery dropped on any replica.
func TestReceiver_ChannelBodyOnlySignedReplayAfterTheHold_CannotBlockAnUnsentID(t *testing.T) {
	st := openScopeStore(t)
	pub := &countingPublisher{}
	hooks := map[string]config.Webhook{"gh": channelHook(githubChannel)}
	captured := []byte(`{"event":"captured"}`)
	later := channelT0.Add(durableDedupTTL + time.Hour)

	wantStatus(t, "the original", deliver(channelReplica(st, pub, channelT0, hooks), "gh", githubChannel, captured, "evt-1"), http.StatusAccepted)
	wantStatus(t, "the replay under evt-9, a day later", deliver(channelReplica(st, pub, later, hooks), "gh", githubChannel, captured, "evt-9"), http.StatusAccepted)
	wantStatus(t, "the sender's own evt-9 on a third replica", deliver(channelReplica(st, pub, later, hooks), "gh", githubChannel, []byte(`{"event":"victim"}`), "evt-9"), http.StatusAccepted)
	wantPublished(t, pub, 3)
}

// A delivery whose channel write fails gives its keys back: the sender's
// retry is published, to this replica or another, and only then deduped.
func TestReceiver_ChannelPublishFailure_ReleasesTheKeysForTheRetry(t *testing.T) {
	for _, m := range []channelMode{githubChannel, stripeChannel} {
		t.Run(m.name, func(t *testing.T) {
			st := openScopeStore(t)
			pub := &countingPublisher{}
			hooks := map[string]config.Webhook{"gh": channelHook(m)}
			a := channelReplica(st, pub, channelT0, hooks)
			b := channelReplica(st, pub, channelT0, hooks)
			body := []byte(`{"event":"ping"}`)

			pub.setFail(errors.New("channel store down"))
			wantStatus(t, "the failed write", deliver(a, "gh", m, body, "evt-1"), http.StatusServiceUnavailable)
			pub.setFail(nil)
			wantStatus(t, "the retry on another replica", deliver(b, "gh", m, body, "evt-1"), http.StatusAccepted)
			wantPublished(t, pub, 1)
			wantStatus(t, "a repeat on the first replica", deliver(a, "gh", m, body, "evt-1"), http.StatusOK)
			wantPublished(t, pub, 1)
		})
	}
}

// The durable keys are scoped to the webhook: one body under one id sent to
// two WebhookDefs, to a same-named one in another tenant, or to a team's own
// webhook, never shares a key — and a WebhookDef's owner segment never holds
// the "/" every team webhook's does.
func TestDurableDeliveryKeys_NeverShareAKeyAcrossWebhooksTenantsOrTeams(t *testing.T) {
	body := []byte(`{"event":"ping"}`)
	defKeys := []string{
		webhookKey(lookup.WebhookOwner{Static: true}, "gh"),
		webhookKey(lookup.WebhookOwner{Static: true}, "gh2"),
		webhookKey(lookup.WebhookOwner{}, "gh"),
		webhookKey(lookup.WebhookOwner{TenantID: "acme"}, "gh"),
		webhookKey(lookup.WebhookOwner{TenantID: "acme/triage"}, "gh"),
		webhookKey(lookup.WebhookOwner{TenantID: "team"}, "gh"),
	}
	teamKeys := []string{
		teamWebhookKey("acme", "triage", "gh"),
		teamWebhookKey("", "acme", "gh"),
	}
	for _, k := range defKeys {
		if owner, _, _ := strings.Cut(k, ":"); strings.Contains(owner, "/") {
			t.Errorf("WebhookDef key %q: its owner segment holds a /", k)
		}
	}
	for _, k := range teamKeys {
		if owner, _, _ := strings.Cut(k, ":"); !strings.Contains(owner, "/") {
			t.Errorf("team webhook key %q: its owner segment holds no /", k)
		}
	}
	for _, m := range channelModes {
		get := func(h string) string {
			switch h {
			case m.header:
				return m.sign(body)
			case "X-Delivery-Id":
				return "evt-1"
			}
			return ""
		}
		env := signedEnvelope(m.auth, get)
		did := deliveryID(m.auth, body, get)
		owner := map[string]string{}
		for _, k := range append(append([]string(nil), defKeys...), teamKeys...) {
			for _, held := range durableDeliveryKeys(newDeliveryKeys(k, did, body, env), env) {
				if prev, dup := owner[held]; dup {
					t.Errorf("%s: %q and %q share the durable key %q", m.name, prev, k, held)
				}
				owner[held] = k
			}
		}
	}
}

// End to end: the same body under the same id to two channel WebhookDefs over
// one store is two deliveries.
func TestReceiver_ChannelSameDeliveryToTwoWebhooks_PublishesToEach(t *testing.T) {
	st := openScopeStore(t)
	pub := &countingPublisher{}
	hooks := map[string]config.Webhook{"gh": channelHook(githubChannel), "gh2": channelHook(githubChannel)}
	body := []byte(`{"event":"ping"}`)

	wantStatus(t, "to gh", deliver(channelReplica(st, pub, channelT0, hooks), "gh", githubChannel, body, "evt-1"), http.StatusAccepted)
	wantStatus(t, "to gh2", deliver(channelReplica(st, pub, channelT0, hooks), "gh2", githubChannel, body, "evt-1"), http.StatusAccepted)
	wantPublished(t, pub, 2)
}

// claimCountingStore counts the durable claims made through it.
type claimCountingStore struct {
	store.Store
	mu     sync.Mutex
	claims int
}

func (s *claimCountingStore) WebhookDeliveryClaim(ctx context.Context, keys []string, now, expiresAt time.Time) (bool, error) {
	s.mu.Lock()
	s.claims++
	s.mu.Unlock()
	return s.Store.WebhookDeliveryClaim(ctx, keys, now, expiresAt)
}

func (s *claimCountingStore) claimCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.claims
}

// A spawn delivery is deduped by its run row, as before: it claims nothing in
// the store, answers byte for byte as it did, and its redelivery to a fresh
// replica lands on the original run.
func TestReceiver_SpawnDelivery_ClaimsNoDurableKeyAndAnswersAsBefore(t *testing.T) {
	inner := openScopeStore(t)
	st := &claimCountingStore{Store: inner}
	fr := &storeRunner{st: inner}
	hooks := map[string]config.Webhook{"gh": modeWebhook(githubMode, "X-Delivery-Id")}
	newRec := func() *Receiver {
		return New(Deps{
			Cfg:          &config.Config{Webhooks: hooks},
			Store:        st,
			Runner:       fr,
			EnvAllowlist: map[string]bool{"WH_SECRET": true},
			Now:          fixedClock(channelT0),
			Getenv:       mapGetenv(map[string]string{"WH_SECRET": scopeSecret}),
		})
	}
	body := []byte(`{"goal":"go"}`)

	w := deliver(newRec(), "gh", githubChannel, body, "evt-1")
	wantStatus(t, "the delivery", w, http.StatusAccepted)
	var got map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || got["run_id"] == "" {
		t.Fatalf("undecodable or run-less answer %q: %v", w.Body.String(), err)
	}
	runID := got["run_id"]
	if want := `{"delivery_id":"evt-1","run_id":"` + runID + `","webhook_name":"gh"}` + "\n"; w.Body.String() != want {
		t.Errorf("answer = %q, want %q", w.Body.String(), want)
	}
	again := deliver(newRec(), "gh", githubChannel, body, "evt-1")
	wantStatus(t, "the redelivery to a fresh replica", again, http.StatusAccepted)
	if want := `{"deduped":"true","delivery_id":"evt-1","run_id":"` + runID + `","webhook_name":"gh"}` + "\n"; again.Body.String() != want {
		t.Errorf("redelivery answer = %q, want %q", again.Body.String(), want)
	}
	if n := fr.callCount(); n != 1 {
		t.Errorf("%d runs started, want 1", n)
	}
	if n := st.claimCount(); n != 0 {
		t.Errorf("a spawn delivery made %d durable claim(s), want none", n)
	}
}

// A team delivery rides the walk's run row too: it claims nothing.
func TestReceiver_TeamDelivery_ClaimsNoDurableKey(t *testing.T) {
	st := &claimCountingStore{Store: openScopeStore(t)}
	teams := &fakeTeams{}
	rec, _ := newTeamReceiver(t, teamHook(), teams, st)
	body := []byte(`{"repository":{"full_name":"denn/loomcycle"},"pull_request":{"number":1}}`)

	if code, resp := signedTeamPost(rec, body, "d-1"); code != http.StatusAccepted {
		t.Fatalf("team delivery = %d %v, want 202", code, resp)
	}
	if len(teams.started()) != 1 {
		t.Fatalf("%d walks started, want 1", len(teams.started()))
	}
	if n := st.claimCount(); n != 0 {
		t.Errorf("a team delivery made %d durable claim(s), want none", n)
	}
}
