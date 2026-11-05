package redact

import (
	"container/list"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"
)

// The runtime-registered set (Tier A') is fed by values a run's tools resolve
// or receive: $cred: values, minted tokens, peer and provider keys, and the
// credential parts of a URL a tool fetched (which a model chooses). It is
// bounded (RFC FZ D5), and the bound is built so that evicting a value can
// never unmask a secret a run can still emit:
//
//   - A value is PINNED while any run that registered it is live (the server
//     answers liveness from its cancel registry, by root run id and agent id,
//     so a parent that repeats what a child resolved keeps it pinned). Pinned
//     values are never evicted. If pinned values alone exceed a bound, the
//     set grows past it with a rate-limited warning: live runs bound them.
//     A run's content is redacted when it is written, while the run is live;
//     what is read back after it ends (a stream re-attach, a replay, a
//     resume, a snapshot) is the stored, already-masked text.
//   - Credentials (resolved secrets) share one global LRU of DefaultDynamicCap
//     values; past it the least recently registered UNPINNED value goes,
//     whoever registered it. No run can emit it any more, and a later run that
//     resolves it registers and pins it again, so no per-tenant fairness is
//     needed.
//   - Observed values (whose content a model may influence: URL parts) have a
//     smaller bucket per tenant and evict only within it, oldest unpinned
//     first; they never displace a credential.
//   - Operator-plane values have their own bucket, evicted only by operator
//     registrations; the static env values given to New are outside the set.
//   - The replacer is one longest-first replacer over the whole set, rebuilt
//     on the first String after the set changed. Two replacers applied in
//     turn can split a value that overlaps another (a suffix of one is a
//     prefix of the other), so masking is never deferred to a second pass.
const (
	// DefaultDynamicCap bounds the unpinned credentials.
	DefaultDynamicCap = 4096
	// The operator plane holds at most a quarter of the cap; a tenant's
	// observed values a sixteenth.
	operatorShareDivisor = 4
	observedShareDivisor = 16
	// pruneHoldersAt is how many run ids a value collects before the dead
	// ones are dropped, so a long-lived secret does not grow its holder set
	// by one per run that ever used it.
	pruneHoldersAt = 16
)

// Holder is who registered a value: the tenant it belongs to and the runs
// that keep it pinned while live.
type Holder struct {
	Tenant string
	// User is carried for the caller's record; buckets are per tenant.
	User string
	// Runs are the ids whose liveness pins the value (a run id and an agent
	// id are both accepted; empty ones are ignored).
	Runs []string
}

type bucketKind byte

const (
	bucketOperator bucketKind = iota
	bucketCredential
	bucketObserved
)

// dynEntry is one registered value and where it is held. It leaves the set
// when no list holds it.
type dynEntry struct {
	value    string
	cred     *list.Element            // in dynamicSet.creds
	observed map[string]*list.Element // tenant → in dynamicSet.observed[tenant]
	op       *list.Element            // in dynamicSet.operator
	runs     map[string]struct{}      // pinning holders
}

// dynamicSet is the bounded set and its compiled form.
type dynamicSet struct {
	mu       sync.RWMutex
	cap      int // 0 means DefaultDynamicCap
	entries  map[string]*dynEntry
	creds    *list.List            // most recent first
	observed map[string]*list.List // per tenant, most recent first
	operator *list.List            // most recent first
	// liveness returns a predicate over run ids, answered from one snapshot;
	// nil means no run is ever live (nothing is pinned).
	liveness func() func(run string) bool

	repl         *strings.Replacer // nil when the set is empty
	dirty        bool              // the set changed since repl was built
	lastWarn     time.Time
	rebuildCount int // for tests
}

func (d *dynamicSet) capacity() int {
	if d.cap > 0 {
		return d.cap
	}
	return DefaultDynamicCap
}

