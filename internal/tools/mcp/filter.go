package mcp

// ApplyToolsFilter narrows a server's discovered tool descriptors
// to the operator-permitted subset (yaml `mcp_servers.<name>.tools`).
// Empty allowed = pass-through (default behaviour: expose every tool the
// server advertises).
//
// Lives here so both the boot-time path in cmd/loomcycle/main.go and the
// lazy-retry path in LazyResolver share one implementation.
func ApplyToolsFilter(descs []ToolDescriptor, allowed []string) []ToolDescriptor {
	if len(allowed) == 0 {
		return descs
	}
	allowSet := make(map[string]struct{}, len(allowed))
	for _, name := range allowed {
		allowSet[name] = struct{}{}
	}
	out := make([]ToolDescriptor, 0, len(descs))
	for _, d := range descs {
		if _, ok := allowSet[d.Name]; ok {
			out = append(out, d)
		}
	}
	return out
}
