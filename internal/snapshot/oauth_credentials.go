package snapshot

// The oauth_credentials section (RFC GB). credential_defs is otherwise never
// carried: its rows are sealed under the source's key and even their names
// stay behind. An OAuth credential is the exception, and only partly: its
// config, URL restriction and key travel — what a person set up and would
// otherwise have to set up again on the target — and nothing sealed. It is
// restored without a grant, so its owner only connects it again.

import (
	"context"
	"errors"
	"fmt"

	"github.com/denn-gubsky/loomcycle/internal/credential"
	"github.com/denn-gubsky/loomcycle/internal/credential/credentialdef"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// OAuthCredentialsSection carries every tenant's OAuth credentials' config.
type OAuthCredentialsSection struct {
	Version string                 `json:"version"`
	Entries []OAuthCredentialEntry `json:"entries"`
}

// OAuthCredentialEntry is one OAuth credential: its key, config and URL
// restriction. It holds no secret.
type OAuthCredentialEntry struct {
	TenantID    string           `json:"tenant_id,omitempty"`
	Scope       string           `json:"scope"`
	ScopeID     string           `json:"scope_id,omitempty"`
	Name        string           `json:"name"`
	Config      OAuthConfigEntry `json:"config"`
	AllowedURLs []string         `json:"allowed_urls"`
	// HadGrant: the credential was connected at some point, so it restores
	// as needs_reauth rather than as never connected.
	HadGrant bool `json:"had_grant,omitempty"`
}

// OAuthConfigEntry mirrors credential.OAuthConfig field by field. The
// envelope keeps its own type so a field added to the credential's config
// does not ride into snapshots without review (the key-set pins).
type OAuthConfigEntry struct {
	Resource string                 `json:"resource"`
	Issuer   string                 `json:"issuer,omitempty"`
	Client   OAuthClientConfigEntry `json:"client"`
	Scopes   []string               `json:"scopes,omitempty"`
}

// OAuthClientConfigEntry mirrors credential.OAuthClientConfig.
type OAuthClientConfigEntry struct {
	Registration      string `json:"registration"`
	TokenEndpointAuth string `json:"token_endpoint_auth"`
}

func oauthConfigEntry(c credential.OAuthConfig) OAuthConfigEntry {
	return OAuthConfigEntry{Resource: c.Resource, Issuer: c.Issuer, Scopes: c.Scopes,
		Client: OAuthClientConfigEntry{Registration: c.Client.Registration, TokenEndpointAuth: c.Client.TokenEndpointAuth}}
}

func (c OAuthConfigEntry) config() credential.OAuthConfig {
	return credential.OAuthConfig{Resource: c.Resource, Issuer: c.Issuer, Scopes: c.Scopes,
		Client: credential.OAuthClientConfig{Registration: c.Client.Registration, TokenEndpointAuth: c.Client.TokenEndpointAuth}}
}

// captureOAuthCredentials reads every credential row and keeps the OAuth
// ones' carried part. out stays nil when there are none, so a capture
// without OAuth credentials is byte-identical to one taken before the
// section existed.
func captureOAuthCredentials(ctx context.Context, s store.Store, out **OAuthCredentialsSection) error {
	rows, err := s.CredentialDefListAll(ctx)
	if err != nil {
		return fmt.Errorf("snapshot: read credential_defs: %w", err)
	}
	var entries []OAuthCredentialEntry
	for _, r := range rows {
		c, ok := credential.CarriedOAuth(r)
		if !ok {
			continue
		}
		entries = append(entries, OAuthCredentialEntry{
			TenantID: r.TenantID, Scope: r.Scope, ScopeID: r.ScopeID, Name: r.Name,
			Config: oauthConfigEntry(c.Config), AllowedURLs: c.AllowedURLs, HadGrant: c.HadGrant,
		})
	}
	if len(entries) > 0 {
		*out = &OAuthCredentialsSection{Version: SectionVersion, Entries: entries}
	}
	return nil
}

// restoreOAuthCredentials creates each OAuth credential the target does not
// hold, without a grant. A credential of the same key already there — of
// any kind — is left as it is, with a warning; an entry whose config or URL
// restriction a create here would refuse is skipped with a warning.
func restoreOAuthCredentials(ctx context.Context, s store.Store, sec *OAuthCredentialsSection, result *RestoreResult) {
	for _, e := range sec.Entries {
		where := fmt.Sprintf("oauth credential %s/%s/%s/%s", e.TenantID, e.Scope, e.ScopeID, e.Name)
		switch e.Scope {
		case "tenant", "user", "agent":
		default:
			result.Warnings = append(result.Warnings, where+": skipped: unknown scope")
			continue
		}
		if _, err := s.CredentialDefGet(ctx, e.TenantID, e.Scope, e.ScopeID, e.Name); err == nil {
			result.Warnings = append(result.Warnings, where+": a credential of this name is already on this host; left as it is")
			continue
		} else if nf := (*store.ErrNotFound)(nil); !errors.As(err, &nf) {
			result.Warnings = append(result.Warnings, fmt.Sprintf("%s: skipped: %v", where, err))
			continue
		}
		def, err := credential.RestoredOAuthDefinition(credential.OAuthCarried{Config: e.Config.config(), AllowedURLs: e.AllowedURLs, HadGrant: e.HadGrant})
		if err != nil {
			result.Warnings = append(result.Warnings, fmt.Sprintf("%s: skipped: %v", where, err))
			continue
		}
		if _, err := s.CredentialDefPut(ctx, credentialdef.Row{
			TenantID: e.TenantID, Scope: e.Scope, ScopeID: e.ScopeID, Name: e.Name,
			Backend: credential.BackendInline, Definition: def,
		}); err != nil {
			result.Warnings = append(result.Warnings, fmt.Sprintf("%s: skipped: %v", where, err))
			continue
		}
		result.OAuthCredentialsRestored++
	}
	if result.OAuthCredentialsRestored > 0 {
		result.Warnings = append(result.Warnings, fmt.Sprintf(
			"%d OAuth credential(s) restored without their grant: each owner connects theirs again with oauth_connect_start", result.OAuthCredentialsRestored))
	}
}
