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

// Availability is one provider's live status for the routing view (RFC BB).
type Availability struct {
	Reachable    bool      // outside any failure cooldown right now
	LastError    string    // last failure text; "" when healthy
	StalledUntil time.Time // cooldown expiry; zero = never failed
}

// Resolver is the search analog of resolve.Resolver, minus the tier/model
// matrix: a single flat priority order + a per-provider last-outcome cooldown
// (RFC BB — no active probing of paid providers). Safe for concurrent use.
type Resolver struct {
	mu       sync.RWMutex
	priority []string
	cooldown time.Duration
	state    map[string]*provState // provider id → last-outcome state
}

type provState struct {
	stalledUntil time.Time
	lastErr      string
}

// NewResolver builds a resolver over the global default priority order.
func NewResolver(priority []string) *Resolver {
	return &Resolver{
		priority: append([]string(nil), priority...),
		cooldown: DefaultCooldown,
		state:    map[string]*provState{},
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
	st, ok := r.state[id]
	return !ok || !time.Now().Before(st.stalledUntil)
}

// MarkOutcome records the result of a Search call: nil clears any cooldown
// (success), a non-nil error opens a cooldown window (last-outcome
// availability). NOT called for an un-keyable skip or an empty-but-ok result —
// those aren't provider failures.
func (r *Resolver) MarkOutcome(id string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err == nil {
		delete(r.state, id)
		return
	}
	r.state[id] = &provState{stalledUntil: time.Now().Add(r.cooldown), lastErr: err.Error()}
}

// Snapshot returns the live availability of every provider in the priority
// order — the routing view's data source.
func (r *Resolver) Snapshot() map[string]Availability {
	r.mu.RLock()
	defer r.mu.RUnlock()
	now := time.Now()
	out := make(map[string]Availability, len(r.priority))
	for _, id := range r.priority {
		st := r.state[id]
		if st == nil {
			out[id] = Availability{Reachable: true}
			continue
		}
		out[id] = Availability{
			Reachable:    !now.Before(st.stalledUntil),
			LastError:    st.lastErr,
			StalledUntil: st.stalledUntil,
		}
	}
	return out
}
