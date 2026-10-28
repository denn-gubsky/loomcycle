package http

// authgate_usertokens.go — RFC FA §4: in strict mode a person's token must be
// one a gate issued after they signed in. A hand-minted per-user token (the
// user-token API, or an operator minting for a user's subject) is refused as a
// Web UI sign-in in strict mode, and — with auth.strict.user_tokens: gate_only
// — on every API transport and at mint too. Operator, service and declared
// tokens are machine credentials and are never affected.

import (
	"context"
	"log/slog"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/auth"
)

// userTokenCandidate reports whether p is a minted token that, were its
// subject a registered user, would be a hand-minted per-user token: a token
// row (not a declared principal, not the legacy secret) that no gate issued
// and that carries no operator-plane or confined-credential scope. Whether
// the subject IS a user needs the store (see handMintedUserToken).
//
// "Per-user" is decided by the users row rather than by how the token was
// minted, because the row does not record that: a token minted through the
// user-token API and one an operator minted for the same user's subject are
// the same credential to a caller. A token for a subject that is no user (a
// CI job, a bridge) is a service token.
func userTokenCandidate(p auth.Principal) bool {
	if p.Legacy || p.TokenDefID == "" || strings.HasPrefix(p.TokenDefID, "cfg:") || p.IssuedByGate != "" {
		return false
	}
	return !auth.HasScope(p.Scopes, auth.ScopeTenant) && !auth.IsAuthManager(p)
}

// handMintedUserToken reports whether p is a hand-minted per-user token. A
// store error reads as per-user, so the check fails closed.
func (s *Server) handMintedUserToken(ctx context.Context, p auth.Principal) bool {
	if !userTokenCandidate(p) || s.store == nil {
		return false
	}
	if _, err := s.store.UserGet(ctx, p.TenantID, p.Subject); err != nil {
		if isNotFound(err) {
			return false
		}
		if ctx.Err() == nil {
			slog.Warn("auth: could not tell whether a token is a user's (refusing it)", "token_def", p.TokenDefID, "err", err)
		}
		return true
	}
	return true
}

// gateOnlyApplies reports whether p must pass the gate_only check: strict mode
// with auth.strict.user_tokens: gate_only, and a token that could be a
// hand-minted per-user one. liveTokenPrincipal then reads the user.
func (s *Server) gateOnlyApplies(p auth.Principal) bool {
	pol := s.cfg().ResolvedAuth
	return pol.Strict() && pol.UserTokensGateOnly && userTokenCandidate(p)
}

// gateOnlyMintRefused reports whether the user-token API is closed: under
// gate_only a per-user token comes only from a gate.
func (s *Server) gateOnlyMintRefused() bool {
	pol := s.cfg().ResolvedAuth
	return pol.Strict() && pol.UserTokensGateOnly
}

// uiRefusesUserToken is the Web UI's refusal: in strict mode a browser signs
// in with a pasted per-user token only when a gate issued it.
func (s *Server) uiRefusesUserToken(ctx context.Context, p auth.Principal) bool {
	return s.cfg().ResolvedAuth.Strict() && s.handMintedUserToken(ctx, p)
}
