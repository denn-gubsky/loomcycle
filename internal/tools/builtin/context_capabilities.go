package builtin

import (
	"context"
	"sort"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/config"
	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/memory/backends/inprocess"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// execCapabilities answers "what does THIS deployment actually support?" so a
// caller can branch BEFORE calling instead of discovering a feature is absent
// by reading a refusal (vector_unsupported / embedder_not_configured /
// capability_unsupported / "SQL Memory is not enabled ..."). Every field is
// derived from live runtime state, never asserted.
//
// SECURITY POSTURE — this is a surface every agent and every MCP client can
// read, so two rules are absolute and are enforced by tests:
//
//  1. NO SECRETS. No API keys, no bearer tokens, no DSNs, no credential values
//     — and no `api_key_env` NAMES either, since the name of the variable an
//     operator chose is itself a hint about their key management.
//  2. NO INFRASTRUCTURE TOPOLOGY. No base URLs, hostnames, IPs, ports, file
//     paths, or container detail. `provider: ollama-local` is a capability;
//     `http://192.168.0.77:11434` is a map of the operator's network. This is
//     why the search block emits only the map KEYS of cfg.SearchProviders —
//     SearchProviderConfig carries a BaseURL, and marshalling the struct would
//     leak a SearXNG address to every agent.
//
// Tenant posture mirrors the routing view: availability goes to EVERY caller
// (a tenant genuinely needs to know whether recall works before calling it),
// and only what is purely operator infrastructure — the storage backend kind —
// is admin-gated. A tenant's output is therefore always a subset of an admin's.
func (c *Context) execCapabilities(ctx context.Context) (tools.Result, error) {
	// Open mode (no principal configured) is treated as admin, the same idiom
	// the routing view uses.
	admin := true
	if p, ok := auth.PrincipalFromContext(ctx); ok {
		admin = auth.HasScope(p.Scopes, auth.ScopeAdmin)
	}

	out := map[string]any{}

	// --- memory ---
	hasVectors := c.Store != nil && c.Store.SupportsVectors()
	vector := map[string]any{
		// End-to-end: an embedder alone cannot search, and a vector-capable
		// store with nothing to embed with cannot either.
		"available": hasVectors && c.Embedder != nil,
	}
	if c.Embedder != nil {
		// Provider/model/dimension only. The dimension is the one an agent
		// genuinely needs (it decides whether a stored vector is comparable);
		// none of the three is a secret or an address.
		emb := map[string]any{}
		if p := c.Embedder.Provider(); p != "" {
			emb["provider"] = p
		}
		if m := c.Embedder.Model(); m != "" {
			emb["model"] = m
		}
		if d := c.Embedder.Dimension(); d > 0 {
			emb["dimension"] = d
		}
		if len(emb) > 0 {
			vector["embedder"] = emb
		}
	}
	out["vector_memory"] = vector
	out["full_text_memory"] = map[string]any{
		"available": c.Store != nil && c.Store.SupportsFullText(),
	}
	// memory_layer: whether add/recall route at all. Probed against the DEFAULT
	// backend (the one an agent gets unless its def names another), by asking
	// the same question the Memory tool asks rather than assuming the answer.
	memLayer := false
	if c.Store != nil {
		_, memLayer = memrank.AsMemoryLayer(inprocess.New(c.Store, c.Embedder))
	}
	out["memory_layer"] = map[string]any{"available": memLayer}

	// --- storage-backed subsystems ---
	// SqlMem is the CONSTRUCTED manager, not the config flag: the flag says the
	// operator asked for SQL Memory, the pointer says they got it. Documents
	// need both it and the store, which is exactly the Document tool's guard.
	sqlMemOK := c.SqlMem != nil
	out["sql_memory"] = map[string]any{"available": sqlMemOK}
	out["documents"] = map[string]any{"available": sqlMemOK && c.Store != nil}

	// --- config-gated subsystems ---
	// Every key below is emitted UNCONDITIONALLY. With no config the zero Env
	// reports each feature as unavailable, rather than omitting the key: this
	// is a discovery surface whose entire purpose is letting a caller branch,
	// and an ABSENT key deserializes as undefined/nil — which is falsy in some
	// clients and a KeyError in others. Reporting "can't confirm" as
	// unavailable is also the fail-safe direction: a caller declines to
	// attempt rather than attempting something that will refuse.
	var env config.Env
	if c.Cfg != nil {
		env = c.Cfg.Env
	}
	out["bash"] = map[string]any{"available": env.BashEnabled}
	out["bashbox"] = map[string]any{"available": env.BashboxEnabled}
	// The scheduler and webhook loops both additionally require a store;
	// mirror the boot condition rather than the flag alone, or an agent
	// authoring a ScheduleDef is told it will fire when it never will.
	out["scheduler"] = map[string]any{"available": env.SchedulerEnabled && c.Store != nil}
	out["webhooks"] = map[string]any{"available": env.WebhooksEnabled && c.Store != nil}
	out["retention"] = map[string]any{"available": env.RetentionEnabled}
	out["code_js"] = map[string]any{"available": env.CodeAgentsEnabled}
	out["search"] = searchCapability(c.Cfg)
	out["consolidation"] = consolidationCapability(c.Cfg)
	out["limits"] = capabilityLimits(ctx, c.Cfg)
	if admin && c.Cfg != nil {
		// Purely operator infrastructure: an agent never needs it to decide
		// whether a call will work, and it describes the deployment rather
		// than a capability. Kind only — never a DSN, host, or path.
		// Normalized to "sqlite"/"postgres" at config load.
		out["storage"] = map[string]any{"backend": c.Cfg.Storage.Backend}
	}

	// sandbox: the sandboxed-code-execution surface arrives as MCP tools from a
	// builder sidecar, so there is no config flag to read — presence in THIS
	// run's tool list is the truth, and it is per-run by nature.
	out["sandbox"] = map[string]any{"available": hasSandboxTools(c.Tools)}

	return okJSON(out)
}

// searchCapability reports which web-search providers are configured, BY NAME
// ONLY. The map value (SearchProviderConfig) carries a BaseURL, so it is never
// serialized — only the keys are.
func searchCapability(cfg *config.Config) map[string]any {
	if cfg == nil {
		return map[string]any{"available": false, "providers": []string{}}
	}
	names := make([]string, 0, len(cfg.SearchProviders))
	for name := range cfg.SearchProviders {
		names = append(names, name)
	}
	// Back-compat: with no `search_providers:` block, a configured Brave key
	// still yields one working provider. Report the capability, never the key.
	if len(names) == 0 && cfg.Env.BraveAPIKey != "" {
		names = append(names, "brave")
	}
	sort.Strings(names)
	return map[string]any{
		"available": len(names) > 0,
		"providers": nonNilStrings(names),
	}
}

// consolidationCapability reports whether a memory consolidator is actually
// wired: a schedule pointing at an agent that holds the consolidation grant.
// `enabled` is the field that matters — a staged-off schedule means queued
// `Memory op=add` items are durable but nothing is draining them, which is
// exactly the state an agent should be able to detect rather than infer from
// a recall that keeps coming back empty.
func consolidationCapability(cfg *config.Config) map[string]any {
	configured, enabled := false, false
	if cfg == nil {
		return map[string]any{"available": false, "configured": false}
	}
	for _, sr := range cfg.ScheduledRuns {
		agent, ok := cfg.Agents[sr.Agent]
		if !ok || !agent.MemoryConsolidation {
			continue
		}
		configured = true
		if sr.Enabled {
			enabled = true
		}
	}
	return map[string]any{
		"available":  configured && enabled,
		"configured": configured,
	}
}

// capabilityLimits reports the caps an agent should respect. All are numbers
// an agent needs to size its own work; none identifies the deployment.
func capabilityLimits(ctx context.Context, cfg *config.Config) map[string]any {
	lim := map[string]any{}
	if cfg == nil {
		lim["memory_inject_max_tokens"] = config.DefaultMemoryInjectMaxTokens
		return lim
	}
	if n := cfg.Env.MaxRequestBytes; n > 0 {
		lim["max_request_bytes"] = n
	}
	if n := cfg.Env.MaxConsolidationTargets; n > 0 {
		lim["max_consolidation_targets"] = n
	}
	if n := cfg.Env.MaxConsolidationConcurrency; n > 0 {
		lim["max_consolidation_concurrency"] = n
	}
	// This agent's own memory-inject budget, resolved the way the injector
	// resolves it (0 on the def = the runtime default).
	inject := config.DefaultMemoryInjectMaxTokens
	if def, ok := cfg.Agents[tools.AgentName(ctx)]; ok && def.MemoryInjectMaxTokens > 0 {
		inject = def.MemoryInjectMaxTokens
	}
	lim["memory_inject_max_tokens"] = inject
	return lim
}

// hasSandboxTools reports whether this run carries the sandbox MCP toolset.
func hasSandboxTools(ts []tools.Tool) bool {
	for _, t := range ts {
		if t != nil && strings.HasPrefix(t.Name(), "mcp__sandbox__") {
			return true
		}
	}
	return false
}
