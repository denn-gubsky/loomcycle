package lifecycle

import (
	"testing"
	"time"
)

const day = 24 * time.Hour

// RFC FA §8 (lifecycle): a removal never skips a stage — the unbind comes
// first whatever the erase time says, and with erase_after: never nothing
// after the unbind is ever due.
func TestNext_StagesNeverSkip(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	r := Policy{UnbindAfter: 7 * day, EraseAfter: 30 * day}.Start(t0)
	if got := Next(r, t0.Add(6*day)); got != StageNone {
		t.Fatalf("before unbind_after: %q", got)
	}
	if got := Next(r, t0.Add(365*day)); got != StageUnbind {
		t.Fatalf("long past both due times, not yet unbound: %q, want unbind first", got)
	}
	r.UnboundAt = t0.Add(7 * day)
	if got := Next(r, t0.Add(29*day)); got != StageNone {
		t.Fatalf("unbound, before erase_after: %q", got)
	}
	if got := Next(r, t0.Add(30*day)); got != StageErase {
		t.Fatalf("unbound, erase due: %q", got)
	}
	r.ErasedAt = t0.Add(30 * day)
	if got := Next(r, t0.Add(365*day)); got != StageNone {
		t.Fatalf("erased: %q", got)
	}

	never := Policy{UnbindAfter: 7 * day}.Start(t0)
	if !never.EraseDueAt.IsZero() {
		t.Fatalf("erase_after never scheduled an erase at %v", never.EraseDueAt)
	}
	never.UnboundAt = t0.Add(7 * day)
	if got := Next(never, t0.Add(100*365*day)); got != StageNone {
		t.Fatalf("erase_after never: %q came due", got)
	}
}

func TestPolicyStart_ErasesNoEarlierThanTheUnbind(t *testing.T) {
	t0 := time.Now()
	r := Policy{UnbindAfter: 7 * day, EraseAfter: day}.Start(t0)
	if r.EraseDueAt.Before(r.UnbindDueAt) {
		t.Fatalf("erase due %v before unbind due %v", r.EraseDueAt, r.UnbindDueAt)
	}
}

func TestMissing_ListsOnlyIdentitiesTheSyncNoLongerAsserts(t *testing.T) {
	ids := []Identity{
		{TenantID: "b", Subject: "tg:3", ExternalSubject: "3"},
		{TenantID: "a", Subject: "tg:1", ExternalSubject: "1"},
		{TenantID: "a", Subject: "tg:2", ExternalSubject: "2"},
	}
	got := Missing(ids, []string{"2"})
	if len(got) != 2 || got[0].Subject != "tg:1" || got[1].Subject != "tg:3" {
		t.Fatalf("missing = %+v", got)
	}
	if got := Missing(ids, []string{"1", "2", "3"}); len(got) != 0 {
		t.Fatalf("a full sync removed %+v", got)
	}
}
