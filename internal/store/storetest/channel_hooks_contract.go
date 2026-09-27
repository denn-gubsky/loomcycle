package storetest

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// Channel hooks: a message awaiting its channel's hooks is stored at the hook
// instant, invisible to every read, until a hook chain releases or drops it
// with a compare-and-set; its chain's lease and progress live beside it.

// publishAwaitingHook stores one message awaiting hooks on (ch, agent, "x")
// and returns its key.
func publishAwaitingHook(t *testing.T, s store.Store, ch string, payload string, mut func(*store.ChannelMessage)) store.ChannelMessageKey {
	t.Helper()
	m := store.ChannelMessage{
		Channel: ch, Scope: store.MemoryScopeAgent, ScopeID: "x",
		Payload: json.RawMessage(payload), VisibleAt: store.ChannelHookHeldVisibleAt(),
	}
	if mut != nil {
		mut(&m)
	}
	id, _, err := s.ChannelPublish(context.Background(), m, 0)
	if err != nil {
		t.Fatalf("publish awaiting hook: %v", err)
	}
	return store.ChannelMessageKey{TenantID: store.ChannelScopeTenant(m.TenantID, m.Scope), Channel: ch, Scope: m.Scope, ScopeID: m.ScopeID, ID: id}
}

func readAll(t *testing.T, s store.Store, ch, cursor string) ([]store.ChannelMessage, string) {
	t.Helper()
	msgs, next, err := s.ChannelSubscribe(context.Background(), "", ch, store.MemoryScopeAgent, "x", cursor, 100)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	return msgs, next
}

func testChannelHookHeldIsInvisibleToReads(t *testing.T, s store.Store) {
	publishAwaitingHook(t, s, "hk-inv", `{"a":1}`, nil)
	if msgs, _ := readAll(t, s, "hk-inv", ""); len(msgs) != 0 {
		t.Errorf("subscribe delivered %d message(s) awaiting hooks", len(msgs))
	}
	peek, err := s.ChannelPeek(context.Background(), "", "hk-inv", store.MemoryScopeAgent, "x", "cur_0", 100)
	if err != nil || len(peek) != 0 {
		t.Errorf("peek delivered %d message(s) awaiting hooks (err %v)", len(peek), err)
	}
}

// The operator's release takes held messages only: a message no hook has
// decided cannot be released past its hooks.
func testChannelReleaseNeverTouchesHookHeld(t *testing.T, s store.Store) {
	ctx := context.Background()
	publishAwaitingHook(t, s, "hk-rel", `"hooked"`, nil)
	if _, _, err := s.ChannelPublish(ctx, store.ChannelMessage{
		Channel: "hk-rel", Scope: store.MemoryScopeAgent, ScopeID: "x",
		Payload: json.RawMessage(`"held"`), VisibleAt: store.ChannelHeldVisibleAt(),
	}, 0); err != nil {
		t.Fatalf("publish held: %v", err)
	}
	released, stillHeld, err := s.ChannelRelease(ctx, "", "hk-rel", store.MemoryScopeAgent, "x", 10)
	if err != nil {
		t.Fatalf("release: %v", err)
	}
	if len(released) != 1 || stillHeld != 0 {
		t.Errorf("released %d, %d still held; want the 1 held message and nothing else", len(released), stillHeld)
	}
	msgs, _ := readAll(t, s, "hk-rel", "")
	if len(msgs) != 1 || string(msgs[0].Payload) != `"held"` {
		t.Errorf("delivered %v, want only the held message", msgs)
	}
}

// A released message lands after anything a subscriber has already read —
// even when asked for a time in the past — so no cursor has to rewind.
func testChannelReleaseHookHeldLandsAfterAProgressedCursor(t *testing.T, s store.Store) {
	ctx := context.Background()
	key := publishAwaitingHook(t, s, "hk-cur", `"late"`, nil)
	time.Sleep(time.Millisecond)
	if _, _, err := s.ChannelPublish(ctx, store.ChannelMessage{
		Channel: "hk-cur", Scope: store.MemoryScopeAgent, ScopeID: "x", Payload: json.RawMessage(`"early"`),
	}, 0); err != nil {
		t.Fatalf("publish: %v", err)
	}
	first, cursor := readAll(t, s, "hk-cur", "")
	if len(first) != 1 {
		t.Fatalf("first read got %d, want 1", len(first))
	}
	ok, err := s.ChannelReleaseHookHeld(ctx, key, nil, time.Unix(1, 0))
	if err != nil || !ok {
		t.Fatalf("release: ok=%v err=%v", ok, err)
	}
	next, _ := readAll(t, s, "hk-cur", cursor)
	if len(next) != 1 || string(next[0].Payload) != `"late"` {
		t.Errorf("after the cursor: %v, want the released message", next)
	}
}

