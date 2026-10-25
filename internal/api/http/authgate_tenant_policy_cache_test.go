package http

import (
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
)

// A load that started before a flush must not store its (now stale) result
// after it: the flush is how a policy change reaches this replica, and a stale
// put would undo it for the memo's lifetime.
func TestTenantPolicyCache_LoadStartedBeforeAFlushIsNotStored(t *testing.T) {
	var c tenantPolicyCache
	cfg := &config.Config{}
	gen := c.begin() // load starts, reads the old policy
	c.flush()        // the policy changes meanwhile
	c.put(cfg, "acme", tenantPolicyEntry{gates: []string{"paste"}, restricted: true, at: time.Now()}, gen)
	if _, ok := c.get(cfg, "acme", time.Now()); ok {
		t.Fatal("a load that raced a flush was cached")
	}
	// A load that did not race is cached.
	gen = c.begin()
	c.put(cfg, "acme", tenantPolicyEntry{gates: []string{"local"}, restricted: true, at: time.Now()}, gen)
	if e, ok := c.get(cfg, "acme", time.Now()); !ok || e.gates[0] != "local" {
		t.Fatalf("an unraced load was not cached: %+v %v", e, ok)
	}
}
