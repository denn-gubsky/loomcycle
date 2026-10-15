package webhook

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"go.opentelemetry.io/otel/trace"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/jsonpath"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// A webhook with a dedup_key names each event by fields of its signed body,
// and holds that name in the store (webhook_deliveries) for the definition's
// dedup_ttl_seconds, with the body hash and the delivery id the signature
// covers (durableDeliveryKeys), all or none. The claim records the run it
// started, and a duplicate is answered as one unless that run ended failed
// (store.WebhookRetakeRunStatuses).
//
// The run still carries its delivery keys, as without a dedup_key: they last
// as long as its row, past the event key's lifetime, so a byte-identical
// capture is refused after that too. Retaking a failed run's claim clears its
// keys in the same transaction.
//
// This replica's in-memory guard is not consulted for such a webhook: it
// cannot tell that a run failed, and would hold back the forge's redelivery of
// an event whose run did.

const verdictRejectedDedupKey = "rejected_dedup_key"

// dedupFieldsID is the identity of the event a body names by the paths of a
// dedup_key: a hash of the values at those paths, in order. missing is the
// index of the first path the body lacks or holds null at (-1 when every one
// resolves), and err a body that is not JSON.
//
// The body is decoded again with numbers kept as written: decoded to float64,
// two ids past 2^53 could share a key.
func dedupFieldsID(paths []string, body []byte) (id string, missing int, err error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var doc any
	if err := dec.Decode(&doc); err != nil {
		return "", -1, fmt.Errorf("malformed json body: %w", err)
	}
	values := make([]any, len(paths))
	for i, p := range paths {
		segs, err := jsonpath.Parse(p)
		if err != nil {
			return "", i, nil // checked when the definition was written
		}
		v, ok := jsonpath.Eval(doc, segs)
		if !ok || v == nil {
			return "", i, nil
		}
		values[i] = v
	}
	// encoding/json writes object keys sorted, so an object value has one
	// encoding whatever order the sender wrote it in.
	enc, err := json.Marshal(values)
	if err != nil {
		return "", -1, fmt.Errorf("encode dedup_key values: %w", err)
	}
	sum := sha256.Sum256(enc)
	return "fields:sha256:" + hex.EncodeToString(sum[:]), -1, nil
}

// eventKey is the store key of the event a dedup_key names, for the webhook
// named by whKey. Its prefix is not dedupKey's: a delivery id is the sender's
// choice, and under dedupKey one could be written to equal an event's key and
// hold it before the event arrives.
func eventKey(whKey, id string) string {
	return "webhook-event:" + whKey + ":" + id
}

// heldKeys are the store keys of one delivery to a webhook with a dedup_key.
// runs is false for a channel delivery, which starts no run.
type heldKeys struct {
	keys []string
	ttl  time.Duration
	runs bool
}

// claim takes keys in the store, before the rate limit, so a duplicate costs
// no token. false means the request is answered: a duplicate, or a store that
// could not be read.
func (rec *Receiver) claimHeld(ctx context.Context, w http.ResponseWriter, span trace.Span, name, whKey, did string, h *heldKeys) bool {
	now := rec.now()
	var (
		ok     bool
		holder string
		err    error
	)
	if h.runs {
		ok, holder, err = rec.store.WebhookDeliveryClaimRun(ctx, h.keys, now, now.Add(h.ttl))
	} else {
		// A channel delivery's keys were held a day before dedup_key existed;
		// a shorter dedup_ttl_seconds must not shorten that.
		ok, err = rec.store.WebhookDeliveryClaim(ctx, h.keys, now, now.Add(max(h.ttl, durableDedupTTL)))
	}
	if err != nil {
		rec.finish(span, whKey, did, "rejected_claim", "")
		rec.logf("webhook %q: claim the delivery's dedup keys: %v", name, err)
		writeError(w, http.StatusServiceUnavailable, "runtime_unavailable", "")
		return false
	}
	if ok {
		return true
	}
	rec.logf("webhook %q: an event already handled (its dedup_key is held) — idempotent ack", name)
	rec.finish(span, whKey, did, verdictAcceptedReplay, holder)
	resp := map[string]string{"webhook_name": name, "delivery_id": did, "deduped": "true"}
	if holder != "" {
		resp["run_id"] = holder
	}
	writeJSON(w, http.StatusOK, resp)
	return false
}

// release drops the keys of a delivery that started nothing, so the sender's
// retry is handled. nil-safe.
func (rec *Receiver) releaseHeld(ctx context.Context, name string, h *heldKeys) {
	if h == nil {
		return
	}
	if err := rec.store.WebhookDeliveryRelease(context.WithoutCancel(ctx), h.keys); err != nil {
		rec.logf("webhook %q: release the dedup keys of a delivery that started nothing: %v", name, err)
	}
}

// setRun records the run a delivery's keys started. nil-safe. Not undone on
// failure: the keys stay held for their lifetime with no run, which reads as a
// run whose row is gone.
func (rec *Receiver) setHeldRun(name string, h *heldKeys, runID string) {
	if h == nil || runID == "" {
		return
	}
	if err := rec.store.WebhookDeliverySetRun(context.Background(), h.keys, runID); err != nil {
		rec.logf("webhook %q: record run %s on its dedup keys: %v", name, runID, err)
	}
}

// startedNothing reports whether a start that returned err, with runID the
// run it registered ("" for none), left the event unhandled, so its held keys
// go back. A duplicate's event was handled, by the run that holds its key.
func startedNothing(err error, runID string) bool {
	return err != nil && runID == "" && !errors.Is(err, store.ErrDuplicateIdempotencyKey)
}

// newHeldKeys builds the held keys of a delivery to wd, or answers it 400
// when its body does not name the event (nil, false). nil, true when wd has
// no dedup_key or there is no store to hold one in (a receiver with no
// store has only its in-memory guard).
func (rec *Receiver) newHeldKeys(w http.ResponseWriter, span trace.Span, name, whKey, did string, wd config.Webhook, body []byte, durable []string) (*heldKeys, bool) {
	if len(wd.DedupKey) == 0 || rec.store == nil {
		return nil, true
	}
	id, missing, err := dedupFieldsID(wd.DedupKey, body)
	if err != nil {
		rec.finish(span, whKey, did, "rejected_mapping", "")
		rec.logf("webhook %q: dedup_key: %v", name, err)
		writeError(w, http.StatusBadRequest, "bad_mapping", "")
		return nil, false
	}
	if missing >= 0 {
		rec.finish(span, whKey, did, verdictRejectedDedupKey, "")
		// The path is the definition's own, not the body's.
		writeError(w, http.StatusBadRequest, "dedup_key_unresolvable",
			fmt.Sprintf("dedup_key[%d] %s is missing or null in the body", missing, wd.DedupKey[missing]))
		return nil, false
	}
	keys := append([]string{eventKey(whKey, id)}, durable...)
	return &heldKeys{keys: keys, ttl: wd.DedupTTL(), runs: wd.Delivery != "channel"}, true
}
