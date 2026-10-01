package webhook

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// Every HMAC envelope signs the body and none signs the delivery-id header:
// GitHub `sha256=` and bare hex cover the raw body only, with no timestamp
// window; Stripe `t=,v1=` covers a timestamp plus the body, valid for ±5 min.
// These tests pin that such a delivery dedups on its body as well as its id,
// so a captured delivery cannot be replayed into new runs by changing that
// header.

// sigMode is one HMAC envelope: the header it travels in and how to sign.
type sigMode struct {
	name   string
	header string
	sign   func(body []byte) string
}

var (
	githubMode  = sigMode{"github", "X-Hub-Signature-256", func(b []byte) string { return githubSig(scopeSecret, b) }}
	bareHexMode = sigMode{"bare-hex", "Linear-Signature", func(b []byte) string {
		return strings.TrimPrefix(githubSig(scopeSecret, b), "sha256=")
	}}
	// Signed at the scope receiver's fixed clock, so inside the ±5 min window.
	stripeMode = sigMode{"stripe", "X-Loomcycle-Signature", func(b []byte) string {
		return stripeSig(scopeSecret, b, time.Unix(1_700_000_000, 0))
	}}
	hmacModes = []sigMode{githubMode, bareHexMode, stripeMode}
)

func modeWebhook(m sigMode, deliveryIDHeader string) config.Webhook {
	return config.Webhook{
		Enabled:  true,
		Delivery: "spawn",
		Agent:    "x",
		Auth: config.WebhookAuth{
			Kind: "hmac", Header: m.header,
			SigningSecretEnv: "WH_SECRET", DeliveryIDHeader: deliveryIDHeader,
		},
	}
}

// postMode POSTs body to /v1/_webhooks/gh signed in mode m, with deliveryID
// in X-Delivery-Id when non-empty.
func postMode(t *testing.T, rec *Receiver, m sigMode, body []byte, deliveryID string) (int, map[string]string) {
	t.Helper()
	mux := http.NewServeMux()
	rec.Mount(mux)
	req := httptest.NewRequest(http.MethodPost, "/v1/_webhooks/gh", bytesReader(body))
	req.Header.Set(m.header, m.sign(body))
	if deliveryID != "" {
		req.Header.Set("X-Delivery-Id", deliveryID)
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	var got map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("POST: undecodable body %q: %v", w.Body.String(), err)
	}
	return w.Code, got
}

// assertDeduped requires a dedup ack (wantCode) naming run original.
func assertDeduped(t *testing.T, label string, wantCode, code int, got map[string]string, original string) {
	t.Helper()
	if code != wantCode || got["deduped"] != "true" || got["run_id"] != original {
		t.Errorf("%s = %d %v; want %d deduped run_id=%s", label, code, got, wantCode, original)
	}
}

// assertPersistedKey requires the run stored under idempotency key k to be runID.
func assertPersistedKey(t *testing.T, st store.Store, k, runID string) {
	t.Helper()
	r, ok, err := st.RunByIdempotencyKey(context.Background(), k)
	if err != nil || !ok || r.ID != runID {
		t.Errorf("run under idempotency key %q = (%q, %v, %v), want %q", k, r.ID, ok, err, runID)
	}
}

func TestReceiver_HMACSignedBodyReplayedWithNewDeliveryIDs_StartsOneRun(t *testing.T) {
	for _, m := range hmacModes {
		t.Run(m.name, func(t *testing.T) {
			st := openScopeStore(t)
			fr := &storeRunner{st: st}
			hooks := map[string]config.Webhook{"gh": modeWebhook(m, "X-Delivery-Id")}
			body := []byte(`{"goal":"deploy prod"}`)

			rec := newScopeReceiver(st, fr, hooks)
			code, got := postMode(t, rec, m, body, "evt-1")
			original := assertFreshRun(t, "evt-1", code, got)

			// Layer 1: same receiver, a new header value each time.
			for _, id := range []string{"evt-2", "evt-3"} {
				code, got = postMode(t, rec, m, body, id)
				assertDeduped(t, "layer-1 replay "+id, http.StatusOK, code, got, original)
				if got["delivery_id"] != id {
					t.Errorf("%s: delivery_id = %q, want the sender's id", id, got["delivery_id"])
				}
			}

			// Layer 2 on its own: a fresh receiver (restart / other replica).
			fresh := newScopeReceiver(st, fr, hooks)
			code, got = postMode(t, fresh, m, body, "evt-4")
			assertDeduped(t, "layer-2 replay evt-4", http.StatusAccepted, code, got, original)

			if n := fr.callCount(); n != 1 {
				t.Errorf("one signed body started %d runs, want 1", n)
			}
			// The signed body, not the unsigned header, is the durable key.
			assertPersistedKey(t, st, staticDedupKey("gh", bodyHashID(body)), original)
		})
	}
}

