package tools

import (
	"context"
	"fmt"

	"github.com/denn-gubsky/loomcycle/internal/credential"
)

// CredentialSubstitute replaces the "$cred:<name>" and "$ghapp:<name>"
// references in s with their values for a request to targetURL, for the run
// identity on ctx (tenant, user, and the agent named by AgentName). It
// returns the names it could not resolve. main wires the credential engine
// behind it; the MCP client and the A2A tool share it.
type CredentialSubstitute func(ctx context.Context, s, targetURL string) (string, []string, error)

// HasCredentialRef reports whether v holds a stored-credential reference.
func HasCredentialRef(v string) bool {
	return len(credential.RefNames(v)) > 0 || len(credential.GHAppRefNames(v)) > 0
}

// ResolveRunCredential opens the references in v, a value a RUN carries (its
// user_bearer or one of its user_credentials), for a request to targetURL.
// A value with no reference is returned as it is.
//
// tenantOnly resolves at the tenant level of the run's tenant: the run's user
// and the calling agent are cleared from the identity the substitute reads,
// so a same-named personal or agent credential cannot stand in for the one
// the value's author meant.
//
// Every failure wraps ErrRunCredentialUnavailable, and its text names no
// store or scope: callers return it to the model. A resolved value that
// itself spells a reference is refused rather than resolved again.
func ResolveRunCredential(ctx context.Context, subst CredentialSubstitute, v, targetURL string, tenantOnly bool) (string, error) {
	if !HasCredentialRef(v) {
		return v, nil
	}
	if subst == nil {
		return "", fmt.Errorf("the run's credential is a stored reference, which this caller cannot resolve: %w", ErrRunCredentialUnavailable)
	}
	if tenantOnly {
		ident := RunIdentity(ctx)
		ident.UserID = ""
		ctx = WithAgentName(WithRunIdentity(ctx, ident), "")
	}
	out, unresolved, err := subst(ctx, v, targetURL)
	if err != nil || len(unresolved) > 0 {
		return "", fmt.Errorf("the run's stored credential could not be resolved: %w", ErrRunCredentialUnavailable)
	}
	if HasCredentialRef(out) {
		return "", fmt.Errorf("the run's stored credential holds a credential reference, which is refused: %w", ErrRunCredentialUnavailable)
	}
	return out, nil
}
