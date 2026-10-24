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
// two ids past 2^53 could share a key. It is scanned first, as every other
// decode of a delivery's body is (decodeJSONBody): this one can come first.
func dedupFieldsID(paths []string, body []byte) (id string, missing int, err error) {
	if err := checkJSONShape(body, maxJSONDepth, maxJSONTokens); err != nil {
		return "", -1, fmt.Errorf("malformed json body: %w", err)
	}
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
	// runKey and runAltKey are the idempotency keys the delivery's run carries
	// (deliveryKeys key and alt): recorded on the claim, they find a run that
	// registered but was never linked to it.
	runKey, runAltKey string
	// claimID is the claim's fencing token, set once claimed (runs only).
	claimID string
}

// claim takes keys in the store, before the rate limit, so a duplicate costs
// no token. false means the request is answered: a duplicate, or a store that
// could not be read.
//
// A refused claim is a write transaction, so a captured delivery replayed in a
// loop would be one write per request. A delivery of an event a live run
// holds is answered after a READ instead (heldByLiveRun): the answer the
// claim would give. Anything else — no holder, one not registered, one that
// failed (which the claim takes the event over from), a read error — goes to
// the claim.
func (rec *Receiver) claimHeld(ctx context.Context, w http.ResponseWriter, span trace.Span, name, whKey, did string, h *heldKeys) bool {
	now := rec.now()
	if h.runs {
		if holder, ok := rec.heldByLiveRun(ctx, h.keys[0], now); ok {
			rec.answerHeldDuplicate(w, span, name, whKey, did, holder)
			return false
		}
	}
	var (
		ok     bool
		holder string
		err    error
	)
	if h.runs {
		var res store.WebhookRunClaimResult
		res, err = rec.store.WebhookDeliveryClaimRun(ctx, store.WebhookRunClaim{
			Keys: h.keys, Now: now, ExpiresAt: now.Add(h.ttl), RunKey: h.runKey, RunAltKey: h.runAltKey})
		ok, holder, h.claimID = res.Claimed, res.Holder, res.ClaimID
	} else {
		// A channel delivery's keys were held a day before dedup_key existed;
		// a shorter dedup_ttl_seconds must not shorten that.
		ok, err = rec.store.WebhookDeliveryClaim(ctx, h.keys, now, now.Add(max(h.ttl, durableDedupTTL)))
	}
	if err != nil {
		rec.finish(span, whKey, did, "rejected_claim", "")
		rec.logger.Warn("webhook: claim the delivery's dedup keys", "webhook", name, "err", err)
		writeError(w, http.StatusServiceUnavailable, "runtime_unavailable", "")
		return false
	}
	if ok {
		return true
	}
	rec.answerHeldDuplicate(w, span, name, whKey, did, holder)
	return false
}

// answerHeldDuplicate answers a delivery of an event already handled, by the
// run holder ("" when that claim's run has not registered).
func (rec *Receiver) answerHeldDuplicate(w http.ResponseWriter, span trace.Span, name, whKey, did, holder string) {
	rec.logger.Info("webhook: an event already handled (its dedup_key is held) — idempotent ack", "webhook", name)
	rec.finish(span, whKey, did, verdictAcceptedReplay, holder)
	resp := map[string]string{"webhook_name": name, "delivery_id": did, "deduped": "true"}
	if holder != "" {
		resp["run_id"] = holder
	}
	writeJSON(w, http.StatusOK, resp)
}

// heldByLiveRunReader is the store read heldByLiveRun makes; a store.Store is
// one. A store without it (a test fake) sends every duplicate to the claim.
type heldByLiveRunReader interface {
	WebhookDeliveryHeldByLiveRun(ctx context.Context, key string, now time.Time) (string, bool, error)
}

// heldByLiveRun reports the run holding an event's key when the claim would
// refuse a delivery of it for that run: held past now by a run whose row
// exists and has not ended in a status the claim takes the event over from.
// false on anything else, a read error included: the claim then decides.
func (rec *Receiver) heldByLiveRun(ctx context.Context, eventKey string, now time.Time) (string, bool) {
	r, ok := rec.store.(heldByLiveRunReader)
	if !ok {
		return "", false
	}
	holder, held, err := r.WebhookDeliveryHeldByLiveRun(ctx, eventKey, now)
	if err != nil || !held {
		return "", false
	}
	return holder, true
}

