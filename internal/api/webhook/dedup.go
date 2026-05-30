package webhook

import (
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
)

// dedupTTL is how long a (webhook_name, delivery_id) pair is remembered as
// "already seen". This is the Layer-1, per-replica replay guard — a
// best-effort defense against duplicate deliveries and naive replays within
// a short window. The durable, cross-replica guard is the
// runs.idempotency_key column landing in WH-5 (Layer 2); this in-memory
// layer absorbs the common case cheaply without a DB round-trip.
const dedupTTL = 10 * time.Minute

// dedupCache is a per-replica replay cache keyed by (webhook_name,
// delivery_id) with lazy TTL expiry. Entries are evicted on access (a hit
// past its TTL is treated as a miss and refreshed) plus an optional
// background sweep; lazy expiry alone is sufficient for correctness, the
// sweep only bounds idle memory growth.
type dedupCache struct {
	m   sync.Map // key string -> time.Time (expiry)
	now func() time.Time
}

// newDedupCache returns a cache using the supplied clock. now is injected so
// the TTL window is deterministic in tests.
func newDedupCache(now func() time.Time) *dedupCache {
	if now == nil {
		now = time.Now
	}
	return &dedupCache{now: now}
}

// dedupKey composes the cache key. The webhook name is operator-addressable
// (from the URL path), the delivery id is request-derived; combining them
// scopes the replay window per webhook so two different webhooks can't
// collide on a shared delivery id namespace.
func dedupKey(webhookName, deliveryID string) string {
	return webhookName + "\x00" + deliveryID
}

// seen reports whether this (name, delivery_id) was RECORDED as an accepted
// delivery within the TTL — a pure check that does NOT record. Returns true
// on a replay of an already-accepted delivery (caller rejects 401), false on
// first sight or a stale entry.
//
// Check and record are deliberately split (vs a single check-and-set): the
// id must be recorded only once a delivery is actually ACCEPTED (run admitted
// / channel published), never at the guard step. Otherwise a request that
// passes the signature but is then rejected downstream — rate-limited (429),
// mapping error (400), or a transient spawn-setup 503 — would burn its
// delivery id, and the sender's legitimate retry (same id) would be dropped
// as a replay. That is silent event loss, which Decision 9 forbids. By
// recording only on acceptance, a non-accepted delivery stays retryable.
//
// The residual window (two identical deliveries both passing seen() before
// either calls record()) is bounded by the Layer-2 runs.idempotency_key
// dedup landing in WH-5; Layer-1 here is best-effort per-replica.
func (c *dedupCache) seen(webhookName, deliveryID string) bool {
	v, ok := c.m.Load(dedupKey(webhookName, deliveryID))
	if !ok {
		return false
	}
	exp, ok := v.(time.Time)
	if !ok || c.now().After(exp) {
		return false // stale entry → treat as first sight
	}
	return true
}

// record marks (name, delivery_id) as an accepted delivery for dedupTTL.
// Called only after the delivery is accepted, so a downstream rejection
// never burns the id (see seen()).
func (c *dedupCache) record(webhookName, deliveryID string) {
	c.m.Store(dedupKey(webhookName, deliveryID), c.now().Add(dedupTTL))
}

// sweep evicts all expired entries. Optional — lazy expiry on seen
// keeps correctness; a periodic sweep bounds memory for delivery ids that
// are never seen again. Safe to call concurrently with seen/record.
func (c *dedupCache) sweep() {
	now := c.now()
	c.m.Range(func(k, v interface{}) bool {
		if exp, ok := v.(time.Time); ok && now.After(exp) {
			c.m.Delete(k)
		}
		return true
	})
}

// deliveryID extracts the dedup delivery identity from the request. When the
// Def names a delivery_id_header and it is present, that header value is
// used verbatim. Otherwise a SHA-256 of the raw body is the fallback id, so
// two byte-identical bodies inside the TTL are treated as one delivery.
//
// Hashing the body (vs using it raw as a key) bounds key size and avoids
// holding the full payload in the cache. The hash is NOT a security
// primitive here — it's a content fingerprint — so a plain digest is fine.
func deliveryID(a config.WebhookAuth, body []byte, headerGet func(string) string) string {
	if a.DeliveryIDHeader != "" {
		if v := headerGet(a.DeliveryIDHeader); v != "" {
			return v
		}
	}
	sum := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(sum[:])
}