// register adds value as kind for h. The caller has checked its length.
func (d *dynamicSet) register(kind bucketKind, h Holder, value string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.entries == nil {
		d.entries = map[string]*dynEntry{}
		d.creds, d.operator = list.New(), list.New()
		d.observed = map[string]*list.List{}
	}
	e, ok := d.entries[value]
	if !ok {
		e = &dynEntry{value: value}
		d.entries[value] = e
		d.dirty = true
	}
	var live func(string) bool
	liveOf := func() func(string) bool {
		if live == nil {
			live = func(string) bool { return false }
			if d.liveness != nil {
				live = d.liveness()
			}
		}
		return live
	}
	for _, run := range h.Runs {
		if run == "" {
			continue
		}
		if e.runs == nil {
			e.runs = map[string]struct{}{}
		}
		e.runs[run] = struct{}{}
	}
	if len(e.runs) > pruneHoldersAt {
		is := liveOf()
		for run := range e.runs {
			if !is(run) {
				delete(e.runs, run)
			}
		}
	}

	switch kind {
	case bucketOperator:
		e.op = pushOrTouch(d.operator, e.op, e)
		for d.operator.Len() > max(1, d.capacity()/operatorShareDivisor) {
			// Operator values are pinned by no run: the oldest goes (an
			// expired credential, in practice).
			if !d.evictOldest(d.operator, e, nil, func(x *dynEntry) { d.operator.Remove(x.op); x.op = nil }) {
				break
			}
		}
	case bucketCredential:
		e.cred = pushOrTouch(d.creds, e.cred, e)
		for d.creds.Len() > d.capacity() {
			if !d.evictOldest(d.creds, e, liveOf(), func(x *dynEntry) { d.creds.Remove(x.cred); x.cred = nil }) {
				d.warnOver("live runs hold more credentials than the cap", h.Tenant, d.creds.Len())
				break
			}
		}
	case bucketObserved:
		b := d.observed[h.Tenant]
		if b == nil {
			b = list.New()
			d.observed[h.Tenant] = b
		}
		if e.observed == nil {
			e.observed = map[string]*list.Element{}
		}
		e.observed[h.Tenant] = pushOrTouch(b, e.observed[h.Tenant], e)
		tenant := h.Tenant
		for b.Len() > max(1, d.capacity()/observedShareDivisor) {
			if !d.evictOldest(b, e, liveOf(), func(x *dynEntry) { b.Remove(x.observed[tenant]); delete(x.observed, tenant) }) {
				d.warnOver("live runs hold more observed values than a tenant's share", tenant, b.Len())
				break
			}
		}
	}
}

// pushOrTouch moves el to the front of l, or pushes e when el is nil.
func pushOrTouch(l *list.List, el *list.Element, e *dynEntry) *list.Element {
	if el != nil {
		l.MoveToFront(el)
		return el
	}
	return l.PushFront(e)
}

// evictOldest takes the least recently registered value of l that is
// neither keep (the value being registered, which must be masked when the
// registration returns) nor pinned, with release (which takes it out of l),
// and drops it from the set when no list holds it any more. live nil means
// nothing in l is pinned. It reports false when l has no such value.
func (d *dynamicSet) evictOldest(l *list.List, keep *dynEntry, live func(string) bool, release func(*dynEntry)) bool {
	for el := l.Back(); el != nil; el = el.Prev() {
		e := el.Value.(*dynEntry)
		if e == keep || (live != nil && pinned(e, live)) {
			continue
		}
		release(e)
		if e.cred == nil && e.op == nil && len(e.observed) == 0 {
			delete(d.entries, e.value)
			d.dirty = true
		}
		return true
	}
	return false
}

func pinned(e *dynEntry, live func(string) bool) bool {
	for run := range e.runs {
		if live(run) {
			return true
		}
	}
	return false
}

// warnOver logs, at most once a minute, that pinned values exceed a bound.
// It names a tenant id, never a value.
func (d *dynamicSet) warnOver(msg, tenant string, n int) {
	now := time.Now()
	if now.Sub(d.lastWarn) < time.Minute {
		return
	}
	d.lastWarn = now
	slog.Warn("redact: keeping pinned secrets past the bound", "reason", msg, "tenant", tenant, "held", n, "cap", d.capacity())
}

// replacer returns the compiled set, rebuilding it first if the set changed.
func (d *dynamicSet) replacer() *strings.Replacer {
	d.mu.RLock()
	repl, dirty := d.repl, d.dirty
	d.mu.RUnlock()
	if !dirty {
		return repl
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.dirty {
		vals := make([]string, 0, len(d.entries))
		for v := range d.entries {
			vals = append(vals, v)
		}
		d.repl = nil
		if len(vals) > 0 {
			d.repl = longestFirstReplacer(vals)
		}
		d.dirty = false
		d.rebuildCount++
	}
	return d.repl
}

// held reports whether the set holds any value.
func (d *dynamicSet) held() bool {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return len(d.entries) > 0 || d.repl != nil
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