func TestReceiver_HMACSignedRedeliverySameID_ReturnsOriginalRun(t *testing.T) {
	for _, m := range hmacModes {
		t.Run(m.name, func(t *testing.T) {
			st := openScopeStore(t)
			fr := &storeRunner{st: st}
			hooks := map[string]config.Webhook{"gh": modeWebhook(m, "X-Delivery-Id")}
			body := []byte(`{"goal":"once"}`)

			rec := newScopeReceiver(st, fr, hooks)
			code, got := postMode(t, rec, m, body, "evt-1")
			original := assertFreshRun(t, "first delivery", code, got)

			code, got = postMode(t, rec, m, body, "evt-1")
			assertDeduped(t, "layer-1 redelivery", http.StatusOK, code, got, original)

			fresh := newScopeReceiver(st, fr, hooks)
			code, got = postMode(t, fresh, m, body, "evt-1")
			assertDeduped(t, "layer-2 redelivery", http.StatusAccepted, code, got, original)
			if got["delivery_id"] != "evt-1" {
				t.Errorf("delivery_id = %q, want the sender's id evt-1", got["delivery_id"])
			}
			if n := fr.callCount(); n != 1 {
				t.Errorf("runner invoked %d times, want 1", n)
			}
		})
	}
}

func TestReceiver_HMACSignedDistinctBodies_StartTwoRuns(t *testing.T) {
	for _, m := range hmacModes {
		t.Run(m.name, func(t *testing.T) {
			st := openScopeStore(t)
			fr := &storeRunner{st: st}
			rec := newScopeReceiver(st, fr, map[string]config.Webhook{"gh": modeWebhook(m, "X-Delivery-Id")})

			code, got := postMode(t, rec, m, []byte(`{"goal":"a"}`), "evt-1")
			runA := assertFreshRun(t, "body a", code, got)
			code, got = postMode(t, rec, m, []byte(`{"goal":"b"}`), "evt-2")
			runB := assertFreshRun(t, "body b", code, got)
			if runA == runB {
				t.Errorf("two deliveries answered one run %q", runA)
			}
		})
	}
}

// Either identity makes a duplicate: a seen delivery id is one even under a
// new body. Only Layer 1 can see this — runs.idempotency_key holds the body
// key alone — so the check runs on the receiver that accepted the first one.
func TestReceiver_HMACSignedSeenDeliveryIDNewBody_DedupedInLayer1(t *testing.T) {
	for _, m := range hmacModes {
		t.Run(m.name, func(t *testing.T) {
			st := openScopeStore(t)
			fr := &storeRunner{st: st}
			rec := newScopeReceiver(st, fr, map[string]config.Webhook{"gh": modeWebhook(m, "X-Delivery-Id")})

			code, got := postMode(t, rec, m, []byte(`{"goal":"a"}`), "evt-1")
			assertFreshRun(t, "evt-1", code, got)
			code, got = postMode(t, rec, m, []byte(`{"goal":"b"}`), "evt-1")
			if code != http.StatusOK || got["deduped"] != "true" {
				t.Errorf("seen evt-1 under a new body = %d %v, want 200 deduped", code, got)
			}
			if n := fr.callCount(); n != 1 {
				t.Errorf("runner invoked %d times, want 1", n)
			}
		})
	}
}

