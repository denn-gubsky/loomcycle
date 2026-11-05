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
// bounded (RFC FZ D5), but a value that leaves it is no longer masked, so the
// bound is built to never cost a live secret:
//
//   - Every value belongs to buckets: the operator plane's, a tenant's
//     credentials, or a tenant and user's observed values (URL parts). A
//     registration that pushes its bucket past its share evicts the least
//     recently registered value of that same bucket, so one tenant (or one
//     user's model) cannot push out another's secrets by flooding.
//   - When the whole set passes the cap, the value is taken from the bucket
//     furthest over its fair share (the cap divided by the buckets in use,
//     never less than bucketFloor), and never from a bucket at or below its
//     fair share or its floor, nor from the operator plane: the overflow is
//     charged to whoever holds the most, not to whoever registered last.
//   - A value is pinned while a run that registered it is live (the server
//     answers liveness from its cancel registry). Eviction skips pinned
//     values; if pinned values alone exceed a bound, they are kept and a
//     warning is logged: they are bounded by the live runs.
//   - A value held by several buckets (two tenants resolved the same secret)
//     leaves the set only when every bucket holding it has let it go.
//   - The replacer is one longest-first replacer over the whole set, rebuilt
//     on the first String after the set changed. Two replacers applied in
//     turn can split a value that overlaps another (a suffix of one is a
//     prefix of the other), so there is no overlay: masking is never deferred.
const (
	// DefaultDynamicCap is how many registered values the set holds before
	// registrations start evicting (pinned values may exceed it).
	DefaultDynamicCap = 4096
	// A tenant's credentials, and the operator plane, each hold at most a
	// quarter of the cap; a tenant and user's observed values a sixteenth.
	credentialShareDivisor = 4
	observedShareDivisor   = 16
	// bucketFloor is how many values a bucket keeps whatever the pressure
	// from other buckets; past cap/bucketFloor buckets the set can exceed
	// the cap (with a warning) rather than take a bucket below it.
	bucketFloor = 8
	// pruneHoldersAt is how many run ids a value collects before the dead
	// ones are dropped, so a long-lived secret does not grow its holder set
	// by one per run that ever used it.
	pruneHoldersAt = 16
)

// Holder is who registered a value: the tenant (and, for an observed value,
// the user) it belongs to and the runs that keep it pinned while live.
type Holder struct {
	Tenant string
	User   string
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

// dynEntry is one registered value.
type dynEntry struct {
	value   string
	buckets map[string]*list.Element // bucket key → this entry's element in it
	runs    map[string]struct{}      // pinning holders
}

// dynamicSet is the bounded set and its compiled form.
type dynamicSet struct {
	mu      sync.RWMutex
	cap     int // 0 means DefaultDynamicCap
	entries map[string]*dynEntry
	buckets map[string]*list.List // most recent first
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

func (d *dynamicSet) share(kind bucketKind) int {
	div := credentialShareDivisor
	if kind == bucketObserved {
		div = observedShareDivisor
	}
	return max(1, d.capacity()/div)
}

func bucketKey(kind bucketKind, h Holder) string {
	switch kind {
	case bucketOperator:
		return "operator"
	case bucketCredential:
		return "credential\x00" + h.Tenant
	}
	return "observed\x00" + h.Tenant + "\x00" + h.User
}

// register adds value to the bucket kind/h names. The caller has checked the
// value's length.
func (d *dynamicSet) register(kind bucketKind, h Holder, value string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.entries == nil {
		d.entries = map[string]*dynEntry{}
		d.buckets = map[string]*list.List{}
	}
	key := bucketKey(kind, h)
	b := d.buckets[key]
	if b == nil {
		b = list.New()
		d.buckets[key] = b
	}
	e, ok := d.entries[value]
	if !ok {
		e = &dynEntry{value: value, buckets: map[string]*list.Element{}}
		d.entries[value] = e
		d.dirty = true
	}
	if el, in := e.buckets[key]; in {
		b.MoveToFront(el)
	} else {
		e.buckets[key] = b.PushFront(e)
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
	for b.Len() > d.share(kind) {
		if !d.evictFrom(key, e, liveOf()) {
			d.warnOver("a bucket's pinned values exceed its share", key, b.Len())
			break
		}
	}
	for len(d.entries) > d.capacity() {
		if !d.evictOverFairShare(e, liveOf()) {
			d.warnOver("the set is over its cap and no bucket is over its fair share with a value to give up", key, len(d.entries))
			return
		}
	}
}

// evictFrom lets go of the least recently registered unpinned value in the
// bucket key, other than keep (the value being registered, which must be
// masked when Register returns): the bucket drops it, and the set drops it
// when no other bucket holds it. It reports false when there is no such value.
func (d *dynamicSet) evictFrom(key string, keep *dynEntry, live func(string) bool) bool {
	b := d.buckets[key]
	if b == nil {
		return false
	}
	for el := b.Back(); el != nil; el = el.Prev() {
		e := el.Value.(*dynEntry)
		if e == keep || pinned(e, live) {
			continue
		}
		b.Remove(el)
		delete(e.buckets, key)
		if b.Len() == 0 {
			delete(d.buckets, key)
		}
		if len(e.buckets) == 0 {
			delete(d.entries, e.value)
			d.dirty = true
		}
		return true
	}
	return false
}

// evictOverFairShare takes one value from the tenant bucket furthest over its
// fair share that has an unpinned value other than keep to give. It reports
// false when no bucket over its fair share can give one.
func (d *dynamicSet) evictOverFairShare(keep *dynEntry, live func(string) bool) bool {
	fair := max(bucketFloor, d.capacity()/max(1, len(d.buckets)))
	tried := map[string]bool{}
	for {
		victim, over := "", 0
		for k, b := range d.buckets {
			if k == bucketKey(bucketOperator, Holder{}) || tried[k] {
				continue
			}
			if n := b.Len() - fair; n > over {
				victim, over = k, n
			}
		}
		if victim == "" {
			return false
		}
		if d.evictFrom(victim, keep, live) {
			return true
		}
		tried[victim] = true // everything it holds is pinned
	}
}

func pinned(e *dynEntry, live func(string) bool) bool {
	for run := range e.runs {
		if live(run) {
			return true
		}
	}
	return false
}

// warnOver logs, at most once a minute, that a bound is exceeded. Bucket keys
// carry a tenant id and a user id, never a value.
func (d *dynamicSet) warnOver(msg, key string, n int) {
	now := time.Now()
	if now.Sub(d.lastWarn) < time.Minute {
		return
	}
	d.lastWarn = now
	kind, rest, _ := strings.Cut(key, "\x00")
	tenant, _, _ := strings.Cut(rest, "\x00")
	slog.Warn("redact: keeping pinned secrets past the bound", "reason", msg, "bucket", kind, "tenant", tenant, "held", n, "cap", d.capacity())
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
