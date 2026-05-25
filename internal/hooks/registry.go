package hooks

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strings"
	"sync"
	"time"
)

// ErrInvalidRegistration is returned when a Register call carries
// missing or malformed fields. Callers map this to HTTP 400.
var ErrInvalidRegistration = errors.New("invalid hook registration")

// ErrNotFound is returned by Delete when no hook with the given id
// is registered. Callers map this to HTTP 404.
var ErrNotFound = errors.New("hook not found")

// RegistryInterface is the v0.12.5 Phase 6 interface extracted from
// the concrete Registry. The Dispatcher holds RegistryInterface so
// either the in-process Registry (single-replica) or the new
// DBBackedRegistry (cluster mode) can sit behind it.
//
// All existing callers of *Registry compile unchanged — Registry
// implicitly satisfies this interface.
type RegistryInterface interface {
	Register(h *Hook) (string, error)
	Delete(id string) error
	List() []*Hook
	Match(agent, tool string, phase Phase) []*Hook
	IsHostWidenPermitted(owner string) bool
}

// Registry holds the set of currently-registered hooks. In-memory
// only — registrations do NOT survive a loomcycle restart, by
// design: registering apps re-establish their hooks on their own
// startup, and an app that's down can't process callbacks anyway.
//
// Concurrency: Register / Delete take the write lock; Match takes
// the read lock. Match is on the hot path (every tool call) so the
// implementation favours read-heavy access patterns.
//
// hostWidenPermitted is an operator-yaml-derived set (loaded once at
// New time, never mutated post-construction). The dispatcher consults
// it via IsHostWidenPermitted() to decide whether a Pre-hook's
// allow_hosts is honoured. Read access is lock-free — the set is
// frozen.
type Registry struct {
	mu    sync.RWMutex
	byID  map[string]*Hook    // id → hook, for DELETE / GET by id
	byKey map[ownerName]*Hook // (owner, name) → hook, for replace-on-conflict
	order []string            // ids in registration order; chain order in Match()

	hostWidenPermitted map[string]struct{} // owner UID set; frozen post-construction
}

type ownerName struct {
	Owner string
	Name  string
}

// NewRegistry constructs an empty Registry with the default policy
// (no host-widen permissions). Use NewRegistryWithPermissions to wire
// the operator-yaml permit list at server New time.
func NewRegistry() *Registry {
	return NewRegistryWithPermissions(nil)
}

// NewRegistryWithPermissions constructs a Registry with the operator's
// host-widen permit list baked in. permitHostWidenOwners is the
// exact-match set of registered-hook owner UIDs whose Pre-hook
// allow_hosts is honoured at dispatch time. nil / empty = no widening
// permitted for anyone (default-deny stance).
//
// The set is built once at construction and never mutated — so the
// trust boundary is "what the operator declared at boot", not "what
// some runtime API ended up writing." That property is load-bearing
// for CLAUDE.md rule #8.
func NewRegistryWithPermissions(permitHostWidenOwners []string) *Registry {
	permit := make(map[string]struct{}, len(permitHostWidenOwners))
	for _, owner := range permitHostWidenOwners {
		owner = strings.TrimSpace(owner)
		if owner == "" {
			continue
		}
		permit[owner] = struct{}{}
	}
	return &Registry{
		byID:               make(map[string]*Hook),
		byKey:              make(map[ownerName]*Hook),
		hostWidenPermitted: permit,
	}
}

// IsHostWidenPermitted reports whether the given hook Owner is on the
// operator-yaml permit list (exact-string match, no globs). Used by
// the dispatcher to decide whether to honour a Pre-hook's allow_hosts
// field. False for any owner not explicitly listed at server boot —
// including the empty string.
func (r *Registry) IsHostWidenPermitted(owner string) bool {
	if r == nil || r.hostWidenPermitted == nil {
		return false
	}
	_, ok := r.hostWidenPermitted[owner]
	return ok
}

// Register adds a hook. If a hook with the same (Owner, Name) is
// already registered, the prior entry is evicted in-place and the
// new one takes its slot — preserves chain order so a re-registration
// on app restart doesn't reshuffle the hook graph.
//
// Returns the assigned ID, or ErrInvalidRegistration if required
// fields are missing or malformed.
func (r *Registry) Register(h *Hook) (string, error) {
	if err := validate(h); err != nil {
		return "", err
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	key := ownerName{Owner: h.Owner, Name: h.Name}
	now := time.Now()
	if existing, ok := r.byKey[key]; ok {
		// Replace in-place: keep the existing position in `order` so
		// chain order is stable across re-registrations. New ID, same
		// slot.
		newID := newHookID()
		h.ID = newID
		h.RegisteredAt = now
		h.Timeout = resolveTimeout(h.TimeoutMs)
		if h.FailMode == "" {
			h.FailMode = FailOpen
		}
		// Replace position in order: find existing.ID, swap to newID.
		for i, id := range r.order {
			if id == existing.ID {
				r.order[i] = newID
				break
			}
		}
		delete(r.byID, existing.ID)
		r.byID[newID] = h
		r.byKey[key] = h
		return newID, nil
	}

	id := newHookID()
	h.ID = id
	h.RegisteredAt = now
	h.Timeout = resolveTimeout(h.TimeoutMs)
	if h.FailMode == "" {
		h.FailMode = FailOpen
	}
	r.byID[id] = h
	r.byKey[key] = h
	r.order = append(r.order, id)
	return id, nil
}

// Delete removes the hook with the given ID. Returns ErrNotFound if
// no such hook is registered.
func (r *Registry) Delete(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	h, ok := r.byID[id]
	if !ok {
		return ErrNotFound
	}
	delete(r.byID, id)
	delete(r.byKey, ownerName{Owner: h.Owner, Name: h.Name})
	for i, oid := range r.order {
		if oid == id {
			r.order = append(r.order[:i], r.order[i+1:]...)
			break
		}
	}
	return nil
}

// List returns a snapshot of all currently-registered hooks in
// registration order. Used by the GET /v1/hooks debug endpoint.
func (r *Registry) List() []*Hook {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*Hook, 0, len(r.order))
	for _, id := range r.order {
		if h, ok := r.byID[id]; ok {
			out = append(out, h)
		}
	}
	return out
}

