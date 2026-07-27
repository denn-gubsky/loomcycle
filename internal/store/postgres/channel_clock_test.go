package postgres

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// channelRead filters `visible_at <= NOW()` on the database's clock, so
// ChannelPublish must stamp visible_at from that same clock. It used to stamp
// it from the application process's clock, and where the application host led
// the database — a few ms is routine with a containerised Postgres — a
// just-published message sat in the database's future and was invisible to the
// publisher's own next read.
//
// Ambient drift is the wrong instrument for pinning that down: it is small,
// it changes direction within the hour, and a test that only fails when the
// developer's VM happens to be skewed is the same class of defect as the bug.
// So these tests inject the skew instead, displacing the process clock by a
// decade — far larger than any real drift, which makes the assertions
// deterministic rather than probabilistic.
//
// A decade is also well outside the ±1h that the deferred-publish contract
// arms use, so the two cases can't be confused for each other.
const clockSkewProbe = 10 * 365 * 24 * time.Hour

// pinClock displaces the store's process clock by delta, standing in for a
// host whose clock leads (or trails) the database's. The clock still advances
// in real time, so ids stay monotonic and cursors behave.
func pinClock(s *Store, delta time.Duration) {
	s.nowFn = func() time.Time { return time.Now().Add(delta) }
}

// An immediate publish is visible to the publisher's own next read even when
// the publishing host's clock runs far ahead of the database's. Before the
// fix, visible_at was stamped from the process clock, so the row landed a
// decade in the database's future and the read returned nothing.
func TestChannelPublish_ImmediateIsVisibleDespiteHostClockAhead(t *testing.T) {
	dsn := pgDSNFromEnv(t)
	fix := freshSchema(t, dsn)
	defer fix.cleanup()
	s := fix.store.(*Store)
	pinClock(s, clockSkewProbe)

	ctx := context.Background()
	if _, _, err := s.ChannelPublish(ctx, store.ChannelMessage{
		Channel: "clock-ahead", Scope: store.MemoryScopeAgent, ScopeID: "x",
		Payload: json.RawMessage(`"published-from-a-leading-clock"`),
	}, 0); err != nil {
		t.Fatalf("publish: %v", err)
	}

	msgs, _, err := s.ChannelSubscribe(ctx, "clock-ahead", store.MemoryScopeAgent, "x", "", 10)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("subscribe returned %d msgs, want 1: an immediate publish must be "+
			"visible to the next read regardless of how far the publishing host's "+
			"clock leads the database's", len(msgs))
	}
}

// The mirror: a host whose clock TRAILS the database must not have its
// messages back-dated below what a concurrent publisher would see. Stamping
// server-side means the row carries the database's NOW() either way.
func TestChannelPublish_ImmediateIsVisibleDespiteHostClockBehind(t *testing.T) {
	dsn := pgDSNFromEnv(t)
	fix := freshSchema(t, dsn)
	defer fix.cleanup()
	s := fix.store.(*Store)
	pinClock(s, -clockSkewProbe)

	ctx := context.Background()
	if _, _, err := s.ChannelPublish(ctx, store.ChannelMessage{
		Channel: "clock-behind", Scope: store.MemoryScopeAgent, ScopeID: "x",
		Payload: json.RawMessage(`"published-from-a-trailing-clock"`),
	}, 0); err != nil {
		t.Fatalf("publish: %v", err)
	}

	msgs, _, err := s.ChannelSubscribe(ctx, "clock-behind", store.MemoryScopeAgent, "x", "", 10)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("subscribe returned %d msgs, want 1", len(msgs))
	}
	// Server-stamped, so the row is dated ~now and not a decade ago.
	if age := time.Since(msgs[0].VisibleAt); age > time.Hour {
		t.Errorf("visible_at is %v old — it was stamped from the trailing process "+
			"clock, not the server's", age)
	}
}

// The server-clock stamp must NOT swallow a deferred publish: a caller-supplied
// FUTURE visible_at is a schedule, not a clock reading, and has to survive
// verbatim. This is the case a naive "always stamp NOW()" fix would break, and
// it is why the write uses GREATEST rather than an unconditional NOW().
func TestChannelPublish_DeferredKeepsCallerSuppliedVisibleAt(t *testing.T) {
	dsn := pgDSNFromEnv(t)
	fix := freshSchema(t, dsn)
	defer fix.cleanup()
	s := fix.store.(*Store)

	ctx := context.Background()
	deferTo := time.Now().Add(2 * time.Hour)
	if _, _, err := s.ChannelPublish(ctx, store.ChannelMessage{
		Channel: "deferred", Scope: store.MemoryScopeAgent, ScopeID: "x",
		Payload:   json.RawMessage(`"later"`),
		VisibleAt: deferTo,
	}, 0); err != nil {
		t.Fatalf("publish deferred: %v", err)
	}

	msgs, _, err := s.ChannelSubscribe(ctx, "deferred", store.MemoryScopeAgent, "x", "", 10)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if len(msgs) != 0 {
		t.Fatalf("a publish deferred 2h was delivered immediately (%d msgs) — the "+
			"server-clock stamp must not collapse a deferred publish", len(msgs))
	}

	// And it is stored at the caller's instant, not clamped to NOW().
	rows, err := s.ChannelPeek(ctx, "deferred", store.MemoryScopeAgent, "x", "cur_0", 10)
	if err != nil {
		t.Fatalf("peek: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("peek surfaced %d deferred rows, want 0 (peek honours visibility too)", len(rows))
	}
}

// A caller-supplied visible_at already in the PAST means "now" — it must be
// clamped up to the server clock, not stored as a stale instant that would
// sort ahead of everything else in the (visible_at, id) delivery order.
func TestChannelPublish_PastVisibleAtClampsToServerNow(t *testing.T) {
	dsn := pgDSNFromEnv(t)
	fix := freshSchema(t, dsn)
	defer fix.cleanup()
	s := fix.store.(*Store)

	ctx := context.Background()
	if _, _, err := s.ChannelPublish(ctx, store.ChannelMessage{
		Channel: "past", Scope: store.MemoryScopeAgent, ScopeID: "x",
		Payload:   json.RawMessage(`"past"`),
		VisibleAt: time.Now().Add(-72 * time.Hour),
	}, 0); err != nil {
		t.Fatalf("publish: %v", err)
	}

	msgs, _, err := s.ChannelSubscribe(ctx, "past", store.MemoryScopeAgent, "x", "", 10)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("subscribe returned %d msgs, want 1", len(msgs))
	}
	if age := time.Since(msgs[0].VisibleAt); age > time.Hour {
		t.Errorf("visible_at kept the caller's 72h-old instant (age %v); a past "+
			"visible_at must clamp to the server's NOW()", age)
	}
}
