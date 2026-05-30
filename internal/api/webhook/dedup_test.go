package webhook

import (
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
)

func TestDedup_ReplayWithinTTL_Detected(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	c := newDedupCache(fixedClock(now))

	if c.seen("gh", "delivery-1") {
		t.Fatal("first sight reported as replay")
	}
	c.record("gh", "delivery-1")
	if !c.seen("gh", "delivery-1") {
		t.Fatal("recorded delivery within TTL not detected as replay")
	}
}

// A check that is never followed by record() must NOT burn the delivery id:
// this is the rate-limited / mapping-error / setup-error retry path, where
// the request was authenticated but not accepted, so the sender's retry of
// the SAME delivery id must still be processed (Decision 9: no silent loss).
func TestDedup_CheckWithoutRecord_StaysRetryable(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	c := newDedupCache(fixedClock(now))

	if c.seen("gh", "delivery-1") {
		t.Fatal("first sight reported as replay")
	}
	// No record() — the delivery was rejected downstream. The retry must
	// still be a first sight.
	if c.seen("gh", "delivery-1") {
		t.Fatal("a checked-but-not-recorded delivery id was burned — retry would be lost")
	}
}

func TestDedup_DifferentWebhooks_NoCollision(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	c := newDedupCache(fixedClock(now))

	c.record("hook-a", "shared-id")
	// Same delivery id on a different webhook is a fresh first-sight.
	if c.seen("hook-b", "shared-id") {
		t.Fatal("delivery id collided across distinct webhook names")
	}
}

func TestDedup_ExpiryAfterTTL_AcceptsAgain(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	clock := base
	c := newDedupCache(func() time.Time { return clock })

	c.record("gh", "delivery-1")
	// Advance past the TTL window.
	clock = base.Add(dedupTTL + time.Minute)
	if c.seen("gh", "delivery-1") {
		t.Fatal("entry past TTL still treated as replay")
	}
}

func TestDeliveryID_HeaderPreferred_BodyHashFallback(t *testing.T) {
	body := []byte(`{"x":1}`)
	a := config.WebhookAuth{DeliveryIDHeader: "X-Delivery"}

	withHeader := deliveryID(a, body, func(k string) string {
		if k == "X-Delivery" {
			return "abc-123"
		}
		return ""
	})
	if withHeader != "abc-123" {
		t.Errorf("header delivery id = %q, want abc-123", withHeader)
	}

	// Header absent → body-hash fallback (stable + prefixed).
	noHeader := deliveryID(a, body, func(string) string { return "" })
	if noHeader == "" || noHeader[:7] != "sha256:" {
		t.Errorf("fallback delivery id = %q, want sha256: prefix", noHeader)
	}
	// Same body → same fallback id (so replays of identical bodies dedup).
	if again := deliveryID(a, body, func(string) string { return "" }); again != noHeader {
		t.Errorf("body-hash fallback not stable: %q vs %q", noHeader, again)
	}
}
