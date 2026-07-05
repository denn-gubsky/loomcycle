package lookup

import (
	"encoding/json"

	"github.com/denn-gubsky/loomcycle/internal/config"
)

// MCPDynamicRegistry is the subset of mcp.DynamicRegistry the
// MCPServer resolver consults — broken out so internal/lookup
// doesn't import internal/tools/mcp (which would invert the existing
// dependency direction; tools/mcp is consumed by everything else).
type MCPDynamicRegistry interface {
	// Get is an exact (tenantID, name) read — the resolver drives the
	// tenant→shared precedence by calling Get twice (RFC N).
	Get(tenantID, name string) (MCPServerSpec, bool)
}

// MCPServerSpec is the runtime spec for an MCP server registration.
// Mirrors mcp.DynamicMCPServerSpec but lives in this package so the
// lookup chain can return a uniform shape regardless of whether the
// name came from the static yaml or the dynamic registry.
//
// The static cfg.MCPServer also has `pool_size` (http) which this spec
// omits. Command/Args/Env carry the stdio invocation: empty for http
// transports, populated only for a `stdio` row, which the substrate
// accepts solely when LOOMCYCLE_MCP_ALLOW_DYNAMIC_STDIO is set (F31).
type MCPServerSpec struct {
	Transport string
	URL       string
	Headers   map[string]string
	// Command/Args/Env carry a dynamic stdio server's invocation (F31);
	// empty for http transports.
	Command string
	Args    []string
	Env     map[string]string
	// Tools is the operator's per-server tools/list narrowing (yaml
	// `tools`); empty = allow all discovered tools. Only the STATIC
	// source carries it — a dynamically-registered (substrate) server has
	// none, so all its discovered tools are allowed and the agent's own
	// tools is the gate. Consumed by the MCP lazy tool resolver.
	Tools []string
	// Source — "static" or "dynamic". Useful for log lines + the
	// /ui/library/mcp-servers page's badge.
	Source string
}

// MCPServer resolves an MCP server NAME to its effective runtime spec
// within the caller's tenant, walking the lookup chain in precedence
// order (MCP is STATIC-first — the opposite of skills):
//
//  1. (tenantID != "") tenant-scoped dynamic registry — a per-tenant
//     registration shadows the shared static base by name.
//  2. static cfg.MCPServers (yaml-defined; the shared operator base).
//  3. shared dynamic registry (tenant_id="", rehydrated from
//     mcp_server_defs at boot + mutated by promote / retire).
//
// Returns (zero, false) when no source has the name.
//
// For the default tenant "" step 1 is skipped, so the order collapses to
// static → shared-dynamic — byte-for-byte the pre-RFC-N behaviour
// (single-tenant deployments are unchanged). Yaml still takes precedence
// over a SHARED dynamic registration on name collisions: the substrate
// tool refuses `create` over a yaml-occupied name, and the boot loader
// skips shared dynamic rows whose name collides with yaml. A per-TENANT
// registration (step 1) deliberately CAN shadow the shared yaml base —
// that is the per-tenant override RFC N grants.
//
// The tenantID MUST come from the authoritative principal in ctx
// (auth.PrincipalFromContext → tools.RunIdentity fallback → ""), never
// from a wire/request field — see internal/api/http/server.go's
// tenantFromCtx.
//
// stdio (F31): when the operator sets LOOMCYCLE_MCP_ALLOW_DYNAMIC_STDIO,
// a dynamic row MAY carry transport=stdio with Command/Args/Env, and this
// resolver returns them so the pool build callback can spawn the child.
// STATIC yaml stdio servers are still resolved directly from
// cfg.MCPServers in the build callback (that first branch short-circuits
// before this resolver), so the static stdio path is unchanged.
//
// Callers that need the full spec (transport/url/headers, or stdio
// command/args/env) get it for any transport. Callers that need ONLY
// membership + Tools are
// transport-agnostic and safe for any server — notably the MCP lazy tool
// resolver (internal/tools/mcp/lazy.go), which delegates client
// construction to the pool and consults this resolver purely to decide
// "is this name known (static OR dynamic)?" + which tools the operator
// allowed. Routing the resolver through here is what keeps MCP membership
// from drifting static-only again (the bug fixed in #341).
func MCPServer(cfg *config.Config, dyn MCPDynamicRegistry, tenantID, name string) (MCPServerSpec, bool) {
	// 1. Tenant-scoped dynamic shadow (skipped for the shared "" tenant so
	//    its order stays static → shared-dynamic, exactly as pre-RFC-N).
	if dyn != nil && tenantID != "" {
		if spec, ok := dyn.Get(tenantID, name); ok {
			spec.Source = "dynamic"
			return spec, true
		}
	}
	// 2. Static cfg.MCPServers — the shared operator base.
	if cfg != nil {
		if srv, ok := cfg.MCPServers[name]; ok {
			return MCPServerSpec{
				Transport: srv.Transport,
				URL:       srv.URL,
				Headers:   srv.Headers,
				Tools:     srv.Tools,
				Source:    "static",
			}, true
		}
	}
	// 3. Shared dynamic registry (tenant_id="").
	if dyn != nil {
		if spec, ok := dyn.Get("", name); ok {
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
	Command         string                   `json:"command,omitempty"` // stdio (F31)
	Args            []string                 `json:"args,omitempty"`    // stdio (F31)
	Env             map[string]string        `json:"env,omitempty"`     // stdio (F31)
	Description     string                   `json:"description,omitempty"`
	DiscoveredTools []SubstrateMCPServerTool `json:"discovered_tools,omitempty"`
}

// SubstrateMCPServerTool is the cached form of a single tool the
// upstream exposed via tools/list. Mirrors `toolDescriptor` in
// builtin/mcpserverdef.go.
type SubstrateMCPServerTool struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	// InputSchema is raw JSON — preserved verbatim to avoid double-parse on
	// every introspection read. MUST be json.RawMessage, not a plain []byte:
	// the stored input_schema is a JSON OBJECT, and encoding/json unmarshals a
	// JSON object into []byte as an ERROR (it expects a base64 string), which
	// aborts the WHOLE def unmarshal. With []byte this silently dropped every
	// discovered tool (the def carrying any object schema failed to decode), so
	// the run-start enumerator advertised nothing even for a fully-discovered
	// server (F33). json.RawMessage captures the raw object verbatim.
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
}
