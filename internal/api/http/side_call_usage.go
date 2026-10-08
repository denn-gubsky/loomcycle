package http

import (
	"context"

	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// RecordRunSideCallUsage charges a model call made OUTSIDE the loop — the
// search rerank a Memory tool call runs, a Decision tool call — to whoever it
// was made for: one token_usage row, counted against the token budget like any
// other call.
//
// Without it the rerank would be free in every report and invisible to the
// budgets, while costing about 6,000 prompt tokens per search.
//
// Who is charged:
//
//   - A call the server admitted with no run behind it
//     (tools.WithMeteredOffRunCall, set only by Server.Decision): the caller it
//     names, on a row with no run, session or agent. The marker is read first
//     and is the ONLY thing that bills a call with no run. "No run id" is not
//     the test: that also describes an operator route dispatching a tool
//     directly, which has always been unbilled.
//   - Otherwise, a call inside a run: that run, by the run identity on ctx.
//   - Neither: nothing is recorded; there is nobody to charge.
//
// The budget counters are incremented, but a tier this call newly crosses is
// not announced here — there is no event stream at a tool call's depth — and
// the next loop call that crosses reports it.
func (s *Server) RecordRunSideCallUsage(ctx context.Context, u *providers.Usage) {
	if u == nil {
		return
	}
	if call, ok := tools.MeteredOffRunCall(ctx); ok {
		_ = s.recordCallUsage(ctx, "", tools.RunIdentityValue{TenantID: call.TenantID, UserID: call.UserID}, "", 0, u)
		return
	}
	runID := tools.RunID(ctx)
	if runID == "" {
		s.observeCall("", u) // nothing to charge, but the call still measured the model
		return
	}
	rid := tools.RunIdentity(ctx)
	_ = s.recordCallUsage(ctx, runID, rid, rid.SessionID, loop.IterationOf(ctx), u)
}
