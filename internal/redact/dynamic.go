package redact

import (
	"container/list"
	"sort"
	"strings"
	"sync"
	"time"
)

// The runtime-registered set (Tier A') is fed by values a run's tools resolve
// or receive: $cred: values, minted tokens, the credential parts of a signed
// URL a model fetched. Some of those a model chooses, so the set is bounded
// (RFC FZ D5) and the cost of keeping it compiled is too.
const (
	// DefaultDynamicCap is how many registered values the set holds at most.
	DefaultDynamicCap = 4096
	// dynamicShareDivisor sets one owner's share of the cap: a quarter. A
	// tenant registering past its share evicts its own least recently used
	// value, never another tenant's; the operator plane has a share of the
	// same size that tenant registrations never evict from.
	dynamicShareDivisor = 4
	// dynamicRebuildInterval is the least time between two recompilations of
	// the set's replacer in the common case. A value registered in between is
	// masked at once through the pending overlay (see dynamicSet.register).
	dynamicRebuildInterval = time.Second
	// maxDynamicPending bounds that overlay: past it the set recompiles at
	// once, so a flood costs one rebuild per this many registrations rather
	// than one each.
	maxDynamicPending = 64
)

// dynEntry is one registered value.
type dynEntry struct {
	value    string
	owner    string // tenant id; unused for the operator plane
	operator bool
	// global is the entry's element in dynamicSet.tenants (tenant entries
	// only); own is its element in its owner's list (dynamicSet.owners, or
	// dynamicSet.operator).
	global, own *list.Element
}

// dynamicSet is the bounded, LRU-ordered set of registered values and its
// compiled form. Least recently used is by registration: a credential a run
// resolves again is registered again, which keeps it fresh.
//
// The compiled form is two replacers. settled holds every value compiled at
// the last rebuild (an evicted value stays in it until the next one: masking
// a value no longer held only over-masks). pending holds the values
// registered since, so a value is masked from the moment Register returns,
// whether or not the rebuild was deferred. The two are applied one after the
// other, which equals one longest-first replacer only when no pending value
// contains, or is contained in, a settled one, so such a registration
// rebuilds at once instead of deferring.
type dynamicSet struct {
	mu       sync.RWMutex
	cap      int              // 0 means DefaultDynamicCap
	now      func() time.Time // nil means time.Now
	entries  map[string]*dynEntry
	tenants  *list.List            // every tenant entry, most recent first
	owners   map[string]*list.List // per tenant, most recent first
	operator *list.List            // the operator plane's entries, most recent first

	settled      []string // values compiled into settledRepl
	settledRepl  *strings.Replacer
	pending      []string
	pendingRepl  *strings.Replacer
	lastRebuild  time.Time
	rebuildCount int // for tests
}

func (d *dynamicSet) capacity() int {
	if d.cap > 0 {
		return d.cap
	}
	return DefaultDynamicCap
}

func (d *dynamicSet) share() int {
	return max(1, d.capacity()/dynamicShareDivisor)
}

func (d *dynamicSet) clock() time.Time {
	if d.now != nil {
		return d.now()
	}
	return time.Now()
}

// register adds value for owner (operator, or a tenant id). The caller has
// checked the value's length.
func (d *dynamicSet) register(value, owner string, operator bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.entries == nil {
		d.entries = map[string]*dynEntry{}
		d.tenants = list.New()
		d.owners = map[string]*list.List{}
		d.operator = list.New()
	}
	if e, ok := d.entries[value]; ok {
		switch {
		case e.operator:
			d.operator.MoveToFront(e.own)
		case operator:
			// The operator plane obtained the same value: it is now an
			// operator secret, out of every tenant's reach.
			d.unlink(e)
			d.linkOperator(e)
		default:
			d.tenants.MoveToFront(e.global)
			d.owners[e.owner].MoveToFront(e.own)
		}
		return // already masked
	}
	e := &dynEntry{value: value, owner: owner}
	d.entries[value] = e
	if operator {
		d.linkOperator(e)
		if d.operator.Len() > d.share() {
			d.evict(d.operator.Back().Value.(*dynEntry))
		}
	} else {
		e.global = d.tenants.PushFront(e)
		own := d.owners[owner]
		if own == nil {
			own = list.New()
			d.owners[owner] = own
		}
		e.own = own.PushFront(e)
		if own.Len() > d.share() {
			d.evict(own.Back().Value.(*dynEntry))
		}
	}
	// The operator plane holds at most one share, so the rest of the cap is
	// always tenant entries and the least recently used of them goes.
	for len(d.entries) > d.capacity() && d.tenants.Len() > 0 {
		d.evict(d.tenants.Back().Value.(*dynEntry))
	}
	d.compile(value)
}

func (d *dynamicSet) linkOperator(e *dynEntry) {
	e.operator, e.owner, e.global = true, "", nil
	e.own = d.operator.PushFront(e)
}

// unlink takes e out of its lists, leaving it in entries.
func (d *dynamicSet) unlink(e *dynEntry) {
	if e.operator {
		d.operator.Remove(e.own)
		return
	}
	d.tenants.Remove(e.global)
	own := d.owners[e.owner]
	own.Remove(e.own)
	if own.Len() == 0 {
		delete(d.owners, e.owner)
	}
}

func (d *dynamicSet) evict(e *dynEntry) {
	d.unlink(e)
	delete(d.entries, e.value)
}

// compile makes the newly registered value masked: through the pending
// overlay when the last rebuild is recent, the overlay is small and value
// nests with no settled value; otherwise by rebuilding now.
func (d *dynamicSet) compile(value string) {
	now := d.clock()
	if d.settledRepl != nil && now.Sub(d.lastRebuild) < dynamicRebuildInterval &&
		len(d.pending) < maxDynamicPending && !nestsWithAny(value, d.settled) {
		d.pending = append(d.pending, value)
		d.pendingRepl = longestFirstReplacer(d.pending)
		return
	}
	d.settled = d.settled[:0]
	for v := range d.entries {
		d.settled = append(d.settled, v)
	}
	d.settledRepl = longestFirstReplacer(d.settled)
	d.pending, d.pendingRepl = nil, nil
	d.lastRebuild = now
	d.rebuildCount++
}

// nestsWithAny reports whether v contains, or is contained in, one of vals.
func nestsWithAny(v string, vals []string) bool {
	for _, s := range vals {
		if strings.Contains(s, v) || strings.Contains(v, s) {
			return true
		}
	}
	return false
}

// longestFirstReplacer masks vals, the longest first, so a value that is a
// substring of another is masked as part of the longer match (mirrors New's
// Tier A). It sorts vals in place.
func longestFirstReplacer(vals []string) *strings.Replacer {
	sort.Slice(vals, func(i, j int) bool { return len(vals[i]) > len(vals[j]) })
	pairs := make([]string, 0, len(vals)*2)
	for _, v := range vals {
		pairs = append(pairs, v, dynamicMarker)
	}
	return strings.NewReplacer(pairs...)
}

// replacers returns the compiled forms to apply, pending first; either may be
// nil.
func (d *dynamicSet) replacers() (pending, settled *strings.Replacer) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.pendingRepl, d.settledRepl
}