// A decision takes effect once: a second release, or a drop after a release,
// changes nothing.
func testChannelReleaseHookHeldIsCompareAndSet(t *testing.T, s store.Store) {
	ctx := context.Background()
	key := publishAwaitingHook(t, s, "hk-cas", `"x"`, nil)
	for i, want := range []bool{true, false} {
		ok, err := s.ChannelReleaseHookHeld(ctx, key, nil, time.Time{})
		if err != nil || ok != want {
			t.Fatalf("release %d: ok=%v err=%v, want %v", i, ok, err, want)
		}
	}
	if ok, err := s.ChannelDropHookHeld(ctx, key); err != nil || ok {
		t.Errorf("drop after release: ok=%v err=%v, want false", ok, err)
	}
	if msgs, _ := readAll(t, s, "hk-cas", ""); len(msgs) != 1 {
		t.Errorf("delivered %d, want the 1 released message", len(msgs))
	}
}

func testChannelReleaseHookHeldRewritesThePayload(t *testing.T, s store.Store) {
	ctx := context.Background()
	key := publishAwaitingHook(t, s, "hk-rw", `{"secret":"s3"}`, nil)
	if ok, err := s.ChannelReleaseHookHeld(ctx, key, json.RawMessage(`{"secret":"***"}`), time.Time{}); err != nil || !ok {
		t.Fatalf("release: ok=%v err=%v", ok, err)
	}
	msgs, _ := readAll(t, s, "hk-rw", "")
	if len(msgs) != 1 {
		t.Fatalf("delivered %d, want 1", len(msgs))
	}
	var got map[string]string
	if err := json.Unmarshal(msgs[0].Payload, &got); err != nil || got["secret"] != "***" {
		t.Errorf("delivered %s, want the rewritten body", msgs[0].Payload)
	}
}

// A channel that holds as well as hooks: the hooks release to the Hold
// instant, where the operator's release takes over.
func testChannelReleaseHookHeldToTheHoldInstant(t *testing.T, s store.Store) {
	ctx := context.Background()
	key := publishAwaitingHook(t, s, "hk-hold", `"x"`, nil)
	if ok, err := s.ChannelReleaseHookHeld(ctx, key, nil, store.ChannelHeldVisibleAt()); err != nil || !ok {
		t.Fatalf("release: ok=%v err=%v", ok, err)
	}
	if msgs, _ := readAll(t, s, "hk-hold", ""); len(msgs) != 0 {
		t.Fatalf("delivered %d past the hold", len(msgs))
	}
	released, _, err := s.ChannelRelease(ctx, "", "hk-hold", store.MemoryScopeAgent, "x", 10)
	if err != nil || len(released) != 1 {
		t.Errorf("the operator's release got %d (err %v), want the message", len(released), err)
	}
}

func testChannelReleaseHookHeldSkipsExpired(t *testing.T, s store.Store) {
	key := publishAwaitingHook(t, s, "hk-exp", `"x"`, func(m *store.ChannelMessage) {
		m.ExpiresAt = time.Now().Add(-time.Minute)
	})
	if ok, err := s.ChannelReleaseHookHeld(context.Background(), key, nil, time.Time{}); err != nil || ok {
		t.Errorf("released an expired message: ok=%v err=%v", ok, err)
	}
}

