package storetest

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// A webhook's counts add up across writes — every replica adds its own — and
// stay apart from another webhook's. The latest write's time is reported.
func testWebhookCountersAddUpPerWebhook(t *testing.T, s store.Store) {
	ctx := context.Background()
	nonce := fmt.Sprintf("%d", time.Now().UnixNano())
	a, b := "acme:gh-"+nonce, "acme:other-"+nonce
	t0 := armT0
	if err := s.WebhookCountersAdd(ctx, []store.WebhookCounterAdd{
		{WebhookKey: a, Outcome: "accepted", N: 3}, {WebhookKey: a, Outcome: "ping", N: 1}, {WebhookKey: b, Outcome: "accepted", N: 7},
	}, t0); err != nil {
		t.Fatal(err)
	}
	if err := s.WebhookCountersAdd(ctx, []store.WebhookCounterAdd{
		{WebhookKey: a, Outcome: "accepted", N: 2}, {WebhookKey: a, Outcome: "refused.signature", N: 4},
	}, t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	got, updated, err := s.WebhookCountersGet(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]int64{"accepted": 5, "ping": 1, "refused.signature": 4}; !reflect.DeepEqual(got, want) {
		t.Errorf("counts = %v, want %v", got, want)
	}
	if !updated.Equal(t0.Add(time.Minute)) {
		t.Errorf("updated_at = %v, want %v", updated, t0.Add(time.Minute))
	}
	if got, _, _ := s.WebhookCountersGet(ctx, b); !reflect.DeepEqual(got, map[string]int64{"accepted": 7}) {
		t.Errorf("the other webhook's counts = %v", got)
	}
	none, updated, err := s.WebhookCountersGet(ctx, "acme:never-"+nonce)
	if err != nil || len(none) != 0 || !updated.IsZero() {
		t.Errorf("a webhook never counted: %v %v %v, want empty", none, updated, err)
	}
	if err := s.WebhookCountersAdd(ctx, nil, t0); err != nil {
		t.Errorf("an empty write: %v", err)
	}
}
