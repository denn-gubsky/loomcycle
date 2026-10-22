package authz

import "strings"

// Registry name prefixes. The tool registry names a mounted MCP server's
// tool `mcp__<server>__<tool>`, an A2A peer's skill `a2a__<peer>__<skill>`,
// a client-hosted tool `client:<name>` and a LocalAPI operation
// `localapi__<api>__<op>`; everything else is a built-in tool. The catalog
// gives each its own type so a rule can say `deny invoke mcp:jobs/*`
// without naming the registry's spelling.
const (
	mcpPrefix      = "mcp__"
	a2aPrefix      = "a2a__"
	clientPrefix   = "client:"
	localAPIPrefix = "localapi__"
	nameSep        = "__"
)

// ResourceForTool maps a registered tool name to the catalog resource that
// governs invoking it (owner decision D8: every registered tool is
// governed). The census test in the tool layer holds that every prefix the
// registry can produce is handled here.
func ResourceForTool(name string) Resource {
	switch {
	case strings.HasPrefix(name, mcpPrefix):
		return Resource{Type: "mcp", Name: splitRegistryName(strings.TrimPrefix(name, mcpPrefix))}
	case strings.HasPrefix(name, a2aPrefix):
		return Resource{Type: "a2a", Name: splitRegistryName(strings.TrimPrefix(name, a2aPrefix))}
	case strings.HasPrefix(name, clientPrefix):
		return Resource{Type: "client", Name: strings.TrimPrefix(name, clientPrefix)}
	case strings.HasPrefix(name, localAPIPrefix):
		return Resource{Type: "localapi", Name: splitRegistryName(strings.TrimPrefix(name, localAPIPrefix))}
	}
	return Resource{Type: "tool", Name: name}
}

// splitRegistryName turns `<server>__<tool>` into `<server>/<tool>` (only the
// first `__` is the separator; a tool name may itself carry one).
func splitRegistryName(rest string) string {
	server, tool, ok := strings.Cut(rest, nameSep)
	if !ok {
		return rest
	}
	return server + "/" + tool
}

// ActionForVolumeMode maps a volume binding mode to the verb a rule must
// allow for it: `ro` needs read, `rw` needs write (write implies read on a
// volume: a principal allowed to write may read).
func ActionForVolumeMode(mode string) string {
	if mode == "ro" {
		return "read"
	}
	return "write"
}
