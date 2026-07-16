package main

import (
	"testing"
	"time"
)

func TestStore_GetEnforcesPrincipalOwnership(t *testing.T) {
	s := NewStore(time.Hour, time.Hour)
	now := time.Now()
	s.Add(&Session{ID: "a", Name: "loom-sbx-a", Principal: "op:alice", CreatedAt: now, LastUsed: now})

	if _, ok := s.Get("a", "op:alice", now); !ok {
		t.Fatalf("owner should resolve its own session")
	}
	// A different principal presenting a valid id must NOT reach the session —
	// the P2 cross-tenant-isolation seam.
	if _, ok := s.Get("a", "op:mallory", now); ok {
		t.Fatalf("a foreign principal must not resolve another's session")
	}
	if _, ok := s.Get("missing", "op:alice", now); ok {
		t.Fatalf("unknown id must not resolve")
	}
}

func TestStore_GetTouchesLastUsed(t *testing.T) {
	s := NewStore(time.Hour, time.Hour)
	t0 := time.Now()
	s.Add(&Session{ID: "a", Principal: "p", CreatedAt: t0, LastUsed: t0})
	t1 := t0.Add(time.Minute)
	if _, ok := s.Get("a", "p", t1); !ok {
		t.Fatal("get failed")
	}
	sess, _ := s.Get("a", "p", t1)
	if !sess.LastUsed.Equal(t1) {
		t.Errorf("LastUsed not touched: got %v want %v", sess.LastUsed, t1)
	}
}

func TestStore_ExpiredIdleAndAged(t *testing.T) {
	s := NewStore(10*time.Minute, time.Hour)
	base := time.Now()
	// idle: last used 11m ago (> idleTTL)
	s.Add(&Session{ID: "idle", Principal: "p", CreatedAt: base, LastUsed: base})
	// aged: created 61m ago but recently used (> maxTTL regardless)
	s.Add(&Session{ID: "aged", Principal: "p", CreatedAt: base.Add(-61 * time.Minute), LastUsed: base.Add(9 * time.Minute)})
	// live: fresh
	s.Add(&Session{ID: "live", Principal: "p", CreatedAt: base.Add(9 * time.Minute), LastUsed: base.Add(9 * time.Minute)})

	now := base.Add(11 * time.Minute)
	expired := s.Expired(now)
	got := map[string]bool{}
	for _, e := range expired {
		got[e.ID] = true
	}
	if !got["idle"] || !got["aged"] {
		t.Errorf("expected idle+aged expired, got %v", got)
	}
	if got["live"] {
		t.Errorf("live session must not expire")
	}
	// Expired removes them.
	if s.Count() != 1 {
		t.Errorf("Expired should remove reaped sessions; count=%d want 1", s.Count())
	}
}
