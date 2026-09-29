package tools

import (
	"context"

	"github.com/denn-gubsky/loomcycle/internal/auth"
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
