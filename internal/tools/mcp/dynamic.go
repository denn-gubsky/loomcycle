// dynamic.go — runtime registry of dynamically-registered MCP servers.
//
// The v0.9.x MCPServerDef substrate persists registrations to the
// store; this in-memory registry is the authoritative source for the
// pool's build() callback (which spawns clients on first agent call
// via the existing v0.8.1 lazy-fallback resolver path).
//
// Lifecycle:
//   - Boot (main.go): every active mcp_server_defs row is loaded into
//     the registry after migrations + backfill complete.
//   - Runtime: MCPServerDef.execCreate / execPromote / execRetire write
//     to the store AND mutate the registry.
//   - Pool integration: the pool's build callback consults
//     cfg.MCPServers (static yaml) first, then DynamicRegistry.Get(name)
//     for dynamic entries. Name collisions are refused at the tool
//     layer; the pool never sees a name that exists in both.
//
// Concurrency: safe for concurrent Get / Set / Remove. The pool's
// build callback runs under the pool mutex; the registry is consulted
// from there. Set/Remove from the substrate-tool goroutine; the
// registry's RWMutex serializes both.
package mcp

import "sync"

// DynamicMCPServerSpec is the in-memory shape the pool's build
// callback needs. Mirrors the operator-yaml mcp_servers.* shape.
//
// Transport is http / streamable-http by default; a `stdio` entry is
// only ever present when the operator opted in via
// LOOMCYCLE_MCP_ALLOW_DYNAMIC_STDIO (F31) — the substrate refuses to
// register a stdio server otherwise, since it runs an arbitrary local
// command. Command/Args/Env carry the stdio invocation and are empty for
// http transports.
//
// Headers are stored verbatim with their ${LOOMCYCLE_*} / ${run.user_bearer}
// substitution placeholders intact — the http client's substitution
// pass at request-build time resolves them (matches yaml-loaded
// headers' semantics; see internal/tools/mcp/http/client.go). Env values
// (stdio) are passed to the child literally — no ${} expansion.
type DynamicMCPServerSpec struct {
	Name      string
	Transport string // "http" | "streamable-http" | "stdio" (stdio gated by LOOMCYCLE_MCP_ALLOW_DYNAMIC_STDIO)
	URL       string
	Headers   map[string]string
	// stdio invocation (F31); empty for http transports.
	Command string
	Args    []string
	Env     map[string]string
	// TenantID is the RFC N tenant-isolation axis. "" = the shared/
	// operator/legacy tenant. Entries are keyed by (TenantID, Name) so
	// two tenants register the same name with distinct URLs without
	// colliding. Set from the authoritative principal at the write site;
	// never from the wire.
	TenantID string
}

// DynamicRegistry holds runtime-registered MCP server specs. One
// instance per loomcycle process; injected into the pool's build
// callback in main.go.
//
// Empty registry behaves the same as no registry at all — the pool's
// build callback falls back to the yaml-static map.
type DynamicRegistry struct {
	mu sync.RWMutex
	// RFC N: keyed by (tenant, name). Two tenants register the same name
	// with distinct connection metadata without colliding. The shared/
	// legacy tenant is the "" tenant key.
	entries map[regKey]DynamicMCPServerSpec
}

// regKey is the composite (tenant, name) registry key. RFC N.
type regKey struct {
	tenant string
	name   string
}

// NewDynamicRegistry returns an empty registry.
func NewDynamicRegistry() *DynamicRegistry {
	return &DynamicRegistry{entries: make(map[regKey]DynamicMCPServerSpec)}
}

// Set installs (or replaces) the entry for (spec.TenantID, spec.Name).
// The pool's build callback consults the registry on every cache miss;
// existing pool entries for the same (tenant, name) continue serving
// until they crash or are explicitly evicted via Pool.Evict (called by
// the tool's retire / promote-replaces-version paths).
func (r *DynamicRegistry) Set(spec DynamicMCPServerSpec) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries[regKey{spec.TenantID, spec.Name}] = spec
}

// Remove evicts the (tenantID, name) entry. Returns true if it existed.
//
// Removing an entry does NOT close existing pool clients on its own —
// the substrate-tool layer is responsible for invoking Pool.Evict BEFORE
// removing the registry entry. Otherwise an agent in-flight on the MCP
// server's tool call continues using the cached pool entry until the
// underlying transport crashes naturally.
func (r *DynamicRegistry) Remove(tenantID, name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	k := regKey{tenantID, name}
	if _, ok := r.entries[k]; !ok {
		return false
	}
	delete(r.entries, k)
	return true
}

// Get returns the spec for (tenantID, name) and a presence boolean.
// Called by the resolver on every lookup. RFC N: the caller is
// responsible for the static→tenant→shared precedence (see
// lookup.MCPServer); Get is an exact (tenant, name) read.
func (r *DynamicRegistry) Get(tenantID, name string) (DynamicMCPServerSpec, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	spec, ok := r.entries[regKey{tenantID, name}]
	return spec, ok
}

// Names returns every registered name across ALL tenants, sorted +
// deduped. Diagnostic-only (the LoomCycle MCP server's
// get_runtime_state) — it is NOT tenant-scoped and must never gate
// advertising. Use NamesForTenant for the per-run candidate set.
//
// Safe to call concurrently with Set/Remove — returns a snapshot.
func (r *DynamicRegistry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	seen := make(map[string]struct{}, len(r.entries))
	out := make([]string, 0, len(r.entries))
	for k := range r.entries {
		if _, dup := seen[k.name]; dup {
			continue
		}
		seen[k.name] = struct{}{}
		out = append(out, k.name)
	}
	sortStrings(out)
	return out
}

// NamesForTenant returns the names visible to a run in tenantID: that
// tenant's own names PLUS the shared ("" tenant) names, sorted +
// deduped. A tenant-owned name shadows a shared one of the same name
// (it appears once). RFC N §3: this is the per-run candidate set the
// advertising filter enumerates so a run in tenant A never sees tenant
// B's MCP servers.
func (r *DynamicRegistry) NamesForTenant(tenantID string) []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	seen := make(map[string]struct{}, len(r.entries))
	out := make([]string, 0, len(r.entries))
	add := func(name string) {
		if _, dup := seen[name]; dup {
			return
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	// Tenant-owned first so it claims the name; shared "" entries only
	// fill names the tenant doesn't already own (tenant shadows shared).
	for k := range r.entries {
		if k.tenant == tenantID {
			add(k.name)
		}
	}
	if tenantID != "" {
		for k := range r.entries {
			if k.tenant == "" {
				add(k.name)
			}
		}
	}
	sortStrings(out)
	return out
}

// sortStrings is an inline insertion sort — avoids pulling in the sort
// package for these small diagnostic / candidate-set lists.
func sortStrings(out []string) {
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1] > out[j]; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
}

// Size returns the current count. Diagnostic helper.
func (r *DynamicRegistry) Size() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.entries)
}
