package webhook

import (
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"sync"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/lookup"
)

// dedupTTL is how long a (webhook, delivery_id) key is remembered as
// "already seen". This is the Layer-1, per-replica replay guard — a
// best-effort defense against duplicate deliveries and naive replays within
// a short window. The durable, cross-replica guard is the
// runs.idempotency_key column landing in WH-5 (Layer 2); this in-memory
// layer absorbs the common case cheaply without a DB round-trip.
const dedupTTL = 10 * time.Minute

// dedupCache is a per-replica replay cache keyed by dedupKey (webhook,
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

// webhookKey names ONE webhook def by the owner it resolved from (see
// lookup.WebhookOwner) plus its name. It keys every piece of per-webhook
// state: both dedup layers (via dedupKey) and the rate-limit bucket.
//
// It deliberately ignores the URL tenant. /v1/_webhooks/{tenant}/{name}
// accepts any tenant segment and an unknown one falls through to the static
// or shared def, so a URL-derived key let one captured signed delivery replay
// once per made-up prefix — each a fresh key in both dedup layers, each a new
// run — and let an unrelated tenant's traffic share a same-named webhook's
// bucket.
//
// The tenant and name are query-escaped, so neither contains ":". The static
// marker does, so no tenant name can produce it. The name is never empty (it
// is a URL path segment).
func webhookKey(owner lookup.WebhookOwner, name string) string {
	o := url.QueryEscape(owner.TenantID)
	if owner.Static {
		o = staticWebhookOwner
	}
	return o + ":" + url.QueryEscape(name)
}

// staticWebhookOwner marks a yaml cfg.Webhooks def in webhookKey.
const staticWebhookOwner = "static:"

// dedupKey composes a key both dedup layers use: the Layer-1 cache key here
// AND the durable runs.idempotency_key (Layer 2); newDeliveryKeys picks which
// of a delivery's keys is persisted. webhook is webhookKey's
// value, so a delivery id only dedups against deliveries to the SAME webhook
// def. The idempotency_key unique index spans the whole database, so a bare
// delivery id (a body hash, or a sender-chosen header) made a byte-identical
// body sent to a different webhook, in any tenant, dedup against the first
// one: no run started and the response carried the other webhook's run_id.
//
// The delivery id is last, so it may contain anything. Postgres TEXT cannot
// hold NUL, which rules out a "\x00" separator for the persisted key.
func dedupKey(webhook, deliveryID string) string {
	return "webhook:" + webhook + ":" + deliveryID
}

// seen reports whether this delivery key was RECORDED as an accepted
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
func (c *dedupCache) seen(key string) bool {
	v, ok := c.m.Load(key)
	if !ok {
		return false
	}
	exp, ok := v.(time.Time)
	if !ok || c.now().After(exp) {
		return false // stale entry → treat as first sight
	}
	return true
}

// record marks a delivery key (see dedupKey) as accepted for dedupTTL.
// Called only after the delivery is accepted, so a downstream rejection
// never burns the id (see seen()).
func (c *dedupCache) record(key string) {
	c.m.Store(key, c.now().Add(dedupTTL))
}

// seenAny reports whether either of a delivery's keys was recorded (seen).
func (c *dedupCache) seenAny(k deliveryKeys) bool {
	return c.seen(k.key) || (k.alt != "" && c.seen(k.alt))
}

// recordAccepted records every key of a delivery this receiver ACCEPTED.
func (c *dedupCache) recordAccepted(k deliveryKeys) {
	c.record(k.key)
	if k.alt != "" {
		c.record(k.alt)
	}
}

// recordDuplicate records a delivery answered as a duplicate: only k.dup, the
// identity its signature covers (see deliveryKeys).
func (c *dedupCache) recordDuplicate(k deliveryKeys) {
	c.record(k.dup)
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
	return bodyDeliveryID(body)
}

// bodyDeliveryID is the body-hash delivery id: deliveryID's fallback, and the
// second identity of a body-only-signed delivery (newDeliveryKeys).
func bodyDeliveryID(body []byte) string {
	sum := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// signedPayloadID is the identity of a Stripe-style delivery's signed payload,
// `<t>.<body>` — exactly what its v1 MAC covers. Its own prefix keeps it apart
// from bodyDeliveryID values.
func signedPayloadID(timestamp string, body []byte) string {
	h := sha256.New()
	h.Write([]byte(timestamp + "."))
	h.Write(body)
	return "signed:sha256:" + hex.EncodeToString(h.Sum(nil))
}

// deliveryKeys are the dedup keys of one delivery (dedupKey values).
type deliveryKeys struct {
	// key is persisted as the run's runs.idempotency_key and is a Layer-1
	// key.
	key string
	// alt is persisted as the run's runs.delivery_alt_key and is a Layer-1
	// key. "" unless the delivery has two identities (see newDeliveryKeys).
	alt string
	// dup is the one key a delivery answered as a duplicate records in
	// Layer 1: the identity its signature covers, when it has two. The other
	// is a header value the signature does not cover; recording it would let
	// a replay burn an id a genuine later delivery has yet to use.
	dup string
}

// newDeliveryKeys derives the keys a delivery to the webhook named by whKey
// dedups on. did is deliveryID's value; env is what its signature covers.
//
// Normally that is the one key dedupKey(whKey, did). But no HMAC envelope
// signs the delivery-id header, so whoever captured one signed delivery could
// replay it with a new header value each time, each a fresh key and a new
// run. So an HMAC-signed delivery also has the identity of what its signature
// covers, and it is a duplicate when EITHER identity was seen; both are
// persisted, so that holds after a restart and on every replica:
//
//   - GitHub `sha256=` / bare hex sign the body alone, with no time limit:
//     key is the body, alt the sender's id. A replay under a new id, and a
//     seen id under a new body, are both duplicates. Two distinct deliveries
//     with byte-identical bodies are one — already the rule for a def without
//     delivery_id_header.
//   - Stripe `t=,v1=` signs `<t>.<body>`, valid for ±signatureTolerance: key
//     is the sender's id, alt the signed payload. A replay must reuse the
//     signed timestamp (it cannot re-sign), so it matches alt; a genuine retry
//     re-signs under a new timestamp but keeps its id, so it matches key, days
//     later too. A sender posting the same body as separate deliveries signs
//     a fresh timestamp each time and is not deduped — unless two share a
//     second.
//
// Without a delivery-id header did IS the body hash, which already covers
// every one of these cases: one key.
func newDeliveryKeys(whKey, did string, body []byte, env envelope) deliveryKeys {
	key := dedupKey(whKey, did)
	if !env.signsBody {
		return deliveryKeys{key: key, dup: key}
	}
	bodyKey := dedupKey(whKey, bodyDeliveryID(body))
	if bodyKey == key {
		return deliveryKeys{key: key, dup: key}
	}
	if env.timestamp != "" {
		signed := dedupKey(whKey, signedPayloadID(env.timestamp, body))
		return deliveryKeys{key: key, alt: signed, dup: signed}
	}
	return deliveryKeys{key: bodyKey, alt: key, dup: bodyKey}
}