// A replay answered as a duplicate must not record its unsigned header value:
// that would let whoever holds a captured delivery burn an id the sender has
// yet to use, and the genuine delivery carrying it would be dropped.
func TestReceiver_HMACSignedReplay_LeavesItsNewDeliveryIDUsable(t *testing.T) {
	for _, m := range hmacModes {
		t.Run(m.name, func(t *testing.T) {
			st := openScopeStore(t)
			fr := &storeRunner{st: st}
			hooks := map[string]config.Webhook{"gh": modeWebhook(m, "X-Delivery-Id")}
			captured := []byte(`{"goal":"captured"}`)

			code, got := postMode(t, newScopeReceiver(st, fr, hooks), m, captured, "evt-1")
			original := assertFreshRun(t, "captured delivery", code, got)

			// On a fresh receiver the replay is answered from Layer 2, the
			// path that warms Layer 1.
			fresh := newScopeReceiver(st, fr, hooks)
			code, got = postMode(t, fresh, m, captured, "evt-9")
			assertDeduped(t, "replay under evt-9", http.StatusAccepted, code, got, original)

			code, got = postMode(t, fresh, m, []byte(`{"goal":"genuine"}`), "evt-9")
			if runID := assertFreshRun(t, "genuine evt-9", code, got); runID == original {
				t.Errorf("genuine evt-9 answered the captured run %s", original)
			}
		})
	}
}

func TestReceiver_HMACSignedChannelReplayWithNewDeliveryID_PublishesOnce(t *testing.T) {
	fp := &fakePublisher{}
	wh := modeWebhook(githubMode, "X-Delivery-Id")
	wh.Delivery, wh.Channel = "channel", "events"
	rec := newTestReceiver(t, map[string]config.Webhook{"gh": wh}, nil, fp, map[string]string{"WH_SECRET": scopeSecret}, []string{"WH_SECRET"}, time.Unix(1_700_000_000, 0))
	body := []byte(`{"goal":"ping"}`)

	if code, got := postMode(t, rec, githubMode, body, "evt-1"); code != http.StatusAccepted {
		t.Fatalf("first delivery = %d %v, want 202", code, got)
	}
	fp.mu.Lock()
	fp.called = false
	fp.mu.Unlock()

	code, got := postMode(t, rec, githubMode, body, "evt-2")
	if code != http.StatusOK || got["deduped"] != "true" {
		t.Errorf("replay under evt-2 = %d %v, want 200 deduped", code, got)
	}
	fp.mu.Lock()
	defer fp.mu.Unlock()
	if fp.called {
		t.Error("replay under a new delivery id published to the channel again")
	}
}

// A genuine Stripe retry re-signs under a NEW timestamp but carries the same
// event body (the event id is inside it), so it is answered with the original
// run by both layers — after a restart through the persisted body key.
func TestReceiver_StripeSignedRetryNewTimestamp_ReturnsOriginalRun(t *testing.T) {
	st := openScopeStore(t)
	fr := &storeRunner{st: st}
	hooks := map[string]config.Webhook{"gh": modeWebhook(stripeMode, "X-Delivery-Id")}
	body := []byte(`{"id":"evt_1","type":"invoice.paid"}`)
	// Still inside the ±5 min window of the receiver's fixed clock.
	retry := sigMode{"stripe-retry", stripeMode.header, func(b []byte) string {
		return stripeSig(scopeSecret, b, time.Unix(1_700_000_000+90, 0))
	}}

	rec := newScopeReceiver(st, fr, hooks)
	code, got := postMode(t, rec, stripeMode, body, "evt-1")
	original := assertFreshRun(t, "first delivery", code, got)

	code, got = postMode(t, rec, retry, body, "evt-1")
	assertDeduped(t, "layer-1 retry", http.StatusOK, code, got, original)

	fresh := newScopeReceiver(st, fr, hooks)
	code, got = postMode(t, fresh, retry, body, "evt-1")
	assertDeduped(t, "layer-2 retry", http.StatusAccepted, code, got, original)

	if n := fr.callCount(); n != 1 {
		t.Errorf("runner invoked %d times, want 1", n)
	}
	assertPersistedKey(t, st, staticDedupKey("gh", bodyHashID(body)), original)
}

