package connector

import "fmt"

// ValidateUserCredentialsMap validates each key in the v1.x RFC F
// per-run credentials map against the wire-locked charset. Lives in
// the connector package so all four transports (HTTP, gRPC, MCP,
// future) share one source of truth — the RFC's "validation enforced
// at all 4 entry points" sharp edge demands it.
//
// Key contract: [a-zA-Z0-9_-]{1,64}. The regex in
// internal/tools/mcp/http/substitute.go's runCredRe MUST match this
// charset — a key passing validation here that fails the substitute
// regex would silently drop headers; that diverges from RFC Decision 4.
//
// Values are NOT validated (operators legitimately pass JWTs, opaque
// tokens, signed payloads — no length or charset constraints make
// sense). Empty map is valid (= run uses no per-tool auth); nil is
// valid.
//
// Returns errMsg suitable for a 400 Bad Request / gRPC
// InvalidArgument / MCP tool-error response naming the offending key.
func ValidateUserCredentialsMap(m map[string]string) (errMsg string, ok bool) {
	for k := range m {
		if !validCredentialKey(k) {
			return fmt.Sprintf(`user_credentials: key %q must match [a-zA-Z0-9_-]{1,64}`, k), false
		}
	}
	return "", true
}

// validCredentialKey reports whether k is a valid key for the v1.x
// RFC F per-run credentials map. Charset: [a-zA-Z0-9_-], length
// 1..64. Keys this strict pass through yaml + JSON + URL paths
// without escaping and align with the regex in
// internal/tools/mcp/http/substitute.go's runCredRe — a single
// source-of-truth shape, validated at every wire entry point.
func validCredentialKey(k string) bool {
	if len(k) == 0 || len(k) > 64 {
		return false
	}
	for _, r := range k {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '_', r == '-':
			continue
		default:
			return false
		}
	}
	return true
}