// release drops the keys of a delivery that started nothing, so the sender's
// retry is handled. nil-safe.
func (rec *Receiver) releaseHeld(ctx context.Context, name string, h *heldKeys) {
	if h == nil {
		return
	}
	var err error
	if h.runs {
		// By claim id: a claim taken over meanwhile is another delivery's.
		err = rec.store.WebhookDeliveryReleaseClaim(context.WithoutCancel(ctx), h.claimID)
	} else {
		err = rec.store.WebhookDeliveryRelease(context.WithoutCancel(ctx), h.keys)
	}
	if err != nil {
		rec.logger.Error("webhook: release the dedup keys of a delivery that started nothing", "webhook", name, "err", err)
	}
}

// setHeldRun links the run a delivery handled to its claim, best-effort:
// a run that already exists (a team walk, or the run a duplicate found).
// nil-safe. A link that fails leaves the claim unlinked, and the event's next
// delivery past the grace adopts the run by its keys. A spawned run links
// through linkSpawnedRun instead, which refuses the run when it cannot.
func (rec *Receiver) setHeldRun(name string, h *heldKeys, runID string) {
	if h == nil || runID == "" {
		return
	}
	if _, err := rec.store.WebhookDeliveryLinkRun(context.Background(), h.claimID, runID); err != nil {
		rec.logger.Warn("webhook: record a run on its dedup keys failed", "webhook", name, "run_id", runID, "err", err)
	}
}

// errClaimTakenOver is a spawned run's refusal when its delivery's claim was
// taken over before the run linked to it: a later delivery of the event, past
// the claim's grace, started the event's run.
var errClaimTakenOver = errors.New("a later delivery of the same event took this delivery's dedup claim over")

// Linking a run to its claim is retried a few times on a store error before
// the run is refused.
const (
	linkAttempts   = 3
	linkRetryDelay = 50 * time.Millisecond
)

// linkSpawnedRun links a spawned run to its delivery's claim as the run
// registers, before its first model call; nil when there is no claim. The
// run must not start otherwise:
//   - the claim was taken over (errClaimTakenOver): the event's run is
//     another delivery's;
//   - the link could not be written: the run would run unlinked. A takeover
//     past the grace would find it by its keys and adopt it, so it would not
//     run twice — but its claim would read as unhandled until then, and a
//     store that just refused a write is no ground to start work on. It
//     fails closed instead, and the sender's retry is handled.
func (rec *Receiver) linkSpawnedRun(name string, h *heldKeys, runID string) error {
	if h == nil || !h.runs {
		return nil
	}
	var err error
	for attempt := 0; attempt < linkAttempts; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt) * linkRetryDelay)
		}
		var ok bool
		ok, err = rec.store.WebhookDeliveryLinkRun(context.Background(), h.claimID, runID)
		if err == nil {
			if !ok {
				rec.logger.Warn("webhook: run refused: its dedup claim was taken over by a later delivery", "webhook", name, "run_id", runID)
				return errClaimTakenOver
			}
			return nil
		}
	}
	rec.logger.Error("webhook: run refused: record it on its dedup claim", "webhook", name, "run_id", runID, "err", err)
	return fmt.Errorf("record the run on its delivery's dedup claim: %w", err)
}

// afterSpawn tidies up once a spawned run's RunOnce returned err. A run its
// claim refused (linkErr) ended failed before its first model call, and one
// that started nothing never registered: either way the delivery is not
// handled, so its claim goes back (fenced — a claim taken over is not
// touched) and so do the delivery keys a failed run row carries, else the
// sender's retry would be answered as a duplicate of a run that never ran.
func (rec *Receiver) afterSpawn(name string, held *heldKeys, dk deliveryKeys, err, linkErr error, runID string) {
	if linkErr == nil && !startedNothing(err, runID) {
		return
	}
	rec.releaseHeld(context.Background(), name, held)
	if rec.store == nil {
		return
	}
	if rerr := rec.store.RunReleaseFailedDeliveryKeys(context.Background(), []string{dk.key, dk.alt}); rerr != nil {
		rec.logger.Error("webhook: give back the delivery keys of a run that never ran", "webhook", name, "err", rerr)
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
func (rec *Receiver) newHeldKeys(w http.ResponseWriter, span trace.Span, name, whKey, did string, wd config.Webhook, body []byte, dk deliveryKeys, durable []string) (*heldKeys, bool) {
	if len(wd.DedupKey) == 0 || rec.store == nil {
		return nil, true
	}
	id, missing, err := dedupFieldsID(wd.DedupKey, body)
	if err != nil {
		rec.finish(span, whKey, did, "rejected_mapping", "")
		rec.logger.Warn("webhook: dedup_key", "webhook", name, "err", err)
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
	return &heldKeys{keys: keys, ttl: wd.DedupTTL(), runs: wd.Delivery != "channel", runKey: dk.key, runAltKey: dk.alt}, true
}