// A drop deletes only a message still awaiting a decision.
func testChannelDropHookHeldOnlyDropsUndecided(t *testing.T, s store.Store) {
	ctx := context.Background()
	id, _, err := s.ChannelPublish(ctx, store.ChannelMessage{
		Channel: "hk-drop", Scope: store.MemoryScopeAgent, ScopeID: "x", Payload: json.RawMessage(`"delivered"`),
	}, 0)
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	delivered := store.ChannelMessageKey{Channel: "hk-drop", Scope: store.MemoryScopeAgent, ScopeID: "x", ID: id}
	if ok, err := s.ChannelDropHookHeld(ctx, delivered); err != nil || ok {
		t.Errorf("dropped a delivered message: ok=%v err=%v", ok, err)
	}
	key := publishAwaitingHook(t, s, "hk-drop", `"spam"`, nil)
	if ok, err := s.ChannelDropHookHeld(ctx, key); err != nil || !ok {
		t.Errorf("drop: ok=%v err=%v", ok, err)
	}
	msgs, _ := readAll(t, s, "hk-drop", "")
	if len(msgs) != 1 || string(msgs[0].Payload) != `"delivered"` {
		t.Errorf("after the drop: %v, want only the delivered message", msgs)
	}
	if ok, _ := s.ChannelReleaseHookHeld(ctx, key, nil, time.Time{}); ok {
		t.Errorf("released a dropped message")
	}
}

// A message is leased to one owner at a time, oldest first; an expired lease
// can be taken over.
func testChannelHookClaimLeasesEachMessageOnce(t *testing.T, s store.Store) {
	ctx := context.Background()
	var keys []store.ChannelMessageKey
	for i := 0; i < 3; i++ {
		keys = append(keys, publishAwaitingHook(t, s, "hk-claim", fmt.Sprintf(`%d`, i), nil))
		time.Sleep(time.Millisecond)
	}
	now := time.Now()
	a, err := s.ChannelHookClaim(ctx, "a", now, now.Add(time.Minute), 10)
	if err != nil || len(a) != 3 {
		t.Fatalf("first claim: %d (err %v), want 3", len(a), err)
	}
	for i, w := range a {
		if w.Message.ID != keys[i].ID {
			t.Errorf("claim order: #%d is %s, want %s (oldest first)", i, w.Message.ID, keys[i].ID)
		}
	}
	if b, err := s.ChannelHookClaim(ctx, "b", now, now.Add(time.Minute), 10); err != nil || len(b) != 0 {
		t.Errorf("a second owner claimed %d leased message(s) (err %v)", len(b), err)
	}
	later := now.Add(2 * time.Minute)
	if b, err := s.ChannelHookClaim(ctx, "b", later, later.Add(time.Minute), 10); err != nil || len(b) != 3 {
		t.Errorf("after the leases expired: claimed %d (err %v), want 3", len(b), err)
	}
}

// Concurrent claimers never both take a message.
func testChannelHookClaimIsExclusiveUnderConcurrency(t *testing.T, s store.Store) {
	ctx := context.Background()
	const (
		n        = 20
		claimers = 8
		rounds   = 5
	)
	var (
		mu   sync.Mutex
		seen = map[string]int{}
	)
	// Each round publishes a fresh batch, then starts every claimer at once
	// asking for all of it, so their candidate reads overlap: a claim that
	// re-checked nothing at write time would hand one message to two of
	// them. Later rounds run on connections the first one opened, which is
	// when the claimers truly race.
	for r := 0; r < rounds; r++ {
		for i := 0; i < n; i++ {
			publishAwaitingHook(t, s, "hk-conc", fmt.Sprintf(`%d`, r*n+i), nil)
		}
		now := time.Now()
		start := make(chan struct{})
		var wg sync.WaitGroup
		for g := 0; g < claimers; g++ {
			wg.Add(1)
			go func(owner string) {
				defer wg.Done()
				<-start
				for tries := 0; tries < 5; tries++ {
					got, err := s.ChannelHookClaim(ctx, owner, now, now.Add(time.Hour), n)
					if err != nil {
						continue // a busy backend: try again
					}
					mu.Lock()
					for _, w := range got {
						seen[w.Message.ID]++
					}
					mu.Unlock()
				}
			}(fmt.Sprintf("w%d", g))
		}
		close(start)
		wg.Wait()
	}
	if len(seen) != n*rounds {
		t.Errorf("claimed %d distinct messages, want all %d", len(seen), n*rounds)
	}
	for id, c := range seen {
		if c != 1 {
			t.Errorf("message %s claimed %d times", id, c)
		}
	}
}

