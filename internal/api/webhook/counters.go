package webhook

import (
	"context"
	"regexp"
	"sync"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// Each webhook counts how its deliveries were answered: accepted, ping,
// ignored, filtered, duplicate, and refused by reason. No body text, no header
// value. A delivery adds to this replica's memory; RunCounters adds the
// increments to the store every 30 seconds, where every replica's add up. A
// write per delivery would let unauthenticated noise write to the database at
// will. So the stored counts are up to 30 seconds behind, and a replica that
// crashes loses its last interval.

// CounterFlushInterval is how often RunCounters writes the counts.
const CounterFlushInterval = 30 * time.Second

// counterOutcome names the counted outcome of a delivery's verdict.
func counterOutcome(verdict string) string {
	switch verdict {
	case verdictAccepted:
		return "accepted"
	case verdictAcceptedReplay:
		return "duplicate"
	case verdictPing, verdictIgnored, verdictFiltered:
		return verdict
	case verdictRejectedSig:
		return "refused.signature"
	case verdictRejectedEvent:
		return "refused.event"
	case verdictRejectedShape:
		return "refused.shape"
	case "rejected_body", "rejected_mapping", verdictRejectedDedupKey:
		return "refused.body"
	case "rejected_rate":
		return "refused.rate"
	}
	return "refused.other"
}

// deliveryCounters are this replica's counts not yet written. Its keys are
// bounded by the definitions that exist times the outcomes: an unknown
// webhook is never counted.
type deliveryCounters struct {
	mu      sync.Mutex
	pending map[[2]string]int64 // {webhook key, outcome} → count
}

func (c *deliveryCounters) add(webhookKey, outcome string, n int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.pending == nil {
		c.pending = make(map[[2]string]int64)
	}
	c.pending[[2]string{webhookKey, outcome}] += n
}

// take empties the counts and returns them.
func (c *deliveryCounters) take() []store.WebhookCounterAdd {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]store.WebhookCounterAdd, 0, len(c.pending))
	for k, n := range c.pending {
		out = append(out, store.WebhookCounterAdd{WebhookKey: k[0], Outcome: k[1], N: n})
	}
	c.pending = nil
	return out
}

// RunCounters writes the delivery counts every CounterFlushInterval until ctx
// ends, then once more. A write that fails keeps its counts for the next one.
func (rec *Receiver) RunCounters(ctx context.Context) {
	t := time.NewTicker(CounterFlushInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			final, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			rec.flushCounters(final)
			cancel()
			return
		case <-t.C:
			rec.flushCounters(ctx)
		}
	}
}

func (rec *Receiver) flushCounters(ctx context.Context) {
	adds := rec.counts.take()
	if len(adds) == 0 || rec.store == nil {
		return
	}
	if err := rec.store.WebhookCountersAdd(ctx, adds, rec.now()); err != nil {
		rec.logf("webhook: write the delivery counters: %v — kept for the next write", err)
		for _, a := range adds {
			rec.counts.add(a.WebhookKey, a.Outcome, a.N)
		}
	}
}

// webhookDeliveryIDRe is what a delivery id may be to reach the run as
// metadata.webhook.delivery_id: a UUID (GitHub, Gitea and GitLab send one), a
// hex digest — the body-hash fallback is "sha256:<hex>" — or a number. It
// comes from an unsigned header and the run reads its metadata as trusted, so
// it is held to forms that cannot spell an instruction; anything else, a
// "short identifier" of letters included, is left out. A label, never proof
// of anything.
var webhookDeliveryIDRe = regexp.MustCompile(`^(?:[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}|(?:sha256:)?[0-9a-fA-F]{1,64}|[0-9]{1,20})$`)

// webhookRunMetadata returns the run's metadata with metadata.webhook added:
// the provider, the event (only once the signed body had its shape), and the
// delivery id when it is a short identifier. The definition's own metadata is
// copied, never changed. A definition that sets neither provider nor events
// is handled as it was before they existed, with nothing added; and one
// without events may hold its own "webhook" key, which is then left as it is.
func webhookRunMetadata(w config.Webhook, event, deliveryID string) map[string]any {
	if w.Provider == "" && len(w.Events) == 0 {
		return w.Metadata
	}
	if _, own := w.Metadata["webhook"]; own {
		return w.Metadata
	}
	hook := map[string]any{}
	if w.Provider != "" {
		hook["provider"] = w.Provider
	}
	if event != "" {
		hook["event"] = event
	}
	if webhookDeliveryIDRe.MatchString(deliveryID) {
		hook["delivery_id"] = deliveryID
	}
	if len(hook) == 0 {
		return w.Metadata
	}
	out := make(map[string]any, len(w.Metadata)+1)
	for k, v := range w.Metadata {
		out[k] = v
	}
	out["webhook"] = hook
	return out
}
