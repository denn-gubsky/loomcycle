package http

// authgate_token.go — RFC FA §7: issued tokens. A non-browser client (the CLI
// and the MCP thin client through the device gate, a bridge or an AWS workload
// through an assertion) gets a loomcycle bearer that loomcycle mints for the
// person a gate verified. It is a delegated user token like any other —
// scopes from the user's access_mode, its ACL reach the user's own (RFC EY
// "inherit": a token resolves to its user's groups and bindings) — but always
// short-lived and tagged with the gate that vouched, so strict mode can tell
// it from a hand-minted one.
//
// What it is NOT: an operator token. An owner's tenant-operator power comes
// from its owner binding on a SESSION; an issued token carries only the
// member scopes access_mode grants, as every delegated user token does, which
// also keeps it inside what retireDelegatedUserTokens retires on disable.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/audit"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

var (
	// errGateTokenUser: the user is not active (unknown, pending or
	// disabled); no token is issued.
	errGateTokenUser = errors.New("auth gate: no active user to issue a token for")
	// errGateTokenGate: the named gate is not configured.
	errGateTokenGate = errors.New("auth gate: unknown gate")
)

// gateTokenTTL is the lifetime cap for tokens a gate issues: its identity
// map's new_user.token_ttl, or the deployment floor's for a gate with none.
func (s *Server) gateTokenTTL(g config.AuthGate) time.Duration {
	ttl := s.cfg().ResolvedAuth.NewUserFloor().TokenTTL
	if g.Identity != nil && g.Identity.NewUser.TokenTTL > 0 {
		ttl = g.Identity.NewUser.TokenTTL
	}
	if ttl <= 0 {
		ttl = config.DefaultNewUserTokenTTL
	}
	return ttl
}

// gateTokenName is the token's label: "u-gate-<gate>", within the 64 a name
// allows.
func gateTokenName(gate string) string {
	var b strings.Builder
	b.WriteString(userTokenNamePrefix + "gate-")
	for _, r := range gate {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			b.WriteRune(r)
		}
	}
	n := b.String()
	if len(n) > 64 {
		n = n[:64]
	}
	return n
}

// issueGateToken mints a per-user bearer for (tenant, subject), vouched for
// by gate. ttl is what the caller asks for (the assertion's or the STS
// credential's remaining life, say); zero or more than the gate allows gives
// the gate's cap, which LOOMCYCLE_USER_TOKEN_MAX_TTL_SECONDS bounds again. A
// token never lives forever. restrictions narrow it (validated here); a
// caller's own principal, when the context carries one, bounds them further.
// The plaintext is returned once and never logged.
func (s *Server) issueGateToken(ctx context.Context, tenant, subject, gate string, ttl time.Duration, restrictions auth.Restrictions) (string, store.OperatorTokenDefRow, error) {
	if s.store == nil {
		return "", store.OperatorTokenDefRow{}, errSessionsUnavailable
	}
	if tenant == "" || subject == "" {
		return "", store.OperatorTokenDefRow{}, errGateTokenUser
	}
	g, ok := s.configuredGate(gate)
	if !ok {
		return "", store.OperatorTokenDefRow{}, errGateTokenGate
	}
	// RFC FA P5: the tenant's gate policy holds for issued tokens too.
	if err := s.tenantGateAllowed(ctx, tenant, gate); err != nil {
		return "", store.OperatorTokenDefRow{}, err
	}
	limit := s.gateTokenTTL(g)
	if s.userTokenMaxTTL > 0 && s.userTokenMaxTTL < limit {
		limit = s.userTokenMaxTTL
	}
	if ttl <= 0 || ttl > limit {
		ttl = limit
	}
	if err := restrictions.Validate(restrictions.Agents != nil); err != nil {
		return "", store.OperatorTokenDefRow{}, fmt.Errorf("auth gate: %w", err)
	}
	if p, ok := auth.PrincipalFromContext(ctx); ok {
		var err error
		if restrictions, err = restrictions.Within(p.Restrictions); err != nil {
			return "", store.OperatorTokenDefRow{}, fmt.Errorf("auth gate: %w", err)
		}
	}
	rj, err := auth.MarshalRestrictions(restrictions)
	if err != nil {
		return "", store.OperatorTokenDefRow{}, err
	}
	user, err := s.store.UserGet(ctx, tenant, subject)
	if err != nil {
		if isNotFound(err) {
			return "", store.OperatorTokenDefRow{}, errGateTokenUser
		}
		return "", store.OperatorTokenDefRow{}, err
	}
	if user.Status != "active" {
		return "", store.OperatorTokenDefRow{}, errGateTokenUser
	}
	scopes, err := auth.GrantableUserScopes(user.AccessMode)
	if err != nil {
		return "", store.OperatorTokenDefRow{}, err
	}
	hasher, err := s.hasher()
	if err != nil {
		return "", store.OperatorTokenDefRow{}, err
	}
	plaintext, suffix, err := auth.MintToken()
	if err != nil {
		return "", store.OperatorTokenDefRow{}, err
	}
	now := time.Now().UTC()
	row, err := s.store.OperatorTokenDefCreate(ctx, store.OperatorTokenDefRow{
		DefID: newTokenDefID(), Name: gateTokenName(gate), TenantID: tenant, Subject: subject,
		TokenHash: hasher.Hash(plaintext), AllowedScopes: scopes, CreatedAt: now, ExpiresAt: now.Add(ttl),
		RestrictionsJSON: rj, IssuedByGate: gate,
	})
	if err != nil {
		return "", store.OperatorTokenDefRow{}, err
	}
	s.invalidateTokenCache(ctx)
	s.recordTokenAudit(ctx, "create", row)
	s.recordAuthEvent(audit.Event{Action: "auth_token_issued", AuthGate: gate, TargetTenant: tenant, TargetSubject: subject,
		TargetDefID: row.DefID, ScopesAfter: scopes})
	slog.Info("auth gate: issued a user token", "gate", gate, "tenant", tenant, "subject", subject,
		"def_id", row.DefID, "suffix", suffix, "expires", row.ExpiresAt)
	return plaintext, row, nil
}