// Progress is the lease owner's to write; it round-trips through a claim.
func testChannelHookProgressIsOwnerChecked(t *testing.T, s store.Store) {
	ctx := context.Background()
	key := publishAwaitingHook(t, s, "hk-prog", `"x"`, nil)
	now := time.Now()
	if w, err := s.ChannelHookClaim(ctx, "a", now, now.Add(time.Minute), 1); err != nil || len(w) != 1 {
		t.Fatalf("claim: %d (err %v)", len(w), err)
	}
	p := store.ChannelHookProgress{
		RunID: "r_1", ChainPos: 2, Body: json.RawMessage(`{"b":1}`), Journal: json.RawMessage(`[{"q":"?"}]`),
		Attempts: 1, LastError: "timeout",
	}
	if ok, err := s.ChannelHookSaveProgress(ctx, key, "b", p, now.Add(time.Minute)); err != nil || ok {
		t.Errorf("a non-owner saved progress: ok=%v err=%v", ok, err)
	}
	if ok, err := s.ChannelHookRenew(ctx, key, "b", now.Add(time.Hour)); err != nil || ok {
		t.Errorf("a non-owner renewed: ok=%v err=%v", ok, err)
	}
	if ok, err := s.ChannelHookRenew(ctx, key, "a", now.Add(time.Hour)); err != nil || !ok {
		t.Errorf("the owner could not renew: ok=%v err=%v", ok, err)
	}
	// Saving with a lease already past gives it up.
	if ok, err := s.ChannelHookSaveProgress(ctx, key, "a", p, now.Add(-time.Second)); err != nil || !ok {
		t.Fatalf("the owner could not save progress: ok=%v err=%v", ok, err)
	}
	w, err := s.ChannelHookClaim(ctx, "b", now, now.Add(time.Minute), 1)
	if err != nil || len(w) != 1 {
		t.Fatalf("reclaim: %d (err %v)", len(w), err)
	}
	got := w[0].Progress
	if got.RunID != "r_1" || got.ChainPos != 2 || got.Attempts != 1 || got.LastError != "timeout" ||
		!jsonEqual(got.Body, `{"b":1}`) || !jsonEqual(got.Journal, `[{"q":"?"}]`) {
		t.Errorf("progress round-trip: %+v", got)
	}
}

// A retry's backoff: the message is not claimable before its next attempt.
func testChannelHookClaimHonoursTheNextAttempt(t *testing.T, s store.Store) {
	ctx := context.Background()
	key := publishAwaitingHook(t, s, "hk-next", `"x"`, nil)
	now := time.Now()
	if w, _ := s.ChannelHookClaim(ctx, "a", now, now.Add(time.Minute), 1); len(w) != 1 {
		t.Fatalf("claim: %d", len(w))
	}
	retryAt := now.Add(10 * time.Minute)
	if ok, err := s.ChannelHookSaveProgress(ctx, key, "a", store.ChannelHookProgress{Attempts: 1, NextAttemptAt: retryAt}, now); err != nil || !ok {
		t.Fatalf("save: ok=%v err=%v", ok, err)
	}
	soon := now.Add(time.Minute)
	if w, _ := s.ChannelHookClaim(ctx, "b", soon, soon.Add(time.Minute), 1); len(w) != 0 {
		t.Errorf("claimed before the next attempt")
	}
	after := retryAt.Add(time.Second)
	if w, _ := s.ChannelHookClaim(ctx, "b", after, after.Add(time.Minute), 1); len(w) != 1 {
		t.Errorf("not claimable after the next attempt")
	}
}

func testChannelHookClaimSkipsExpired(t *testing.T, s store.Store) {
	publishAwaitingHook(t, s, "hk-cexp", `"x"`, func(m *store.ChannelMessage) {
		m.ExpiresAt = time.Now().Add(-time.Minute)
	})
	now := time.Now()
	if w, err := s.ChannelHookClaim(context.Background(), "a", now, now.Add(time.Minute), 10); err != nil || len(w) != 0 {
		t.Errorf("claimed %d expired message(s) (err %v)", len(w), err)
	}
}

