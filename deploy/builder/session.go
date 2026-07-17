package main

import (
	"sync"
	"time"
)

// Session is one live sandbox container the sidecar manages.
type Session struct {
	ID        string // opaque high-entropy id handed to the agent
	Name      string // podman container name (loom-sbx-<id>)
	Principal string // owner key derived from the caller's bearer (P1: one
	//                     shared principal; P2 derives it from the attested tenant)
	Image     string
	Network   string
	Workspace string // durable workspace name (RFC BI P2a); empty = tmpfs /work
	CreatedAt time.Time
	LastUsed  time.Time // touched on every exec/read/write; drives idle TTL
}

// Store is the in-memory session registry. Sessions are also self-describing on
// the engine (loomcycle.managed=1 labels), so a crash-sweeper can reap orphans
// the store lost track of — but the store is the authoritative liveness source
// while the process is up.
type Store struct {
	mu      sync.Mutex
	m       map[string]*Session
	idleTTL time.Duration
	maxTTL  time.Duration
}

func NewStore(idleTTL, maxTTL time.Duration) *Store {
	return &Store{m: map[string]*Session{}, idleTTL: idleTTL, maxTTL: maxTTL}
}

func (s *Store) Add(sess *Session) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[sess.ID] = sess
}

// Get returns the session iff it exists AND is owned by principal — a leaked or
// guessed id from another principal resolves to (nil,false), never another
// tenant's container. Touches LastUsed on success (a use resets the idle clock).
func (s *Store) Get(id, principal string, now time.Time) (*Session, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.m[id]
	if !ok || sess.Principal != principal {
		return nil, false
	}
	sess.LastUsed = now
	return sess, true
}

func (s *Store) Remove(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, id)
}

// Count returns the number of live sessions (global; P1 caps globally).
func (s *Store) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.m)
}

// ListByPrincipal returns a snapshot of one principal's sessions.
func (s *Store) ListByPrincipal(principal string) []*Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*Session
	for _, sess := range s.m {
		if sess.Principal == principal {
			cp := *sess
			out = append(out, &cp)
		}
	}
	return out
}

// Expired returns and REMOVES every session past its idle or absolute TTL, so
// the caller can tear down the containers. Removing under the same lock avoids a
// double-reap race with a concurrent GC tick.
func (s *Store) Expired(now time.Time) []*Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*Session
	for id, sess := range s.m {
		idle := now.Sub(sess.LastUsed) > s.idleTTL
		aged := now.Sub(sess.CreatedAt) > s.maxTTL
		if idle || aged {
			out = append(out, sess)
			delete(s.m, id)
		}
	}
	return out
}
