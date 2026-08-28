package config

import "sync/atomic"

// Holder is a concurrency-safe cell holding the current *Config (RFC CK runtime
// reload). Readers Load() the current snapshot lock-free; a reload Store()s a new
// one atomically. A run captures Load() once at start so it sees a consistent
// config even if a reload lands mid-run — the swap affects runs admitted AFTER
// it, never one already in flight.
type Holder struct {
	p atomic.Pointer[Config]
}

// NewHolder returns a Holder seeded with c (the boot config).
func NewHolder(c *Config) *Holder {
	h := &Holder{}
	h.p.Store(c)
	return h
}

// Load returns the current config snapshot (never blocks). nil until a Store.
func (h *Holder) Load() *Config { return h.p.Load() }

// Store atomically swaps in a new current config. The previous one keeps serving
// any reader that already Load()ed it.
func (h *Holder) Store(c *Config) { h.p.Store(c) }
