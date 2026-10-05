package runner

import (
	"context"
	"errors"
)

// TeamWalkStarter is the seam a TRIGGER starts a team walk through. It sits
// beside Runner for the same reason Runner exists: the scheduler and the
// webhook receiver must not import internal/api/http, which owns the walk.
// *internal/api/http.Server satisfies both, and main.go hands the one instance
// to each trigger.
//
// It is deliberately narrower than the TeamDef tool it drives. A trigger has
// one thing to ask — "start this team's walk, detached, as this identity" —
// and no business with the tool's other arguments (a pinned version, a board,
// breakpoints).
type TeamWalkStarter interface {
	// StartTeamWalk starts a DETACHED walk of the active version of in.Team in
	// in.TenantID and returns the walk's run id once the walk exists; the walk
	// carries on after the call returns. It is the same start `TeamDef op=run
	// mode=detach` performs, with the same admission.
	//
	// The caller's identity is IN the input, never on ctx: a trigger fires
	// with nobody on the request, so the definition's own tenant and captured
	// bits are the only authority there is.
	//
	// Errors a caller branches on: ErrTeamNotStartable (wrapped, with the
	// reason), ErrInvalidArgument, ErrTokenLimitExceeded, ErrRuntimePaused.
	// Anything else is a failed start.
	StartTeamWalk(ctx context.Context, in TeamWalkInput) (runID string, err error)
}

// TeamWalkInput is one trigger's request to start a walk.
type TeamWalkInput struct {
	// Team is the team's name. Only the active version in TenantID is
	// started — never another tenant's, and never the shared layer's when
	// TenantID is a real tenant.
	Team string
	// Vars are the values for variables the team declares, name → text. A
	// name it does not declare, or a value it refuses, starts nothing.
	Vars map[string]string
	// Input is the walk's input.
	Input string

	// TenantID is the tenant the walk and every member it spawns run in. It
	// comes from the trigger DEFINITION's execution tenant only — the same
	// field, written under the same author guard, that an agent-run delivery
	// of the same trigger reads.
	TenantID string
	// UserID is who the walk is attributed to; "" attributes it to nobody.
	UserID string
	// OperatorKeyRestricted and Isolated are the confinement the trigger's
	// author was under when the definition was written, as RunInput carries
	// them for an agent-run delivery.
	OperatorKeyRestricted bool
	Isolated              bool

	// IdempotencyKey and DeliveryAltKey are the durable dedup identities of
	// the delivery that asked for this walk (see RunInput.IdempotencyKey).
	// Both empty when the trigger has none — a schedule.
	IdempotencyKey string
	DeliveryAltKey string
}

// ErrTeamNotStartable — the walk cannot start because of what the trigger
// names: no active team by that name in the tenant, a retired one, a variable
// the team does not declare, or a value or input it refuses. Nothing started,
// and nothing will until the trigger or the team is changed — the walk's twin
// of ErrUnknownAgent. The wrapping error carries the reason for the operator's
// log; a wire surface that answers an outside caller must not echo it.
// Wire: HTTP 400.
var ErrTeamNotStartable = errors.New("team walk not startable")
