package mcp

import "github.com/denn-gubsky/loomcycle/internal/authz"

// The route table's tool scopes and run plane, as the tests written against
// the hand-kept maps read them (RFC EZ P6 moved those maps into
// authz.MCPToolRoute).
var toolRequiredScope, runPlaneTools = func() (map[string]string, map[string]bool) {
	scopes, plane := map[string]string{}, map[string]bool{}
	for name, t := range authz.MCPToolRoutes() {
		if t.Scope != "" {
			scopes[name] = t.Scope
		}
		if t.RunPlane {
			plane[name] = true
		}
	}
	return scopes, plane
}()