// Match returns the hooks that fire for the given (agent, tool, phase),
// in chain order:
//   - Pre-hooks: registration order (earliest first; first non-nil
//     deny short-circuits the chain).
//   - Post-hooks: REVERSE registration order (LIFO middleware
//     pattern; outer hooks see the inner hooks' modifications).
//
// Returns nil when no hooks match — callers can short-circuit
// without allocating.
func (r *Registry) Match(agent, tool string, phase Phase) []*Hook {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var out []*Hook
	for _, id := range r.order {
		h, ok := r.byID[id]
		if !ok {
			continue
		}
		if h.Matches(agent, tool, phase) {
			out = append(out, h)
		}
	}
	if phase == PhasePost {
		// Reverse for LIFO middleware ordering.
		for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
			out[i], out[j] = out[j], out[i]
		}
	}
	return out
}

// validate enforces the required-field contract at registration
// time so the dispatcher hot path can trust well-formed Hook values.
func validate(h *Hook) error {
	if h == nil {
		return ErrInvalidRegistration
	}
	if strings.TrimSpace(h.Owner) == "" {
		return wrap(ErrInvalidRegistration, "owner required")
	}
	if strings.TrimSpace(h.Name) == "" {
		return wrap(ErrInvalidRegistration, "name required")
	}
	if h.Phase != PhasePre && h.Phase != PhasePost {
		return wrap(ErrInvalidRegistration, "phase must be \"pre\" or \"post\"")
	}
	if strings.TrimSpace(h.CallbackURL) == "" {
		return wrap(ErrInvalidRegistration, "callback_url required")
	}
	// Reject obvious URL malformation; we don't dial it here, that
	// happens lazily on first invocation.
	if !strings.HasPrefix(h.CallbackURL, "http://") && !strings.HasPrefix(h.CallbackURL, "https://") {
		return wrap(ErrInvalidRegistration, "callback_url must be http:// or https://")
	}
	if h.FailMode != "" && h.FailMode != FailOpen && h.FailMode != FailClosed {
		return wrap(ErrInvalidRegistration, "fail_mode must be \"open\" or \"closed\"")
	}
	if h.TimeoutMs < 0 {
		return wrap(ErrInvalidRegistration, "timeout_ms must be ≥ 0")
	}
	return nil
}

// resolveTimeout converts the wire-friendly TimeoutMs into a
// time.Duration with a sensible default and a hard ceiling.
func resolveTimeout(ms int) time.Duration {
	if ms <= 0 {
		return 5 * time.Second
	}
	d := time.Duration(ms) * time.Millisecond
	const maxTimeout = 60 * time.Second
	if d > maxTimeout {
		d = maxTimeout
	}
	return d
}

// globsMatch returns true if `s` matches at least one entry in
// `globs`. An empty/nil `globs` is treated as ["*"] (match all).
// Each glob entry is either an exact match or a trailing-* prefix
// glob. No middle wildcards or regex.
func globsMatch(globs []string, s string) bool {
	if len(globs) == 0 {
		return true
	}
	for _, g := range globs {
		if g == "*" {
			return true
		}
		if strings.HasSuffix(g, "*") {
			prefix := g[:len(g)-1]
			if strings.HasPrefix(s, prefix) {
				return true
			}
			continue
		}
		if g == s {
			return true
		}
	}
	return false
}

// newHookID returns a 16-hex-char random ID prefixed "hook_". Caller
// owns no entropy assumptions beyond crypto/rand's strength — this
// is a public surface ID used in the DELETE path, so collision risk
// matters.
func newHookID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failure is a runtime catastrophe; the registry
		// is unusable without a fresh ID. Return a fallback derived
		// from time so we don't panic — caller will likely overwrite
		// on re-register anyway.
		return "hook_" + hex.EncodeToString([]byte(time.Now().UTC().Format("150405.000000")))
	}
	return "hook_" + hex.EncodeToString(b[:])
}

// wrap stitches a contextual message onto a sentinel error without
// breaking errors.Is.
func wrap(sentinel error, msg string) error {
	return &wrappedError{sentinel: sentinel, msg: msg}
}

type wrappedError struct {
	sentinel error
	msg      string
}

func (e *wrappedError) Error() string { return e.sentinel.Error() + ": " + e.msg }
func (e *wrappedError) Unwrap() error { return e.sentinel }
