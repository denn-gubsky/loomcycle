package http

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/denn-gubsky/loomcycle/internal/runner"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/teamgraph"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// A schedule or a webhook with `delivery: team` starts a walk here.
//
// ONE WAY TO START A WALK. The start itself is `TeamDef op=run mode=detach`,
// dispatched exactly as an operator's call and a subscribed team's are
// (runSubscribedTeam): admission, the walk's run row, its recorded spec and
// its finish all come from that one path. What this file adds is only what a
// trigger has and a caller does not — an identity that comes from a stored
// definition instead of a request, and a need to tell apart the ways a start
// can fail.

var _ runner.TeamWalkStarter = (*Server)(nil)

// StartTeamWalk implements runner.TeamWalkStarter.
func (s *Server) StartTeamWalk(ctx context.Context, in runner.TeamWalkInput) (string, error) {
	if s.store == nil || s.teamDefTool == nil {
		return "", fmt.Errorf("%w: team walks are not configured on this server", runner.ErrInternal)
	}
	// The user is the one identity field a trigger may take from outside (a
	// webhook's user_id mapping), so it is held to the rule a run's is.
	if in.UserID != "" && !validIdent(in.UserID) {
		return "", fmt.Errorf("%w: user_id must match [A-Za-z0-9_-]{1,128}", runner.ErrInvalidArgument)
	}
	// A trigger is an admission surface: while the runtime is quiescing for a
	// snapshot it starts nothing, as a triggered agent run does not.
	if err := s.pausedRunErr(); err != nil {
		return "", err
	}
	if err := s.teamWalkStartable(ctx, in); err != nil {
		return "", err
	}

	body, err := json.Marshal(map[string]any{
		"op":    "run",
		"name":  in.Team,
		"mode":  "detach",
		"input": in.Input,
		"vars":  in.Vars,
	})
	if err != nil {
		return "", fmt.Errorf("%w: %v", runner.ErrInternal, err)
	}
	start := &triggerWalkStart{idempotencyKey: in.IdempotencyKey, deliveryAltKey: in.DeliveryAltKey}
	res, err := s.TeamDef(triggerWalkCtx(ctx, in, start), body)
	if err != nil {
		return "", fmt.Errorf("%w: %v", runner.ErrInternal, err)
	}
	if res.IsError {
		// The run row lost a race for its delivery key: another delivery of
		// the same event got there first. Typed, so the receiver can answer
		// with the winner instead of failing the delivery.
		if errors.Is(start.openErr, store.ErrDuplicateIdempotencyKey) {
			return "", start.openErr
		}
		return "", fmt.Errorf("start team walk %q: %s", in.Team, res.Text)
	}
	var out struct {
		RunID string `json:"run_id"`
	}
	if err := json.Unmarshal([]byte(res.Text), &out); err != nil || out.RunID == "" {
		return "", fmt.Errorf("%w: the walk of team %q started but reported no run id", runner.ErrInternal, in.Team)
	}
	return out.RunID, nil
}

// teamWalkStartable answers, BEFORE the start, the refusals a trigger has to
// tell apart from a failure: runner.ErrTeamNotStartable (the trigger names
// something the team is not — fix the definition) and
// runner.ErrTokenLimitExceeded (the budget is spent — wait).
//
// It reads only, and op=run makes every one of these checks again; this is not
// a second start. It exists because op=run reports a refusal as TEXT, which is
// right for the person or model that called it and useless to a sweeper
// deciding whether a tick counts against max_fires, or a receiver choosing
// between 400 and 503. A team changed between this read and the start is
// refused by op=run itself, as an unclassified failure.
func (s *Server) teamWalkStartable(ctx context.Context, in runner.TeamWalkInput) error {
	// The trigger definition's tenant, and only that tenant's active pointer —
	// the same single lookup op=run makes for a caller of that tenant.
	row, err := s.store.TeamDefGetActive(ctx, in.TenantID, in.Team)
	if err != nil {
		var nf *store.ErrNotFound
		if errors.As(err, &nf) {
			return fmt.Errorf("%w: no active team %q in tenant %q", runner.ErrTeamNotStartable, in.Team, in.TenantID)
		}
		return fmt.Errorf("%w: read team %q: %v", runner.ErrInternal, in.Team, err)
	}
	if row.Retired {
		return fmt.Errorf("%w: team %q is retired", runner.ErrTeamNotStartable, in.Team)
	}
	def, err := teamgraph.Parse(row.Definition)
	if err != nil {
		return fmt.Errorf("%w: team %q: %v", runner.ErrTeamNotStartable, in.Team, err)
	}
	if err := teamgraph.CheckStartVars(def, in.Vars); err != nil {
		return fmt.Errorf("%w: team %q: %v", runner.ErrTeamNotStartable, in.Team, err)
	}
	if err := teamgraph.CheckInput(def, in.Input); err != nil {
		return fmt.Errorf("%w: team %q: %v", runner.ErrTeamNotStartable, in.Team, err)
	}
	if dec := s.limits.Check(in.TenantID, in.UserID); !dec.Allowed {
		return fmt.Errorf("%w: %s", runner.ErrTokenLimitExceeded, dec.Refusal.Message)
	}
	return nil
}

// triggerWalkCtx stamps the identity a triggered walk runs under.
//
// EVERYTHING FROM THE DEFINITION. Nobody is on a trigger's ctx — a tick has no
// request and a webhook delivery authenticates against the def's own secret,
// not as a principal — so the run identity set here is the whole of the walk's
// authority: op=run looks the team up under its tenant, admission reads its
// restriction bits (admitTeamRun), and the walk's row and every member it
// spawns inherit all of it. The tenant is the definition's EXECUTION tenant,
// which a non-admin author cannot point outside its own tenant; the two bits
// are the ones captured from that author. This is subscriptionCtx with a
// trigger definition in place of a promoter.
//
// The agent id is the walk's own label: nothing resolves it, and it must not
// read as some agent's.
func triggerWalkCtx(ctx context.Context, in runner.TeamWalkInput, start *triggerWalkStart) context.Context {
	ctx = context.WithValue(ctx, triggerWalkStartKey{}, start)
	return tools.WithRunIdentity(ctx, tools.RunIdentityValue{
		TenantID:              in.TenantID,
		UserID:                in.UserID,
		AgentID:               teamWalkAgentPrefix + in.Team,
		OperatorKeyRestricted: in.OperatorKeyRestricted,
		Isolated:              in.Isolated,
	})
}

// triggerWalkStart rides the ctx of one triggered start to the place the
// walk's run row is created (openTeamWalkRun), which is several calls below
// StartTeamWalk and reached through the TeamDef tool. It carries the delivery's
// dedup keys DOWN, so the row is the durable record that this delivery was
// handled, and the row's create error back UP, so a lost race for those keys
// is still an error value by the time the trigger sees it.
type triggerWalkStart struct {
	idempotencyKey string
	deliveryAltKey string
	openErr        error
}

type triggerWalkStartKey struct{}

// triggerWalkStartFrom returns the triggered start ctx belongs to, or nil for
// a walk somebody called for.
func triggerWalkStartFrom(ctx context.Context) *triggerWalkStart {
	start, _ := ctx.Value(triggerWalkStartKey{}).(*triggerWalkStart)
	return start
}
