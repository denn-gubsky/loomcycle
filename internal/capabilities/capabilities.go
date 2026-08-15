// Package capabilities answers "what does THIS deployment actually support?"
//
// It exists so the two surfaces that answer that question — the in-band
// `Context op=capabilities` tool and the out-of-band GET /v1/config — compute it
// from ONE place. Duplicating the probe would drift silently, which is the exact
// failure class v1.34.0 was themed on: the runtime reporting something other
// than what it does, with nothing saying so.
//
// SECURITY POSTURE — every consumer of this package is readable by someone the
// operator did not individually vet (any agent, any MCP client, and with
// LOOMCYCLE_PUBLIC_CONFIG, the public internet). Two rules are absolute and are
// enforced by tests in both callers:
//
//  1. NO SECRETS. No API keys, bearer tokens, DSNs, or credential values — and
//     no `api_key_env` NAMES either, since the name of the variable an operator
//     chose is itself a hint about their key management.
//  2. NO INFRASTRUCTURE TOPOLOGY. No base URLs, hostnames, IPs, ports, file
//     paths, or container detail. `provider: ollama-local` is a capability;
//     `http://192.168.0.77:11434` is a map of the operator's network. That is
//     why Search emits only the map KEYS of cfg.SearchProviders —
//     SearchProviderConfig carries a BaseURL, and marshalling the struct would
//     leak a SearXNG address to every reader.
package capabilities

