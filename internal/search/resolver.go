package search

import (
	"sync"
	"time"
)

// DefaultCooldown is how long a provider is skipped after a failed call.
// Short — availability is advisory + per-replica, and search failures are
// usually transient (rate-limit, upstream blip); a long cooldown would strand a
// provider on one blip.
const DefaultCooldown = 60 * time.Second

// Resolver is the search analog of resolve.Resolver, minus the tier/model
// matrix: a single flat priority order + a per-provider last-outcome cooldown
// (RFC BB — no active probing of paid providers). Safe for concurrent use.
type Resolver struct {
	mu       sync.RWMutex
	priority []string
	cooldown time.Duration
	stalled  map[string]time.Time // provider id → cooldown expiry
}

// NewResolver builds a resolver over the global default priority order.
func NewResolver(priority []string) *Resolver {
	return &Resolver{
		priority: append([]string(nil), priority...),
		cooldown: DefaultCooldown,
		stalled:  map[string]time.Time{},
	}
}

// Cascade returns the effective ordered provider IDs to try: the per-agent list
// when non-empty (a full override, mirroring AgentDef.Providers), else the
// global default priority. The caller skips IDs that aren't registered.
func (r *Resolver) Cascade(agentList []string) []string {
	if len(agentList) > 0 {
		return append([]string(nil), agentList...)
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]string(nil), r.priority...)
}

// Available reports whether a provider is outside its failure cooldown.
func (r *Resolver) Available(id string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	until, ok := r.stalled[id]
	return !ok || !time.Now().Before(until)
}

// MarkOutcome records the result of a Search call: nil clears any cooldown
// (success), a non-nil error opens a cooldown window (last-outcome
// availability). NOT called for ErrNotKeyable or empty-but-ok results — those
// aren't provider failures.
func (r *Resolver) MarkOutcome(id string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err == nil {
		delete(r.stalled, id)
		return
	}
	r.stalled[id] = time.Now().Add(r.cooldown)
}