// Bearer and none sign nothing, so the delivery id stays their only key: a
// second id on the same body is a separate delivery, as before.
func TestReceiver_UnsignedAuthSameBodyNewDeliveryID_KeysOnDeliveryIDOnly(t *testing.T) {
	bearer := sigMode{"bearer", "Authorization", func([]byte) string { return "Bearer " + scopeSecret }}
	none := sigMode{"none", "X-Unused", func([]byte) string { return "" }}
	for _, tc := range []struct {
		m    sigMode
		auth config.WebhookAuth
	}{
		{bearer, config.WebhookAuth{Kind: "bearer", BearerTokenEnv: "WH_SECRET", DeliveryIDHeader: "X-Delivery-Id"}},
		{none, config.WebhookAuth{Kind: "none", DeliveryIDHeader: "X-Delivery-Id"}},
	} {
		t.Run(tc.m.name, func(t *testing.T) {
			st := openScopeStore(t)
			fr := &storeRunner{st: st}
			hooks := map[string]config.Webhook{"gh": {Enabled: true, Delivery: "spawn", Agent: "x", Auth: tc.auth}}
			newRec := func() *Receiver {
				return New(Deps{
					Cfg:                  &config.Config{Webhooks: hooks},
					Store:                st,
					Runner:               fr,
					EnvAllowlist:         map[string]bool{"WH_SECRET": true},
					Now:                  fixedClock(time.Unix(1_700_000_000, 0)),
					Getenv:               mapGetenv(map[string]string{"WH_SECRET": scopeSecret}),
					AllowUnauthenticated: true,
				})
			}
			body := []byte(`{"goal":"same"}`)
			rec := newRec()

			code, got := postMode(t, rec, tc.m, body, "evt-1")
			first := assertFreshRun(t, "evt-1", code, got)
			code, got = postMode(t, rec, tc.m, body, "evt-2")
			if second := assertFreshRun(t, "evt-2", code, got); second == first {
				t.Errorf("evt-2 answered evt-1's run %s", first)
			}
			code, got = postMode(t, newRec(), tc.m, body, "evt-1")
			assertDeduped(t, "layer-2 evt-1 redelivery", http.StatusAccepted, code, got, first)
			assertPersistedKey(t, st, staticDedupKey("gh", "evt-1"), first)
		})
	}
}

func TestSignsBody_TrueOnlyForHMACAuth(t *testing.T) {
	for kind, want := range map[string]bool{"": true, "hmac": true, " HMAC ": true, "bearer": false, "none": false, "other": false} {
		if got := signsBody(config.WebhookAuth{Kind: kind}); got != want {
			t.Errorf("signsBody(kind=%q) = %v, want %v", kind, got, want)
		}
	}
}

// Without delivery_id_header the delivery id already IS the body hash, in
// every mode; a header the def does not name is ignored as before.
func TestReceiver_NoDeliveryIDHeaderDef_KeysOnBodyHashAsBefore(t *testing.T) {
	for _, m := range []sigMode{githubMode, bareHexMode, stripeMode} {
		t.Run(m.name, func(t *testing.T) {
			st := openScopeStore(t)
			fr := &storeRunner{st: st}
			hooks := map[string]config.Webhook{"gh": modeWebhook(m, "")}
			body := []byte(`{"goal":"same"}`)
			rec := newScopeReceiver(st, fr, hooks)

			code, got := postMode(t, rec, m, body, "evt-1")
			original := assertFreshRun(t, "first", code, got)
			code, got = postMode(t, rec, m, body, "evt-2")
			assertDeduped(t, "same body", http.StatusOK, code, got, original)
			code, got = postMode(t, rec, m, []byte(`{"goal":"other"}`), "evt-1")
			if runID := assertFreshRun(t, "other body", code, got); runID == original {
				t.Errorf("a different body answered run %s", original)
			}
			assertPersistedKey(t, st, staticDedupKey("gh", bodyHashID(body)), original)
		})
	}
}
