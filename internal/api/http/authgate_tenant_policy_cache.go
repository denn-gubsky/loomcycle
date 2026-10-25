package http

import (
	"sync"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
)

// tenantPolicyCacheTTL bounds how long a replica answers from its memo of a
// tenant's gate policy. A policy change flushes every replica at once (the
// token-invalidation backplane); the TTL is only the bound for a replica that
// missed that message.
const tenantPolicyCacheTTL = 10 * time.Second

// tenantPolicyCache memoises effectiveTenantGates per tenant for the check
// every request makes. An entry is valid only for the config it was computed
// against, so a reload that changes auth.tenant_policies is seen at once. A
// store error is never cached: the request is refused and the next one asks
// again.
type tenantPolicyCache struct {
	mu      sync.Mutex
	cfg     *config.Config
	entries map[string]tenantPolicyEntry
}

type tenantPolicyEntry struct {
	gates      []string
	restricted bool
	at         time.Time
}

func (c *tenantPolicyCache) get(cfg *config.Config, tenant string, now time.Time) (tenantPolicyEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cfg != cfg {
		return tenantPolicyEntry{}, false
	}
	e, ok := c.entries[tenant]
	if !ok || now.Sub(e.at) >= tenantPolicyCacheTTL {
		return tenantPolicyEntry{}, false
	}
	return e, true
}

func (c *tenantPolicyCache) put(cfg *config.Config, tenant string, e tenantPolicyEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cfg != cfg || c.entries == nil {
		c.cfg, c.entries = cfg, map[string]tenantPolicyEntry{}
	}
	c.entries[tenant] = e
}

func (c *tenantPolicyCache) flush() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = nil
}
