package hooks

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// ErrInvalidRegistration is returned when a hook added to a Set carries
// missing or malformed fields.
var ErrInvalidRegistration = errors.New("invalid hook registration")

// Set is a run's hooks: the resolved chain a Run carries and fires, in the
// order its definitions list them. It is built once, when the run starts, from
// the run's AgentDef (and, later, what its TeamDef and request add); nothing
// registers into a Set from outside the run.
//
// There is no tenant filter and no tenant-first ordering. Those protected the
// operator's hooks inside one process-wide registry every tenant wrote to; a
// Set belongs to one run and is exactly what that run's definitions named.
type Set struct {
	mu    sync.RWMutex
	hooks []*Hook
	err   error
}

// NewSet returns an empty Set.
func NewSet() *Set { return &Set{} }

// FailedSet is the set of a run whose hooks could not be resolved. It fires
// nothing, and Err says why; the loop refuses to start such a run.
func FailedSet(err error) *Set { return &Set{err: err} }

// Err is why the set could not be resolved, or nil.
func (s *Set) Err() error {
	if s == nil {
		return nil
	}
	return s.err
}

// Register validates h and appends it to the chain. A hook listed twice runs
// twice: a run may add hooks but never remove or merge one. Returns its id.
func (s *Set) Register(h *Hook) (string, error) {
	if err := validate(h); err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if h.ID == "" {
		h.ID = newHookID()
	}
	if h.RegisteredAt.IsZero() {
		h.RegisteredAt = time.Now()
	}
	h.Timeout = timeoutFor(h)
	if h.FailMode == "" {
		h.FailMode = FailOpen
	}
	s.hooks = append(s.hooks, h)
	return h.ID, nil
}

// List returns the chain in order.
func (s *Set) List() []*Hook {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]*Hook(nil), s.hooks...)
}

// Len is the number of hooks in the chain.
func (s *Set) Len() int {
	if s == nil {
		return 0
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.hooks)
}

// Match returns the hooks that fire for (agent, tool, phase), in chain order:
// listed order for pre and the run phases (the first deny stops the chain),
// reversed for post and post_failure (middleware: the outer hook sees what the
// inner ones did). Returns nil when nothing matches.
func (s *Set) Match(agent, tool string, phase Phase) []*Hook {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []*Hook
	for _, h := range s.hooks {
		if h.Matches(agent, tool, phase) {
			out = append(out, h)
		}
	}
	if phase == PhasePost || phase == PhasePostFailure {
		reverse(out)
	}
	return out
}

type setKey struct{}

// WithSet attaches a run's hooks to ctx; the dispatcher fires what it finds
// there. A nil or empty Set attaches nothing.
func WithSet(ctx context.Context, s *Set) context.Context {
	return context.WithValue(ctx, setKey{}, s)
}

// SetFrom returns the run's hooks, or nil.
func SetFrom(ctx context.Context) *Set {
	s, _ := ctx.Value(setKey{}).(*Set)
	return s
}

// Permits is the operator's host-widen permit list: the hooks whose pre-hook
// allow_hosts may be honoured. An entry is `[tenant:]name` — the tenant that
// owns the definition the hook came from ("" for the operator's own config)
// and the hook's name (a HookDef's, or an inline webhook's). It is read once at
// boot and never changes, so the trust boundary is what the operator declared,
// not what a runtime API wrote.
type Permits struct {
	set map[tenantName]struct{}
}

type tenantName struct{ Tenant, Name string }

// NewPermits parses the permit entries; an empty entry or one with no name is
// dropped.
func NewPermits(entries []string) Permits {
	p := Permits{set: make(map[tenantName]struct{}, len(entries))}
	for _, entry := range entries {
		entry = strings.TrimSpace(entry)
		tenant, name := "", entry
		if i := strings.IndexByte(entry, ':'); i >= 0 {
			tenant, name = strings.TrimSpace(entry[:i]), strings.TrimSpace(entry[i+1:])
		}
		if name == "" {
			continue
		}
		p.set[tenantName{tenant, name}] = struct{}{}
	}
	return p
}

// Has reports whether the operator permitted (tenant, name) to widen hosts.
func (p Permits) Has(tenant, name string) bool {
	_, ok := p.set[tenantName{tenant, name}]
	return ok
}

func reverse(hs []*Hook) {
	for i, j := 0, len(hs)-1; i < j; i, j = i+1, j-1 {
		hs[i], hs[j] = hs[j], hs[i]
	}
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
	switch h.Phase {
	case PhasePre, PhasePost, PhasePostFailure:
	case PhaseAgentStart, PhaseAgentStop, PhaseSubagentStart, PhaseSubagentStop,
		PhasePreCompact, PhasePostCompact, PhaseRunEnd:
		if len(h.Tools) > 0 {
			return wrap(ErrInvalidRegistration, "tools selects tool calls; an "+string(h.Phase)+" hook is selected by agents only")
		}
	default:
		return wrap(ErrInvalidRegistration, "phase must be one of pre, post, post_failure, agent_start, agent_stop, subagent_start, subagent_stop, pre_compact, post_compact, run_end")
	}
	hasURL, hasCode := strings.TrimSpace(h.CallbackURL) != "", strings.TrimSpace(h.Code) != ""
	switch {
	case hasURL && hasCode:
		return wrap(ErrInvalidRegistration, "set callback_url or code, not both")
	case hasCode:
		if len(h.Code) > MaxCodeBytes {
			return wrap(ErrInvalidRegistration, fmt.Sprintf("code is %d bytes; the limit is %d", len(h.Code), MaxCodeBytes))
		}
	case !hasURL:
		return wrap(ErrInvalidRegistration, "callback_url or code required")
	// Reject obvious URL malformation; we don't dial it here, that
	// happens lazily on first invocation.
	case !strings.HasPrefix(h.CallbackURL, "http://") && !strings.HasPrefix(h.CallbackURL, "https://"):
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

// MaxCodeBytes bounds a code hook's body. A hook runs on every matching call,
// so its source is kept small; bigger logic belongs in a webhook.
const MaxCodeBytes = 256 << 10

// timeoutFor is a hook's resolved timeout. For a webhook it bounds the whole
// call. For a code body it bounds each run of the JavaScript: a hook is on the
// hot path of every matching tool call, so the default is tight, and the time
// a person takes to answer the hook's Interruption.ask is not counted.
func timeoutFor(h *Hook) time.Duration {
	if !h.IsCode() {
		return resolveTimeout(h.TimeoutMs)
	}
	if h.TimeoutMs <= 0 {
		return 50 * time.Millisecond
	}
	d := time.Duration(h.TimeoutMs) * time.Millisecond
	if d > time.Second {
		d = time.Second
	}
	return d
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
