package lifecycle

import (
	"context"
	"errors"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// ErrPrincipalOwner: an op a workspace opened over the MCP server does not
// take (sharing; RFC GW P9 plan D-P9-4).
var ErrPrincipalOwner = errors.New("a workspace opened over the MCP server is not shared")

// PrincipalRule is when a workspace opened over loomcycle's MCP server is
// released (RFC GW D35 left it open; the P9 plan's D-P9-1 proposes it): at
// the first of its release, Idle without an op, MaxLifetime after it opened,
// and the end of the token that opened it.
type PrincipalRule struct {
	// Idle and MaxLifetime are workspace.mcp.idle_release_s and
	// max_lifetime_s; zero is no bound of that kind.
	Idle, MaxLifetime time.Duration
	// TokenValid reports whether the token definition that opened a
	// workspace still authenticates (not retired, not expired). A false
	// answer releases the workspace; an error keeps it until the next pass,
	// so a store outage releases nothing. nil checks no token.
	TokenValid func(ctx context.Context, tokenDefID string) (bool, error)
}

// SweepPrincipals releases the principal-owned workspaces the rule ends, and
// returns how many. A row already releasing whose worker is away is left for
// that worker's next attach, as Sweep leaves one.
func (m *Manager) SweepPrincipals(ctx context.Context, rule PrincipalRule) (int, error) {
	rows, err := m.store.WorkspaceListPrincipalOwned(ctx)
	if err != nil {
		return 0, err
	}
	now := m.now()
	// One answer per token per pass: a subject with many workspaces opened
	// by one token costs one read.
	valid := map[string]bool{}
	n := 0
	for _, r := range rows {
		if r.State == store.WorkspaceReleasing && !m.disp.Attached(r.WorkerID, r.TenantID) {
			continue
		}
		why := m.principalEnded(ctx, r, rule, now, valid)
		if why == "" {
			continue
		}
		if err := m.Release(ctx, r); err != nil {
			m.log.Warn("cannot release a workspace opened over MCP", "workspace_id", r.ID, "why", why, "err", err)
			continue
		}
		m.log.Info("released a workspace opened over MCP", "workspace_id", r.ID, "tenant", r.TenantID, "why", why)
		n++
	}
	return n, nil
}

// principalEnded names why the rule ends r, or "" when it keeps it.
func (m *Manager) principalEnded(ctx context.Context, r store.WorkspaceRow, rule PrincipalRule, now time.Time, valid map[string]bool) string {
	if r.State == store.WorkspaceReleasing {
		return "releasing"
	}
	if rule.MaxLifetime > 0 && now.Sub(r.CreatedAt) >= rule.MaxLifetime {
		return "max_lifetime"
	}
	last := r.LastUsedAt
	if last.Before(r.CreatedAt) {
		last = r.CreatedAt
	}
	if rule.Idle > 0 && now.Sub(last) >= rule.Idle {
		return "idle"
	}
	if rule.TokenValid == nil || r.OpenedToken == "" {
		return ""
	}
	ok, seen := valid[r.OpenedToken]
	if !seen {
		v, err := rule.TokenValid(ctx, r.OpenedToken)
		if err != nil {
			m.log.Warn("cannot read the token that opened a workspace; keeping it until the next pass", "workspace_id", r.ID, "err", err)
			return ""
		}
		ok = v
		valid[r.OpenedToken] = v
	}
	if !ok {
		return "token_ended"
	}
	return ""
}

type principalOwnerKey struct{}

// WithPrincipalOwner marks ctx as a call of the Workspace tool by the
// principal of a session of loomcycle's MCP server, outside any run (RFC GW
// D35). Only the runtime's own MCP twin sets it, from the session's
// authenticated principal; nothing a model sends reaches it.
func WithPrincipalOwner(ctx context.Context, o Owner) context.Context {
	return context.WithValue(ctx, principalOwnerKey{}, o)
}

// PrincipalOwnerFrom is the principal owner WithPrincipalOwner set, if any.
func PrincipalOwnerFrom(ctx context.Context) (Owner, bool) {
	o, ok := ctx.Value(principalOwnerKey{}).(Owner)
	return o, ok && o.IsPrincipal() && o.RunID != ""
}
