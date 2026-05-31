package webhook

import (
	"net/http"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
)

// TestReceiver_RateLimitedDeliveryNotRecordedAsReplay pins the property the
// RFC H two-layer idempotency design depends on: a delivery rejected by the
// rate limiter (429) must NOT be recorded in the dedup cache, so the
// sender's legitimate retry of that same delivery id is processed rather
// than silently dropped as a "replay". (Recording happens only on
// acceptance paths, AFTER the rate-limit gate.)
//
// This is a known real-world bug class — a regression that moved record()
// before the rate-limit check would re-introduce it — so the property is
// pinned end-to-end through the HTTP handler, not just the dedup primitive.
func TestReceiver_RateLimitedDeliveryNotRecordedAsReplay(t *testing.T) {
	secret := "shhh"
	now := time.Unix(1_700_000_000, 0)
	wh := config.Webhook{
		Enabled:  true,
		Delivery: "spawn",
		Agent:    "x",
		Auth: config.WebhookAuth{
			Kind:             "hmac",
			Header:           "X-Hub-Signature-256",
			SigningSecretEnv: "WH_SECRET",
			DeliveryIDHeader: "X-Delivery-Id", // explicit ids so the test controls them
		},
		RateLimit: config.WebhookRateLimit{RequestsPerMinute: 60, Burst: 1},
	}
	fr := &fakeRunner{runID: "r", agentID: "a"}
	rec := newTestReceiver(t, map[string]config.Webhook{"gh": wh}, fr, nil, map[string]string{"WH_SECRET": secret}, []string{"WH_SECRET"}, now)

	body := []byte(`{"goal":"g"}`)
	sig := githubSig(secret, body)
	post := func(id string) int {
		h := http.Header{}
		h.Set("X-Hub-Signature-256", sig)
		h.Set("X-Delivery-Id", id)
		return doPost(rec, "gh", body, h).Code
	}

	// Delivery A consumes the only burst token → accepted + recorded.
	if code := post("A"); code != http.StatusAccepted {
		t.Fatalf("delivery A status = %d, want 202", code)
	}
	// Delivery B finds no token → 429 (rate-limited).
	if code := post("B"); code != http.StatusTooManyRequests {
		t.Fatalf("delivery B status = %d, want 429", code)
	}

	// The rate-limited delivery B must NOT be in the dedup cache: a 429 is a
	// "try again", not a "seen this one". If it were recorded, B's retry
	// would be wrongly dropped as a replay.
	if rec.dedup.seen("gh", "B") {
		t.Error("rate-limited delivery B was recorded in the dedup cache; its retry would be dropped as a replay")
	}
	// Sanity: the ACCEPTED delivery A is recorded, so genuine duplicates are
	// still caught (the dedup layer isn't simply disabled).
	if !rec.dedup.seen("gh", "A") {
		t.Error("accepted delivery A was not recorded; genuine replays would not be caught")
	}
}