import (
	"sort"

	"github.com/denn-gubsky/loomcycle/internal/config"
	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/memory/backends/inprocess"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// Inputs is the live runtime state the probe reads. Every field is what the
// caller already holds; nothing is looked up globally, so a test can plant any
// combination.
type Inputs struct {
	Cfg      *config.Config
	Store    store.Store        // nil ⇒ no persistence
	Embedder providers.Embedder // nil ⇒ nothing to embed with
	// SQLMem is whether the manager was CONSTRUCTED, not whether the operator
	// asked for it: the config flag says they requested SQL Memory, the pointer
	// says they got it.
	SQLMem bool
	// Admin gates the one purely-operator-infrastructure key. A non-admin's
	// output is therefore always a subset of an admin's.
	Admin bool
}

// Deployment reports the deployment-level capability map. Every value is derived
// from live state, never asserted.
//
// It deliberately excludes the two per-RUN keys the Context tool adds on top —
// `sandbox` (presence in that run's tool list) and `limits` (which carries the
// calling agent's own inject budget) — because neither is a property of the
// deployment and neither is answerable here.
func Deployment(in Inputs) map[string]any {
	out := map[string]any{}

	// --- memory ---
	hasVectors := in.Store != nil && in.Store.SupportsVectors()
	vector := map[string]any{
		// End-to-end: an embedder alone cannot search, and a vector-capable
		// store with nothing to embed with cannot either.
		"available": hasVectors && in.Embedder != nil,
	}
	if in.Embedder != nil {
		// Provider/model/dimension only. The dimension is the one a caller
		// genuinely needs (it decides whether a stored vector is comparable);
		// none of the three is a secret or an address.
		emb := map[string]any{}
		if p := in.Embedder.Provider(); p != "" {
			emb["provider"] = p
		}
		if m := in.Embedder.Model(); m != "" {
			emb["model"] = m
		}
		if d := in.Embedder.Dimension(); d > 0 {
			emb["dimension"] = d
		}
		if len(emb) > 0 {
			vector["embedder"] = emb
		}
	}
	out["vector_memory"] = vector
	out["full_text_memory"] = map[string]any{
		"available": in.Store != nil && in.Store.SupportsFullText(),
	}
	// memory_layer: whether add/recall route at all. Probed against the DEFAULT
	// backend (the one an agent gets unless its def names another), by asking
	// the same question the Memory tool asks rather than assuming the answer.
	memLayer := false
	if in.Store != nil {
		_, memLayer = memrank.AsMemoryLayer(inprocess.New(in.Store, in.Embedder))
	}
	out["memory_layer"] = map[string]any{"available": memLayer}

	// --- storage-backed subsystems ---
	// Documents need both the manager and the store, which is exactly the
	// Document tool's own guard.
	out["sql_memory"] = map[string]any{"available": in.SQLMem}
	out["documents"] = map[string]any{"available": in.SQLMem && in.Store != nil}

	// --- config-gated subsystems ---
	// Every key below is emitted UNCONDITIONALLY. With no config the zero Env
	// reports each feature as unavailable, rather than omitting the key: this is
	// a discovery surface whose entire purpose is letting a caller branch, and an
	// ABSENT key deserializes as undefined/nil — falsy in some clients, a
	// KeyError in others. Reporting "can't confirm" as unavailable is also the
	// fail-safe direction: a caller declines to attempt rather than attempting
	// something that will refuse.
	var env config.Env
	if in.Cfg != nil {
		env = in.Cfg.Env
	}
	out["bash"] = map[string]any{"available": env.BashEnabled}
	out["bashbox"] = map[string]any{"available": env.BashboxEnabled}
	// The scheduler and webhook loops both additionally require a store; mirror
	// the boot condition rather than the flag alone, or a caller authoring a
	// ScheduleDef is told it will fire when it never will.
	out["scheduler"] = map[string]any{"available": env.SchedulerEnabled && in.Store != nil}
	out["webhooks"] = map[string]any{"available": env.WebhooksEnabled && in.Store != nil}
	out["retention"] = map[string]any{"available": env.RetentionEnabled}
	out["code_js"] = map[string]any{"available": env.CodeAgentsEnabled}
	out["search"] = Search(in.Cfg)
	out["consolidation"] = Consolidation(in.Cfg)
	if in.Admin && in.Cfg != nil {
		// Purely operator infrastructure: a caller never needs it to decide
		// whether a call will work, and it describes the deployment rather than a
		// capability. Kind only — never a DSN, host, or path. Normalized to
		// "sqlite"/"postgres" at config load.
		out["storage"] = map[string]any{"backend": in.Cfg.Storage.Backend}
	}
	return out
}

// Search reports which web-search providers are configured, BY NAME ONLY. The
// map value (SearchProviderConfig) carries a BaseURL, so it is never serialized
// — only the keys are.
func Search(cfg *config.Config) map[string]any {
	names := SearchProviderNames(cfg)
	return map[string]any{
		"available": len(names) > 0,
		"providers": names,
	}
}

// SearchProviderNames is the sorted, non-nil name set behind Search, exposed so
// a caller that renders its own shape does not re-derive the back-compat rule.
func SearchProviderNames(cfg *config.Config) []string {
	if cfg == nil {
		return []string{}
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
	if names == nil {
		return []string{}
	}
	return names
}

// Consolidation reports whether a memory consolidator is actually wired: a
// schedule pointing at an agent that holds the consolidation grant.
//
// `enabled` is the field that matters — a staged-off schedule means queued
// `Memory op=add` items are durable but nothing is draining them, which is
// exactly the state a caller should be able to detect rather than infer from a
// recall that keeps coming back empty.
//
// The two similarity bands ride along because a caller reasoning about
// duplicates otherwise has no way to know where the line is — and, more to the
// point, no way to notice the line is somewhere nothing ever reaches. Cosine
// scale is a property of the embedding model, so a band that merges nothing on
// this deployment's embedder is a real and completely silent state
// (`loomcycle memory-calibrate` is what measures it). They are reported as
// EFFECTIVE values so an unset config reads as the number in force rather than
// as absent — a `0` here would be a lie about what the consolidator does.
func Consolidation(cfg *config.Config) map[string]any {
	if cfg == nil {
		var unset config.ConsolidationConfig
		return map[string]any{
			"available":         false,
			"configured":        false,
			"merge_threshold":   unset.EffectiveMergeThreshold(),
			"related_threshold": unset.EffectiveRelatedThreshold(),
			"verify_writes":     false,
		}
	}
	configured, enabled := false, false
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
		"available":         configured && enabled,
		"configured":        configured,
		"merge_threshold":   cfg.Memory.Consolidation.EffectiveMergeThreshold(),
		"related_threshold": cfg.Memory.Consolidation.EffectiveRelatedThreshold(),
		// Reported so the pass can read it. Non-secret, and a boolean an operator
		// set themselves.
		"verify_writes": cfg.Memory.Consolidation.VerifyWrites,
	}
}

// DeploymentLimits reports the caps that describe the DEPLOYMENT. Numbers only;
// none identifies the deployment or carries topology.
//
// The calling agent's own inject budget is deliberately absent — that is
// per-run, and the Context tool overlays it.
func DeploymentLimits(cfg *config.Config) map[string]any {
	lim := map[string]any{}
	if cfg == nil {
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
	return lim
}
