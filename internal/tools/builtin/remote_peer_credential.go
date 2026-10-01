package builtin

import (
	"context"
	"fmt"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/credential"
)

// A remote memory backend or document source sends `Authorization: Bearer
// <credential>` to its base_url. Besides an env-var name, config.api_key_env
// may hold a reference to a stored credential, "$cred:<name>", so a tenant can
// key its own peer without the operator setting an env var for it.

// PeerCredentials resolves the "$cred:<name>" a remote peer def names, over the
// one credential engine every other $cred: consumer uses.
//
// The reference resolves in the def's OWNING tenant — the layer lookup read it
// from (lookup.Provenance.TenantID) — and at TENANT scope only:
//
//   - the owning tenant, never the calling run's: a shared ("") def is resolved
//     by every tenant, and resolving in the caller's tenant would send each
//     tenant's same-named credential to a base_url that tenant did not choose.
//   - tenant scope, never the run's user or agent credential (the precedence
//     an MCP header's $cred: uses): the def belongs to the tenant, and a
//     user-scope match would send one member's own token to a host another
//     member picked.
//
// A nil *PeerCredentials (no credential store wired) has no credentials.
type PeerCredentials struct {
	engine   *credential.Engine
	register func(string)
}

// NewPeerCredentials wraps the server's credential engine. register, if
// non-nil, receives each resolved value so the caller can mask a downstream
// echo of it (the server redactor), as the MCP $cred: resolver does.
func NewPeerCredentials(engine *credential.Engine, register func(string)) *PeerCredentials {
	return &PeerCredentials{engine: engine, register: register}
}

// Exists reports whether tenantID holds a tenant-level credential named name.
// Metadata only: nothing is decrypted.
func (p *PeerCredentials) Exists(ctx context.Context, tenantID, name string) (bool, error) {
	if p == nil || p.engine == nil {
		return false, nil
	}
	return p.engine.HasKey(ctx, tenantID, "", "", name)
}

// Resolve returns the value of the tenant-level credential name in tenantID.
// Errors name the reference and the tenant, never a value.
func (p *PeerCredentials) Resolve(ctx context.Context, tenantID, name string) (string, error) {
	ref := "$cred:" + name
	if p == nil || p.engine == nil || !p.engine.CanResolve() {
		return "", fmt.Errorf("api_key_env %q: this server cannot resolve stored credentials (no credential key is configured)", ref)
	}
	res, found, err := p.engine.Resolve(ctx, tenantID, "", "", name)
	if err != nil {
		return "", fmt.Errorf("api_key_env %q: %w", ref, err)
	}
	if !found {
		return "", fmt.Errorf("api_key_env %q: tenant %q has no tenant-level credential of that name", ref, tenantID)
	}
	if p.register != nil {
		p.register(res.Value)
	}
	return res.Value, nil
}

// peerKeyResolver is the KeyResolver for a remote peer client built from a def
// read in ownerTenant: a "$cred:<name>" resolves there, through creds; any
// other name is an env var, gated as before (resolveCredentialEnv).
func peerKeyResolver(creds *PeerCredentials, ownerTenant string) func(context.Context, string) (string, error) {
	return func(ctx context.Context, name string) (string, error) {
		if ref, ok := credential.ParseRef(name); ok {
			return creds.Resolve(ctx, ownerTenant, ref)
		}
		return resolveCredentialEnv(name)
	}
}

// requireAPIKeyEnvShape is the shape check for config.api_key_env: a
// "$cred:<name>" reference, or an env-var name an author may use as a
// credential. Who may set which is requireAuthorBoundCredential's to decide.
func requireAPIKeyEnvShape(v string) error {
	if v == "" || config.EnvNameCredentialSafe(v) {
		return nil
	}
	if _, ok := credential.ParseRef(v); ok {
		return nil
	}
	return fmt.Errorf("config.api_key_env %q is not an allowed credential env var "+
		"(must be LOOMCYCLE_-prefixed or a known third-party key, and must not be one "+
		"of loomcycle's own infrastructure secrets) nor a $cred:<name> reference", v)
}
