// Package credentialdef holds the stored form of an RFC AR CredentialDef: the
// row the credential engine reads and writes and the store persists.
//
// It is a leaf of its own rather than a type in package credential because the
// store imports it (the store sits above the credential layer, RFC EX) while
// package credential's own tests open a real store. With the row in package
// credential, those tests would import a store that imports credential — an
// import cycle.
package credentialdef

import (
	"encoding/json"
	"time"
)

// Row is a single RFC AR credential: a named secret scoped to a
// (tenant, scope, scope_id) bucket. Definition holds ONLY sealed ciphertext
// (inline backend) or an external-backend pointer — NEVER a plaintext secret;
// callers that surface a row to a client MUST strip Definition (metadata-only).
type Row struct {
	// TenantID is the RFC N tenant-isolation axis. "" = shared/operator/legacy.
	// Set from the authoritative principal at the write site, never the wire.
	TenantID string `json:"tenant_id,omitempty"`
	// Scope is "tenant" | "user" | "agent". ScopeID is "" for tenant, the user
	// subject for user scope, the agent name for agent scope — together with
	// name they form the row key, so user A's token can't collide with B's.
	Scope   string `json:"scope"`
	ScopeID string `json:"scope_id"`
	Name    string `json:"name"`
	// Backend is "inline" (sealed value) | "vault" | "aws_sm" | "gcp_sm" |
	// "onepassword" (pointer only).
	Backend string `json:"backend"`
	// Definition is the sealed value ({"value":{key_id,nonce,ciphertext}}) for
	// inline, or the external pointer JSON. Never plaintext, never logged.
	Definition json.RawMessage `json:"definition"`
	// ExpiresAt is when the credential stops resolving; nil = no expiry. The
	// credential engine refuses an expired row on every by-name lookup; the
	// row is kept so it can be listed and rotated.
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
}
