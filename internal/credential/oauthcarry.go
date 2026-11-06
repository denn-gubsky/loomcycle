package credential

// What a snapshot carries of an OAuth credential (RFC GB, owner decision):
// its config, its URL restriction and its key — never its sealed secret,
// so no token, device code or client registration, and no status time that
// would imply a grant. On the target it is restored without a grant, and its
// owner connects it again.

import (
	"encoding/json"

	"github.com/denn-gubsky/loomcycle/internal/credential/credentialdef"
)

// OAuthCarried is the part of an OAuth credential a snapshot carries.
type OAuthCarried struct {
	Config      OAuthConfig
	AllowedURLs []string
	// HadGrant says the credential was ever connected (any state but
	// pending_connect): it then restores as needs_reauth, so its owner is
	// told to connect it again, rather than as never connected.
	HadGrant bool
}

// CarriedOAuth reads what a snapshot may carry of row. (_, false) for a row
// that is not an OAuth credential — every other credential stays behind —
// or whose definition does not read.
func CarriedOAuth(row credentialdef.Row) (OAuthCarried, bool) {
	if defKind(row.Definition) != kindOAuth {
		return OAuthCarried{}, false
	}
	d, err := parseOAuthDef(row.Definition)
	if err != nil {
		return OAuthCarried{}, false
	}
	return OAuthCarried{
		Config:      d.Config,
		AllowedURLs: append([]string{}, (*d.AllowedURLs)...),
		HadGrant:    d.Status.State != OAuthPendingConnect,
	}, true
}

// RestoredOAuthDefinition builds the stored definition of an OAuth
// credential a snapshot brings back, as PutOAuth builds a new one: its config
// and URL restriction checked as a create checks them, no secret, and
// needs_reauth (pending_connect when it never had a grant). Nothing in it is
// sealed, so it needs no key: the first connect seals the grant.
//
// A missing or empty carried list restores as an empty restriction (sent
// nowhere), never as the resource default: the restore fails closed rather
// than widen what the source allowed.
func RestoredOAuthDefinition(c OAuthCarried) (json.RawMessage, error) {
	allowed := append([]string{}, c.AllowedURLs...)
	norm, err := NormalizeAllowedURLs(&allowed)
	if err != nil {
		return nil, err
	}
	status := OAuthStatus{State: OAuthPendingConnect}
	if c.HadGrant {
		status = OAuthStatus{State: OAuthNeedsReauth, LastError: "restored from a snapshot; connect it again"}
	}
	return newOAuthDefinition(c.Config, norm, status)
}
