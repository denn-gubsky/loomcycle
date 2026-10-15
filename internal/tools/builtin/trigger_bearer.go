package builtin

import (
	"context"
	"errors"
	"fmt"

	"github.com/denn-gubsky/loomcycle/internal/credential"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// checkUserBearerAuthority refuses, at create and fork, a webhook's or a
// schedule's user_bearer that the author may not name. The reference rides
// into the run unopened and resolves for each MCP request in the run's tenant,
// for its agent, then its user, then the tenant; so the author must hold a
// credential of that name for the definition's agent or the tenant (or, for
// a schedule, its user) in the author's own tenant. An author with no tenant
// would name the shared tenant's credentials, and is refused. An operator is
// not checked, so a credential may be created after its definition — the
// rule a webhook's stored secret follows.
//
// Not part of the definition's validation: a snapshot restore re-runs that,
// and an author's tenant is not a restore's. userID is "" for a webhook,
// whose run's user may come from the delivery.
func checkUserBearerAuthority(ctx context.Context, creds *credential.Engine, agent, userID, ref string) error {
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
	// No destination yet: which MCP server the bearer reaches is the run's
	// business, and a credential restricted by allowed_urls is checked
	// against each one when it is sent.
	found, _, err := creds.HasKeyFor(ctx, tenantID, agent, userID, name, "")
	if errors.Is(err, credential.ErrExpired) {
		return fmt.Errorf("user_bearer %q: %v", ref, err)
	}
	if err != nil {
		return fmt.Errorf("user_bearer %q: could not check the credential: %v", ref, err)
	}
	if !found {
		return fmt.Errorf("user_bearer %q names no credential of agent %q or of tenant %q. "+
			"Create it first with CredentialDef (op=create, scope=tenant or scope=agent, name=%q)", ref, agent, tenantID, name)
	}
	return nil
}
