package lookup

import (
	"github.com/denn-gubsky/loomcycle/internal/config"
)

// MCPDynamicRegistry is the subset of mcp.DynamicRegistry the
// MCPServer resolver consults — broken out so internal/lookup
// doesn't import internal/tools/mcp (which would invert the existing
// dependency direction; tools/mcp is consumed by everything else).
type MCPDynamicRegistry interface {
	Get(name string) (MCPServerSpec, bool)
}

// MCPServerSpec is the runtime spec for an MCP server registration.
// Mirrors mcp.DynamicMCPServerSpec but lives in this package so the
// lookup chain can return a uniform shape regardless of whether the
// name came from the static yaml or the dynamic registry.
//
// (The static cfg.MCPServer has additional yaml-only fields like
// `command`, `args`, `env`, `pool_size` for stdio servers; those
// don't apply to dynamically-registered http / streamable-http
// servers — the substrate refuses stdio at the create boundary.)
type MCPServerSpec struct {
	Transport string
	URL       string
	Headers   map[string]string
	// AllowedTools is the operator's per-server tools/list narrowing (yaml
	// `allowed_tools`); empty = allow all discovered tools. Only the STATIC
	// source carries it — a dynamically-registered (substrate) server has
	// none, so all its discovered tools are allowed and the agent's own
	// allowed_tools is the gate. Consumed by the MCP lazy tool resolver.
	AllowedTools []string
	// Source — "static" or "dynamic". Useful for log lines + the
	// /ui/library/mcp-servers page's badge.
	Source string
}

// MCPServer resolves an MCP server NAME to its effective runtime
// spec by walking the lookup chain in precedence order:
//
//  1. static cfg.MCPServers (yaml-defined; ground truth).
//  2. dynamic registry (v0.9.x substrate, rehydrated from
//     mcp_server_defs at boot + mutated by promote / retire).
//
// Returns (zero, false) when neither source has the name.
//
// Yaml takes precedence on name collisions: the substrate tool refuses
// `create` over a yaml-occupied name, and the boot-time loader skips
// dynamic rows whose name collides with yaml. This resolver enforces
// the same order at the lookup boundary so future refactors that add
// a third tier can't accidentally invert it.
//
// LIMITATION (intentional): MCPServerSpec is the HTTP / streamable-http
// subset. The stdio transport carries additional yaml-only fields
// (Command/Args/Env/PoolSize) that this resolver deliberately omits
// because the substrate refuses stdio at the create boundary — there's
// no path by which a dynamic row could carry those fields. As a
// consequence, this function cannot replace the inline pool build
// callback in cmd/loomcycle/main.go for the stdio case; that callback
// reads cfg.MCPServer directly to get the stdio fields.
//
// Callers that need the full spec (transport/url/headers) should use it
// only for http-transport servers (the /ui/library/mcp-servers GET
// endpoint). Callers that need ONLY membership + AllowedTools are
// transport-agnostic and safe for any server — notably the MCP lazy tool
// resolver (internal/tools/mcp/lazy.go), which delegates client
// construction to the pool and consults this resolver purely to decide
// "is this name known (static OR dynamic)?" + which tools the operator
// allowed. Routing the resolver through here is what keeps MCP membership
// from drifting static-only again (the bug fixed in #341).
func MCPServer(cfg *config.Config, dyn MCPDynamicRegistry, name string) (MCPServerSpec, bool) {
	if cfg != nil {
		if srv, ok := cfg.MCPServers[name]; ok {
			return MCPServerSpec{
				Transport:    srv.Transport,
				URL:          srv.URL,
				Headers:      srv.Headers,
				AllowedTools: srv.AllowedTools,
				Source:       "static",
			}, true
		}
	}
	if dyn != nil {
		if spec, ok := dyn.Get(name); ok {
			spec.Source = "dynamic"
			return spec, true
		}
	}
	return MCPServerSpec{}, false
}

// SubstrateMCPServer mirrors the JSON shape `MCPServerDef.create`
// persists in `mcp_server_defs.definition` (snake_case json tags via
// the `mcpServerOverlay` struct in
// internal/tools/builtin/mcpserverdef.go).
//
// Symmetric across marshal + unmarshal — both ends use json tags, so
// the "silent unmarshal drop" bug PR #184 exposed for AgentDef CAN'T
// fire here. Defined here for the same three reasons SubstrateSkillDef
// is defined: documentation, drift-test symmetry, and eliminating the
// duplicate type declaration that main.go's boot-time loader
// currently has inline.
type SubstrateMCPServer struct {
	Transport       string                   `json:"transport,omitempty"`
	URL             string                   `json:"url,omitempty"`
	Headers         map[string]string        `json:"headers,omitempty"`
	Description     string                   `json:"description,omitempty"`
	DiscoveredTools []SubstrateMCPServerTool `json:"discovered_tools,omitempty"`
}

// SubstrateMCPServerTool is the cached form of a single tool the
// upstream exposed via tools/list. Mirrors `toolDescriptor` in
// builtin/mcpserverdef.go.
type SubstrateMCPServerTool struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	// InputSchema is raw JSON — preserved verbatim to avoid double-
	// parse on every introspection read.
	InputSchema []byte `json:"input_schema,omitempty"`
}
