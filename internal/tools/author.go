package tools

import (
	"context"

	"github.com/denn-gubsky/loomcycle/internal/aclres"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/authz"
)

// A definition that starts runs later — a schedule, a webhook, a team walk —
// fires with no token on ctx, so it carries its author's operator-key and
// isolation bits and the fired run is stamped from them. These two helpers are
// the one answer to "how restricted is whoever is authoring through ctx".
//
// Two sources can say so, and the more restrictive one wins:
//
//   - the RUN's own bits (RunIdentity). They are fixed at run start, inherited
//     unchanged by every sub-run and restored from the row on resume — where no
//     principal is on ctx at all. They also outlive a looser principal: a draft
//     started by another user keeps its creator's bits, while ctx carries the
//     starter's token.
//   - the live principal, when one is present. Off-run (the substrate plane)
//     this is the only source, exactly as before.
//
// Reading only the principal failed open for every run that has none, so a
// restricted run could leave behind a definition that fired unrestricted. An OR
// never widens: each source can only add a restriction.
//
// Neither source → false. That is open mode (no auth configured, nothing to
// restrict) or a principal that holds the operator key / is not confined, so
// there is no restriction to capture.

// AuthorOperatorKeyRestricted reports whether the author on ctx must be denied
// the operator's provider key. gateOn is the deployment gate; it governs only
// the principal's side — a run's bit was already computed under the gate when
// the run started, and resume enforces it as recorded.
func AuthorOperatorKeyRestricted(ctx context.Context, gateOn bool) bool {
	p, ok := auth.PrincipalFromContext(ctx)
	return RunIdentity(ctx).OperatorKeyRestricted || auth.OperatorKeyRestricted(p, ok, gateOn)
}

// AuthorIsolated reports whether the author on ctx is an isolated member whose
// runs must stay confined to its own user and agent scope. It has no deployment
// gate, like auth.IsIsolated.
func AuthorIsolated(ctx context.Context) bool {
	return RunIdentity(ctx).Isolated || auth.IsIsolated(auth.PrincipalFromContext(ctx))
}

// AuthorACLOwner is whose access-control set (RFC EY) binds what the author on
// ctx leaves behind to start runs later — a schedule, a webhook — recorded on
// the definition so each fire is narrowed by that set as it stands then (RFC
// EZ §1.5). In a run it is the run's originating caller (RunPolicy.ACL, which
// a resumed or restarted run carries with no principal on ctx); off-run the
// live principal's. A principal whose set could not be compiled is still
// named, with its class: the fire compiles the set again and refuses the run
// while it cannot, rather than the definition firing unnarrowed for ever.
//
// nil: no subject — open mode, or a run no set binds. The
// definition then fires as before, on its captured bits alone.
func AuthorACLOwner(ctx context.Context) *authz.SubjectRef {
	if acl := PolicyOf(ctx).ACL; acl != nil {
		return authz.RefOf(acl.Subject)
	}
	if p, ok := auth.PrincipalFromContext(ctx); ok {
		return aclres.RefOf(p)
	}
	return nil
}
