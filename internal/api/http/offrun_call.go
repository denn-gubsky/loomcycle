package http

import (
	"context"
	"fmt"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/runner"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// admitOffRunCall admits a model call a caller asked for with no run (a
// decision, a read-aloud, a transcription) and returns the context the call
// must be made on. A run's start is what normally holds a model call to its
// rules; with no run, this does each by hand, in this order:
//
//  1. The caller is the authenticated principal: its tenant and subject. With
//     no principal (open mode, the stdio MCP server) the call is the
//     operator's, filed under the shared tenant with no user, as a run started
//     without a user_id is. Nothing in the request can name another caller.
//  2. The run rate, as a run start takes it.
//  3. The budget: a caller at a hard token ceiling is refused before any call
//     (*connector.TokenLimitError), by the check run admission uses.
//  4. The caller's per-user cap, held until release is called.
//  5. The operator's provider key: whether this caller may spend it is derived
//     from the principal and stamped EXPLICITLY either way. The driver's check
//     reads a bit that means "allowed" when nothing stamped it, so a path that
//     stamped nothing would hand a restricted tenant the operator's key. The
//     credential resolver is stamped beside it, as at a run start, so a
//     restricted tenant holding its own key for the provider is still served,
//     on that key.
//  6. The bill: the context names who is charged (tools.WithMeteredOffRunCall),
//     which is what makes the side-call ledgers write the usage row under the
//     caller, and what lifts a tool's own refusal of a call with no run.
//
// what names the call in a run-plane token's refusal. surface is the
// grantable surface the call belongs to ("" for none): a run-plane token
// granted it is admitted (auth.Restrictions.Surfaces). release is never nil
// when err is nil, and must be called once the call is over.
func (s *Server) admitOffRunCall(ctx context.Context, what, surface string) (context.Context, func(), error) {
	if err := s.refuseAgentlessCall(ctx, what, surface); err != nil {
		return ctx, nil, err
	}
	// A call a caller asked for directly is a model call, like a run start.
	if err := s.admitPrincipalRate(ctx); err != nil {
		return ctx, nil, err
	}
	p, authed := auth.PrincipalFromContext(ctx)
	var tenant, user string
	if authed {
		tenant, user = p.TenantID, p.Subject
	}
	// The more restrictive of the principal and any run bit already on ctx: an
	// OR can only add a restriction.
	restricted := tools.AuthorOperatorKeyRestricted(ctx, s.cfg().Env.OperatorKeyRestriction)

	if dec := s.limits.Check(tenant, user); !dec.Allowed {
		return ctx, nil, &connector.TokenLimitError{Info: *dec.Refusal}
	}

	// Counted against the caller's per-user cap for as long as the call lasts —
	// its wait for a provider lane included — like the LLM gateway and
	// /v1/embeddings. An off-run call takes no run slot, so without this
	// nothing tied the calls one caller has waiting on a provider to that
	// caller.
	release := func() {}
	if s.sem != nil {
		releaseUser, err := s.sem.ReserveUser(ctx, tenant, user)
		if err != nil {
			if concurrency.IsPerUserQuotaExhausted(err) {
				// Both wrapped: the sentinel is what gRPC and MCP map, the typed
				// error is what the HTTP 429 body is built from.
				return ctx, nil, fmt.Errorf("%w: %w", runner.ErrPerUserQuotaExhausted, err)
			}
			return ctx, nil, fmt.Errorf("%w: %v", runner.ErrInternal, err)
		}
		release = releaseUser
	}
	// A provider lane takes turns across (tenant, user), which it reads off
	// the run identity on ctx. This call is no run — no run id, so it earns no
	// lease — but it has the same fairness identity as a run would.
	ctx = providers.WithRunMeta(ctx, providers.RunMeta{TenantID: tenant, UserID: user})

	// The identity the credential resolver looks a tenant's own key up under.
	// The agent name is cleared: an off-run call belongs to no agent, and a
	// transport's synthetic one (the MCP operator's) must not match an
	// agent-scoped credential on one surface and not on the others.
	ctx = tools.WithRunIdentity(ctx, tools.RunIdentityValue{
		TenantID:              tenant,
		UserID:                user,
		OperatorKeyRestricted: restricted,
		Isolated:              auth.IsIsolated(p, authed),
	})
	ctx = tools.WithAgentName(ctx, "")
	ctx = providers.WithCredentialResolver(ctx, s.credResolver)
	ctx = providers.WithOperatorKeyAllowed(ctx, !restricted)
	ctx = tools.WithMeteredOffRunCall(ctx, tools.MeteredOffRunCallValue{TenantID: tenant, UserID: user})
	return ctx, release, nil
}
