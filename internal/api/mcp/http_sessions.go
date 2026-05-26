package mcp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"
)

// defaultHTTPSessionTTL is the inactivity window after which an HTTP
// MCP session is reaped by the sweeper. Picked at 30 minutes per the
// v0.8.15.3 RFC decision C2: long enough to handle operators leaving
// a session open during a coffee break, short enough that abandoned
// sessions don't accumulate. Operators with longer interactive workflows
// can revisit this in v0.9.x if it becomes an operational pain point.
const defaultHTTPSessionTTL = 30 * time.Minute

// HTTPSessionStore tracks active HTTP MCP sessions by Mcp-Session-Id.
// Sessions live until the inactivity TTL elapses since their last
// Get / Create call, at which point the periodic Sweep removes them.
//
// Stdio MCP doesn't need this — there's exactly one Session per
// loomcycle-mcp process for its lifetime. HTTP MCP is multi-tenant:
// any number of concurrent clients can each have their own session.
//
// Safe for concurrent use.
type HTTPSessionStore struct {
	mu       sync.Mutex
	sessions map[string]*httpSessionEntry
	ttl      time.Duration
}

type httpSessionEntry struct {
	session    *Session
	lastAccess time.Time
}

// NewHTTPSessionStore returns an empty store with the given inactivity
// TTL. Pass 0 to use the default (30 minutes).
func NewHTTPSessionStore(ttl time.Duration) *HTTPSessionStore {
	if ttl <= 0 {
		ttl = defaultHTTPSessionTTL
	}
	return &HTTPSessionStore{
		sessions: make(map[string]*httpSessionEntry),
		ttl:      ttl,
	}
}

// Create stores a new session and returns the generated session ID.
// The ID is a UUIDv4-shaped 36-character string (canonical hex-with-
// dashes form). The MCP Streamable HTTP spec doesn't mandate UUID
// shape but it's the conventional choice and existing client SDKs
// (including ours at internal/tools/mcp/http/client.go) handle it
// without special-casing.
func (s *HTTPSessionStore) Create(sess *Session) string {
	id := newSessionID()
	s.mu.Lock()
	s.sessions[id] = &httpSessionEntry{
		session:    sess,
		lastAccess: time.Now(),
	}
	s.mu.Unlock()
	return id
}

// Get looks up a session by ID. Returns (session, true) when present
// AND not yet expired; (nil, false) otherwise. The lookup updates the
// session's lastAccess time so active sessions don't get reaped while
// they're in the middle of a long-running tools/call.
//
// Already-expired sessions return (nil, false) WITHOUT being eagerly
// deleted — the periodic Sweep handles cleanup. This avoids a write
// under the read path and matches the v0.8.0 Memory tool's
// "expires-at filtering at read time" pattern.
func (s *HTTPSessionStore) Get(id string) (*Session, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.sessions[id]
	if !ok {
		return nil, false
	}
	now := time.Now()
	if now.Sub(entry.lastAccess) > s.ttl {
		return nil, false
	}
	entry.lastAccess = now
	return entry.session, true
}

// Delete removes a session by ID. Idempotent — deleting a missing key
// is a no-op. Called when a client sends DELETE /v1/_mcp with its
// Mcp-Session-Id header (graceful session teardown per the MCP spec).
func (s *HTTPSessionStore) Delete(id string) {
	s.mu.Lock()
	delete(s.sessions, id)
	s.mu.Unlock()
}

// Sweep removes sessions whose lastAccess is older than the TTL.
// Returns the number of entries deleted. Designed to be called from a
// background ticker (typically every 5 minutes — much coarser than the
// 30-minute TTL so a session has at least ~25 minutes between its last
// access and its earliest possible reaping).
func (s *HTTPSessionStore) Sweep() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	cutoff := time.Now().Add(-s.ttl)
	n := 0
	for id, entry := range s.sessions {
		if entry.lastAccess.Before(cutoff) {
			delete(s.sessions, id)
			n++
		}
	}
	return n
}

// Len returns the number of live (non-swept) sessions. Exposed for
// tests and future observability (e.g., a /v1/_metrics surface for
// HTTP MCP session counts).
func (s *HTTPSessionStore) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sessions)
}

// newSessionID returns a fresh UUIDv4-shaped string. RFC 4122 section
// 4.4 specifies version 4 in the high nibble of byte 6, and variant
// 10xx in the high two bits of byte 8. The dashed-hex layout
// (8-4-4-4-12) is the conventional canonical form. We don't depend on
// google/uuid for this because crypto/rand + 6 lines of bit-twiddling
// is smaller than the import.
func newSessionID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand.Read should never fail; if it does the process
		// has bigger problems than session naming. Fall back to a
		// non-random ID rather than panic — the caller can still
		// dispatch the request, just with a predictable session ID.
		// In practice this branch is unreachable.
		return "00000000-0000-0000-0000-000000000000"
	}
	// Version 4: top nibble of byte 6 is 0100.
	b[6] = (b[6] & 0x0f) | 0x40
	// Variant 10: top two bits of byte 8 are 10.
	b[8] = (b[8] & 0x3f) | 0x80
	var s [36]byte
	hex.Encode(s[0:8], b[0:4])
	s[8] = '-'
	hex.Encode(s[9:13], b[4:6])
	s[13] = '-'
	hex.Encode(s[14:18], b[6:8])
	s[18] = '-'
	hex.Encode(s[19:23], b[8:10])
	s[23] = '-'
	hex.Encode(s[24:36], b[10:16])
	return string(s[:])
}

// RunHTTPSessionSweeper starts a periodic goroutine that calls
// store.Sweep() at the given interval. Returns immediately; the
// goroutine exits when ctx is done.
//
// interval=0 disables the sweeper — expired sessions linger in the
// map but Get filters them at read time, so functional correctness
// is preserved (only storage reclamation is forgone). For an
// HTTP MCP server with low session volume this is acceptable.
func RunHTTPSessionSweeper(ctx context.Context, store *HTTPSessionStore, interval time.Duration, logf func(string, ...any)) {
	if interval <= 0 || store == nil {
		return
	}
	if logf == nil {
		logf = defaultLogf
	}
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if n := store.Sweep(); n > 0 {
					logf("http_mcp: swept %d expired session(s)", n)
				}
			}
		}
	}()
}
