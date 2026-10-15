package storetest

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

var countsT0 = time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)

func countOf(tenant, name string, static bool, outcome, reason string, n int64, at time.Time) store.WebhookCount {
	return store.WebhookCount{TenantID: tenant, Name: name, Static: static, Outcome: outcome, Reason: reason, Count: n, LastAt: at}
}

// countsByOutcome indexes a webhook's counters by "outcome/reason".
func countsByOutcome(t *testing.T, s store.Store, tenant, name string, static bool) map[string]store.WebhookCount {
	t.Helper()
	rows, err := s.WebhookCounts(context.Background(), tenant, name, static)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]store.WebhookCount{}
	for _, r := range rows {
		out[r.Outcome+"/"+r.Reason] = r
	}
	return out
}

// Adds sum into one row per (definition, outcome, reason); last_at only moves
// forward, so a replica flushing late does not set it back. A zero count
// writes nothing.
func testWebhookCountsAddSumsPerOutcome(t *testing.T, s store.Store) {
	ctx := context.Background()
	if err := s.WebhookCountsAdd(ctx, []store.WebhookCount{
		countOf("acme", "push", false, "accepted", "", 3, countsT0),
		countOf("acme", "push", false, "refused", "signature", 2, countsT0),
		countOf("acme", "push", false, "ping", "", 0, countsT0),
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.WebhookCountsAdd(ctx, []store.WebhookCount{
		countOf("acme", "push", false, "accepted", "", 4, countsT0.Add(time.Minute)),
		countOf("acme", "push", false, "refused", "signature", 1, countsT0.Add(-time.Minute)),
	}); err != nil {
		t.Fatal(err)
	}
	got := countsByOutcome(t, s, "acme", "push", false)
	if len(got) != 2 {
		t.Fatalf("counters = %v, want accepted and refused/signature only (a zero count writes nothing)", got)
	}
	if c := got["accepted/"]; c.Count != 7 || !c.LastAt.Equal(countsT0.Add(time.Minute)) {
		t.Errorf("accepted = %d at %v, want 7 at the later flush", c.Count, c.LastAt)
	}
	if c := got["refused/signature"]; c.Count != 3 || !c.LastAt.Equal(countsT0) {
		t.Errorf("refused/signature = %d at %v, want 3, last_at not set back", c.Count, c.LastAt)
	}
}

// A definition's counters are its own: another tenant's webhook of the same
// name, and the operator's static one in the shared tenant, count apart.
func testWebhookCountsAreKeptPerDefinition(t *testing.T, s store.Store) {
	ctx := context.Background()
	if err := s.WebhookCountsAdd(ctx, []store.WebhookCount{
		countOf("acme", "hook", false, "accepted", "", 1, countsT0),
		countOf("globex", "hook", false, "accepted", "", 2, countsT0),
		countOf("", "hook", false, "accepted", "", 3, countsT0),
		countOf("", "hook", true, "accepted", "", 4, countsT0),
	}); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		tenant string
		static bool
		want   int64
	}{{"acme", false, 1}, {"globex", false, 2}, {"", false, 3}, {"", true, 4}} {
		if got := countsByOutcome(t, s, c.tenant, "hook", c.static)["accepted/"].Count; got != c.want {
			t.Errorf("tenant %q static=%v: accepted = %d, want %d", c.tenant, c.static, got, c.want)
		}
	}
	if rows, err := s.WebhookCounts(ctx, "acme", "never-counted", false); err != nil || len(rows) != 0 {
		t.Errorf("an uncounted webhook = %v, %v; want none", rows, err)
	}
}

// Replicas flushing at once lose no increment.
func testWebhookCountsConcurrentAddsLoseNothing(t *testing.T, s store.Store) {
	ctx := context.Background()
	const writers, each = 8, 5
	var wg sync.WaitGroup
	errs := make(chan error, writers*each)
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < each; i++ {
				errs <- s.WebhookCountsAdd(ctx, []store.WebhookCount{countOf("acme", "busy", false, "accepted", "", 1, countsT0)})
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := countsByOutcome(t, s, "acme", "busy", false)["accepted/"].Count; got != writers*each {
		t.Errorf("accepted = %d after %d concurrent adds of one, want %d", got, writers*each, writers*each)
	}
}
