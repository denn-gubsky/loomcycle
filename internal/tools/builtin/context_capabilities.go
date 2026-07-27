package builtin

import (
	"context"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/capabilities"
	"github.com/denn-gubsky/loomcycle/internal/config"
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

	// The deployment-level probe is shared with GET /v1/config
	// (internal/capabilities) so the two surfaces cannot report different
	// answers to the same question. Only the two genuinely per-RUN keys are
	// added here.
	out := capabilities.Deployment(capabilities.Inputs{
		Cfg:      c.Cfg,
		Store:    c.Store,
		Embedder: c.Embedder,
		// SqlMem is the CONSTRUCTED manager, not the config flag: the flag says
		// the operator asked for SQL Memory, the pointer says they got it.
		SQLMem: c.SqlMem != nil,
		Admin:  admin,
	})

	// limits carries THIS agent's inject budget, so it cannot come from the
	// deployment probe.
	out["limits"] = capabilityLimits(ctx, c.Cfg)

	// sandbox: the sandboxed-code-execution surface arrives as MCP tools from a
	// builder sidecar, so there is no config flag to read — presence in THIS
	// run's tool list is the truth, and it is per-run by nature.
	out["sandbox"] = map[string]any{"available": hasSandboxTools(c.Tools)}

	return okJSON(out)
}

// capabilityLimits reports the caps an agent should respect: the deployment's
// own numbers plus THIS agent's inject budget. All are numbers an agent needs to
// size its own work; none identifies the deployment.
func capabilityLimits(ctx context.Context, cfg *config.Config) map[string]any {
	lim := capabilities.DeploymentLimits(cfg)
	// This agent's own memory-inject budget, resolved the way the injector
	// resolves it (0 on the def = the runtime default). Per-run, so it is not
	// part of the shared deployment probe.
	inject := config.DefaultMemoryInjectMaxTokens
	if cfg != nil {
		if def, ok := cfg.Agents[tools.AgentName(ctx)]; ok && def.MemoryInjectMaxTokens > 0 {
			inject = def.MemoryInjectMaxTokens
		}
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
