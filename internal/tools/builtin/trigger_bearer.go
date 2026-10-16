package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/denn-gubsky/loomcycle/internal/credential"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// checkUserBearerAuthority refuses, at create and fork, a webhook's or a
// schedule's user_bearer that the author may not name. The reference rides
// into the run unopened and the MCP client resolves it for each request at
// TENANT level of the run's tenant only (never a user's or an agent's scope:
// the author is neither — see the MCP client's runBearer), so the author must
// hold a tenant-level credential of that name in their own tenant. An author
// with no tenant would name the shared tenant's credentials, and is refused.
// An operator is not checked, so a credential may be created after its
// definition — the rule a webhook's stored secret follows.
//
// Not part of the definition's validation: a snapshot restore re-runs that,
// and an author's tenant is not a restore's.
func checkUserBearerAuthority(ctx context.Context, creds *credential.Engine, ref string) error {
	if ref == "" || callerHasOperatorAuthority(ctx) {
		return nil
	}
	name, _ := credential.ParseRef(ref) // the shape check passed
	tenantID := tools.RunIdentity(ctx).TenantID
	if tenantID == "" {
		return fmt.Errorf("user_bearer %q may not be set by an author with no tenant: it would read the shared tenant's credentials", ref)
	}
	if creds == nil {
		return fmt.Errorf("user_bearer %q: this server stores no credentials", ref)
	}
	// Tenant level only (no agent, no user), and no destination yet: which
	// MCP server the bearer reaches is the run's business, and a credential
	// restricted by allowed_urls is checked against each one when it is sent.
	found, _, err := creds.HasKeyFor(ctx, tenantID, "", "", name, "")
	if errors.Is(err, credential.ErrExpired) {
		return fmt.Errorf("user_bearer %q: %v", ref, err)
	}
	if err != nil {
		return fmt.Errorf("user_bearer %q: could not check the credential: %v", ref, err)
	}
	if !found {
		return fmt.Errorf("user_bearer %q names no tenant-level credential in tenant %q. "+
			"Create it first with CredentialDef (op=create, scope=tenant, name=%q)", ref, tenantID, name)
	}
	return nil
}

// overlayClearsUserBearer reports whether a create/fork overlay names
// user_bearer with an empty value (or null). The merged field is a string
// whose "" means "keep the parent's" in applyOverlay, so a fork could never
// remove a bearer; a key present and empty is the author emptying the field.
func overlayClearsUserBearer(overlay json.RawMessage) bool {
	var top map[string]json.RawMessage
	if json.Unmarshal(overlay, &top) != nil {
		return false
	}
	raw, ok := top["user_bearer"]
	if !ok {
		return false
	}
	var v *string
	return json.Unmarshal(raw, &v) == nil && (v == nil || *v == "")
}
