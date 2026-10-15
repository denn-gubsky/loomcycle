package webhook

import (
	"context"
	"net/url"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/lookup"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// Delivery counters (RFC EK §9): how each webhook definition's deliveries
// ended, by outcome. Counted in memory at finish and added to the store every
// CountFlushInterval, never per delivery: the receiver answers requests no
// one has authenticated yet, and a write per request would let anyone write
// to the database at will. What is counted names no body text and no header
// value. The cost: a reader is up to one interval behind, and a replica that
// crashes loses what it counted since its last flush.

// CountFlushInterval is how often RunCountFlusher adds the counts to the store.
const CountFlushInterval = 30 * time.Second

// CountStore is where the counters are kept (a store.Store satisfies it).
type CountStore interface {
	WebhookCountsAdd(ctx context.Context, rows []store.WebhookCount) error
	WebhookCounts(ctx context.Context, tenantID, name string, static bool) ([]store.WebhookCount, error)
}

// countKey is one counter: the webhook (webhookKey's value), the outcome
// and, for a refusal, why.
type countKey struct {
	webhook, outcome, reason string
}

// countOutcome maps a delivery's verdict to the outcome it is counted under.
func countOutcome(verdict string) (outcome, reason string) {
	switch verdict {
	case verdictAccepted:
		return store.WebhookOutcomeAccepted, ""
	case verdictAcceptedReplay:
		return store.WebhookOutcomeDuplicate, ""
	case verdictPing:
		return store.WebhookOutcomePing, ""
	case verdictIgnored:
		return store.WebhookOutcomeIgnored, ""
	case verdictFiltered:
		return store.WebhookOutcomeFiltered, ""
	case verdictRejectedSig:
		return store.WebhookOutcomeRefused, "signature"
	case verdictUnresolved:
		return store.WebhookOutcomeRefused, "secret"
	}
	if r, ok := strings.CutPrefix(verdict, "rejected_"); ok {
		return store.WebhookOutcomeRefused, r
	}
	return store.WebhookOutcomeOther, verdict
}

// countDelivery counts one delivery of the webhook named by whKey ("" — no
// definition resolved — counts nothing, as recordDelivery records nothing:
// a counter per made-up name would let anyone grow the map without bound).
// Keyed this way, the map holds at most one entry per definition and outcome.
func (rec *Receiver) countDelivery(whKey, verdict string) {
	if whKey == "" || rec.counts == nil {
		return
	}
	outcome, reason := countOutcome(verdict)
	rec.countMu.Lock()
	rec.pending[countKey{webhook: whKey, outcome: outcome, reason: reason}]++
	rec.countMu.Unlock()
}

// FlushCounts adds what this replica counted since its last flush to the
// store. On an error nothing was added (WebhookCountsAdd is all or none), so
// the counts are kept for the next flush.
func (rec *Receiver) FlushCounts(ctx context.Context) error {
	if rec.counts == nil {
		return nil
	}
	rec.countMu.Lock()
	batch := rec.pending
	rec.pending = map[countKey]int64{}
	rec.countMu.Unlock()
	if len(batch) == 0 {
		return nil
	}
	now := rec.now().UTC()
	rows := make([]store.WebhookCount, 0, len(batch))
	for k, n := range batch {
		owner, name, ok := parseWebhookKey(k.webhook)
		if !ok {
			continue // webhookKey never makes one; nothing to attribute it to
		}
		rows = append(rows, store.WebhookCount{
			TenantID: owner.TenantID, Name: name, Static: owner.Static,
			Outcome: k.outcome, Reason: k.reason, Count: n, LastAt: now,
		})
	}
	if err := rec.counts.WebhookCountsAdd(ctx, rows); err != nil {
		rec.countMu.Lock()
		for k, n := range batch {
			rec.pending[k] += n
		}
		rec.countMu.Unlock()
		return err
	}
	return nil
}

// RunCountFlusher flushes the counts every interval until ctx ends. It does
// not flush on the way out: a clean shutdown calls FlushCounts itself once
// the HTTP server has drained, so a delivery answered during the drain is
// kept too. Returns at once when the receiver counts nothing.
func (rec *Receiver) RunCountFlusher(ctx context.Context, interval time.Duration) {
	if rec.counts == nil {
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			fctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			if err := rec.FlushCounts(fctx); err != nil {
				rec.logf("webhook: storing delivery counts failed (kept for the next try): %v", err)
			}
			cancel()
		case <-ctx.Done():
			return
		}
	}
}

// parseWebhookKey is webhookKey read back: the owner and name of a key it
// made. QueryEscape never leaves a ':' in either part, so the first ':' after
// the owner is the separator.
func parseWebhookKey(key string) (lookup.WebhookOwner, string, bool) {
	if rest, ok := strings.CutPrefix(key, staticWebhookOwner+":"); ok {
		name, err := url.QueryUnescape(rest)
		if err != nil || name == "" {
			return lookup.WebhookOwner{}, "", false
		}
		return lookup.WebhookOwner{Static: true}, name, true
	}
	t, n, ok := strings.Cut(key, ":")
	if !ok {
		return lookup.WebhookOwner{}, "", false
	}
	tenant, err1 := url.QueryUnescape(t)
	name, err2 := url.QueryUnescape(n)
	if err1 != nil || err2 != nil || name == "" {
		return lookup.WebhookOwner{}, "", false
	}
	return lookup.WebhookOwner{TenantID: tenant}, name, true
}
