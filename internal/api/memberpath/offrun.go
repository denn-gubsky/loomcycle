package memberpath

import (
	"encoding/json"

	"github.com/denn-gubsky/loomcycle/internal/auth"
)

// OperatorPlane reports whether an off-run caller is the operator plane —
// open mode (no principal), the legacy token, an admin, or a substrate:tenant
// operator — rather than a member, whom the transports confine (RFC FY Part A).
// Shared by the HTTP, gRPC and MCP data planes so they draw the line in one
// place.
func OperatorPlane(p auth.Principal, ok bool) bool {
	return !ok || p.Legacy || auth.HasScope(p.Scopes, auth.ScopeAdmin) || auth.HasScope(p.Scopes, auth.ScopeTenant)
}

// NamesAgentScope reports whether a Document, Path or Memory call's input
// touches an agent scope: its scope (defaultScope when absent — Path's is
// agent, Document's user, Memory has none), a copy or move's to_scope, or a
// search's documents_scope. A member has no off-run agent scope on a
// transport that cannot name an agent, so such a call is refused there. Input
// that is not an object names no scope; the tool refuses it on its own.
func NamesAgentScope(input json.RawMessage, defaultScope string) bool {
	var in struct {
		Scope          string `json:"scope"`
		ToScope        string `json:"to_scope"`
		DocumentsScope string `json:"documents_scope"`
	}
	if json.Unmarshal(input, &in) != nil {
		return false
	}
	if in.Scope == "" {
		in.Scope = defaultScope
	}
	return in.Scope == "agent" || in.ToScope == "agent" || in.DocumentsScope == "agent"
}
