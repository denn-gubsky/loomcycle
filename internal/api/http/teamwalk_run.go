package http

import (
	"context"
	"fmt"
	"log"

	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// A team walk is a RUN.
//
// Everything that makes a walk observable or controllable from outside is
// addressed by run id — the breakpoint set, the Interruption ask a pause is
// answered through, cancel, the event stream. A walk that had no run had no
// handle, so none of them could reach it. That was not a gap in any one of
// them; it was the walk not being a first-class unit of work.
//
// The concrete failure this fixes: loomboard drives loomcycle through the TS
// adapter over `POST /v1/_teamdef {op:"run"}`, which dispatches under
// substrateAdminCtx — no run id, no Interruption policy. A breakpoint armed
// there could not be answered, and a pause that cannot be answered fails safe
// to ABORT, so arming one killed the walk instead of pausing it.

// teamWalkAgentPrefix names the synthetic agent a walk's run is filed under, so
// the runs list shows `team:triage` rather than a bare id. It is a run's
// `agent` label, never a resolvable AgentDef — the walk spawns its members
// through their own defs.
const teamWalkAgentPrefix = "team:"

// openTeamWalkRun gives one op=run walk its own run. Wired into the TeamDef
// tool by SetTeamDefTool.
//
// detach decides the ctx's lifetime, not the run's identity: a detached walk
// keeps the request's ctx VALUES (auth principal, tenant, admission) but is not
// cancelled when the handler returns, mirroring how an interactive run survives
// the client navigating away.
func (s *Server) openTeamWalkRun(ctx context.Context, teamName string, detach bool) (context.Context, string, func(error), error) {
	if s.store == nil {
		return ctx, "", func(error) {}, fmt.Errorf("run tracking requires a store")
	}
	ident := tools.RunIdentity(ctx)
	agent := teamWalkAgentPrefix + teamName
	_, runID, err := s.openOrCreateSessionAndRun(ctx, "", agent, ident.TenantID, ident.UserID, store.RunIdentity{
		AgentID: agent,
		UserID:  ident.UserID,
	})
	if err != nil {
		return ctx, "", func(error) {}, err
	}

	walkCtx := ctx
	if detach {
		walkCtx = context.WithoutCancel(ctx)
	}
	walkCtx = tools.WithRunID(walkCtx, runID)
	// The pause machinery is the Interruption tool's `ask`, which is gated on
	// the CALLING AGENT's policy. A walk has no AgentDef to carry one, so the
	// grant is made here, narrowed to the one kind a breakpoint uses: a
	// question. Without it every pause errors, and a pause that cannot be
	// answered aborts the walk.
	walkCtx = tools.WithInterruptionPolicy(walkCtx, tools.InterruptionPolicyValue{
		Enabled: true,
		Kinds:   []string{"question"},
	})

	finish := func(walkErr error) {
		status, msg := store.RunCompleted, ""
		if walkErr != nil {
			status, msg = store.RunFailed, walkErr.Error()
		}
		// A survival ctx: the run row must be closed even when the walk failed
		// because its ctx was cancelled, or a cancelled walk would sit in the
		// runs list as running forever.
		if ferr := s.store.FinishRun(context.WithoutCancel(walkCtx), runID, status, "", store.Usage{}, msg); ferr != nil {
			log.Printf("teamdef: finish walk run %s: %v", runID, ferr)
		}
	}
	return walkCtx, runID, finish, nil
}