// A message carries its hook context — origin, the governing tenant, the
// requested delivery time — into a claim and through a snapshot.
func testChannelHookContextRoundTrips(t *testing.T, s store.Store) {
	ctx := context.Background()
	requested := time.Now().Add(time.Hour).Truncate(time.Microsecond)
	publishAwaitingHook(t, s, "hk-meta", `"x"`, func(m *store.ChannelMessage) {
		m.Origin, m.HookTenant, m.RequestedVisibleAt = "starter_sink", "t-owner", requested
	})
	check := func(where string, m store.ChannelMessage) {
		t.Helper()
		if m.Origin != "starter_sink" || m.HookTenant != "t-owner" || !m.RequestedVisibleAt.Equal(requested) {
			t.Errorf("%s: origin=%q hook_tenant=%q requested=%v", where, m.Origin, m.HookTenant, m.RequestedVisibleAt)
		}
	}
	snap, err := s.SnapshotReadChannelMessages(ctx)
	if err != nil {
		t.Fatalf("snapshot read: %v", err)
	}
	found := false
	for _, m := range snap {
		if m.Channel == "hk-meta" {
			found = true
			check("snapshot read", m)
			m.ID = store.MintChannelMessageID(time.Now())
			if ok, err := s.SnapshotRestoreChannelMessage(ctx, m); err != nil || !ok {
				t.Fatalf("snapshot restore: ok=%v err=%v", ok, err)
			}
		}
	}
	if !found {
		t.Fatal("the snapshot read did not return the message")
	}
	now := time.Now()
	w, err := s.ChannelHookClaim(ctx, "a", now, now.Add(time.Minute), 10)
	if err != nil || len(w) != 2 {
		t.Fatalf("claim: %d (err %v), want the original and the restored copy", len(w), err)
	}
	for _, x := range w {
		check("claim", x.Message)
	}
}

// Purging a channel takes its hook progress with it.
func testChannelPurgeCascadesHookState(t *testing.T, s store.Store) {
	ctx := context.Background()
	publishAwaitingHook(t, s, "hk-purge", `"x"`, nil)
	now := time.Now()
	if w, _ := s.ChannelHookClaim(ctx, "a", now, now.Add(time.Minute), 1); len(w) != 1 {
		t.Fatalf("claim: %d", len(w))
	}
	if _, err := s.ChannelPurge(ctx, "", "hk-purge"); err != nil {
		t.Fatalf("purge: %v", err)
	}
	if n, err := s.ChannelHookGC(ctx, 100); err != nil || n != 0 {
		t.Errorf("after a purge, GC still found %d progress row(s) (err %v) — the purge left them", n, err)
	}
}

func testChannelsDeleteCascadesHookState(t *testing.T, s store.Store) {
	ctx := context.Background()
	if err := s.ChannelsCreate(ctx, store.ChannelRow{Name: "hk-del", Scope: "agent", Semantic: "queue"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	publishAwaitingHook(t, s, "hk-del", `"x"`, nil)
	now := time.Now()
	if w, _ := s.ChannelHookClaim(ctx, "a", now, now.Add(time.Minute), 1); len(w) != 1 {
		t.Fatalf("claim: %d", len(w))
	}
	if err := s.ChannelsDelete(ctx, "", "hk-del"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if n, err := s.ChannelHookGC(ctx, 100); err != nil || n != 0 {
		t.Errorf("after a delete, GC still found %d progress row(s) (err %v)", n, err)
	}
}

// GC removes progress rows whose message is gone; it keeps live ones.
func testChannelHookGCRemovesOrphans(t *testing.T, s store.Store) {
	ctx := context.Background()
	publishAwaitingHook(t, s, "hk-gc", `"keep"`, nil)
	publishAwaitingHook(t, s, "hk-gc", `"expire"`, func(m *store.ChannelMessage) {
		m.ExpiresAt = time.Now().Add(150 * time.Millisecond)
	})
	now := time.Now()
	if w, _ := s.ChannelHookClaim(ctx, "a", now, now.Add(time.Minute), 10); len(w) != 2 {
		t.Fatalf("claim: %d, want 2", len(w))
	}
	time.Sleep(300 * time.Millisecond)
	if _, err := s.ChannelSweepExpired(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n, err := s.ChannelHookGC(ctx, 100); err != nil || n != 1 {
		t.Errorf("GC deleted %d (err %v), want the 1 orphaned row", n, err)
	}
	if n, err := s.ChannelHookGC(ctx, 100); err != nil || n != 0 {
		t.Errorf("a second GC deleted %d (err %v), want 0 — the live row must stay", n, err)
	}
}
