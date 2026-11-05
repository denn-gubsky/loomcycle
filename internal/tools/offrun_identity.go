package tools

// The off-run substrate planes (the HTTP /v1/_* routes, the gRPC substrate
// RPCs, the MCP server's builtin tools) call the tools with no run behind
// them, so each stamps a synthetic identity: this name as the agent name (and
// the user id with no principal to key on). Every caller of a plane gets the
// same one, so its agent scope is the OPERATOR's off-run scope, never a
// member's (RFC FY Part A). Defined once so every gate that must keep members
// off these scopes names the same three.
const (
	OffRunOperatorHTTP = "http-admin"
	OffRunOperatorGRPC = "grpc-admin"
	OffRunOperatorMCP  = "mcp-operator"
)

// IsOffRunOperatorIdentity reports whether name is one of the synthetic
// off-run identities.
func IsOffRunOperatorIdentity(name string) bool {
	switch name {
	case OffRunOperatorHTTP, OffRunOperatorGRPC, OffRunOperatorMCP:
		return true
	}
	return false
}
