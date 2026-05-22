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
// callback needs. Mirrors the operator-yaml mcp_servers.* shape minus
// stdio bits (dynamic registration is HTTP + Streamable-HTTP only).
//
// Headers are stored verbatim with their ${LOOMCYCLE_*} / ${run.user_bearer}
// substitution placeholders intact — the http client's substitution
// pass at request-build time resolves them (matches yaml-loaded
// headers' semantics; see internal/tools/mcp/http/client.go).
type DynamicMCPServerSpec struct {
	Name      string
	Transport string // "http" | "streamable-http"
	URL       string
	Headers   map[string]string
}

// DynamicRegistry holds runtime-registered MCP server specs. One
// instance per loomcycle process; injected into the pool's build
// callback in main.go.
//
// Empty registry behaves the same as no registry at all — the pool's
// build callback falls back to the yaml-static map.
type DynamicRegistry struct {
	mu      sync.RWMutex
	entries map[string]DynamicMCPServerSpec
}

// NewDynamicRegistry returns an empty registry.
func NewDynamicRegistry() *DynamicRegistry {
	return &DynamicRegistry{entries: make(map[string]DynamicMCPServerSpec)}
}

// Set installs (or replaces) the entry for spec.Name. The pool's
// build callback consults the registry on every cache miss; existing
// pool entries for the same name continue serving until they crash
// or are explicitly evicted via PoolEvict (called by the tool's
// retire / promote-replaces-version paths).
func (r *DynamicRegistry) Set(spec DynamicMCPServerSpec) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries[spec.Name] = spec
}

// Remove evicts the named entry. Returns true if it existed.
//
// Removing an entry does NOT close existing pool clients on its own —
// the substrate-tool layer is responsible for invoking Pool.evict(name)
// (or the equivalent) to tear down any in-flight client BEFORE
// removing the registry entry. Otherwise an agent in-flight on the
// MCP server's tool call continues using the cached pool entry until
// the underlying transport crashes naturally.
func (r *DynamicRegistry) Remove(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.entries[name]; !ok {
		return false
	}
	delete(r.entries, name)
	return true
}

// Get returns the spec for `name` and a presence boolean. Called by
// the pool's build callback on every cache miss.
func (r *DynamicRegistry) Get(name string) (DynamicMCPServerSpec, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	spec, ok := r.entries[name]
	return spec, ok
}

// Names returns every registered name, sorted. Used by diagnostic
// surfaces (the LoomCycle MCP server's get_runtime_state, etc.).
//
// Safe to call concurrently with Set/Remove — returns a snapshot.
func (r *DynamicRegistry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.entries))
	for name := range r.entries {
		out = append(out, name)
	}
	// Inline sort to avoid pulling in sort for one-call shape.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1] > out[j]; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}

// Size returns the current count. Diagnostic helper.
func (r *DynamicRegistry) Size() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.entries)
}
