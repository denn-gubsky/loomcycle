package http

import (
	"context"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// SideCallUsage returns the recorder for model calls made OUTSIDE the loop for
// purpose — the search rerank a Memory tool call runs, a Decision tool call —
// which charges each to whoever it was made for: one token_usage row under that
// purpose, counted against the token budget like any other call.
//
// Without it the rerank would be free in every report and invisible to the
// budgets, while costing about 6,000 prompt tokens per search.
//
// Who is charged:
//
//   - A call the server admitted with no run behind it
//     (tools.WithMeteredOffRunCall, set only by Server.Decision): the caller it
//     names, on a row with no run, session or agent. The marker is read first
//     and is the ONLY thing that bills a side call with no run. "No run id" is
//     not the test: that also describes an operator route dispatching a tool
//     directly, which has always been unbilled.
//   - Otherwise, a call inside a run: that run, by the run identity on ctx.
//   - Neither: nothing is recorded; there is nobody to charge.
//
// The budget counters are incremented, but a tier this call newly crosses is
// not announced here — there is no event stream at a tool call's depth — and
// the next loop call that crosses reports it.
func (s *Server) SideCallUsage(purpose string) func(ctx context.Context, u *providers.Usage) {
	return func(ctx context.Context, u *providers.Usage) {
		if u == nil {
			return
		}
		runID, rid, sessionID, iteration, ok := sideCallCharge(ctx)
		if !ok {
			s.observeCall("", u) // nothing to charge, but the call still measured the model
			return
		}
		_ = s.recordCallUsage(ctx, runID, rid, sessionID, iteration, u, purpose)
	}
}

// runCallPurpose is the purpose a run's own model calls are charged under:
// consolidation for a run of an `internal:` agent, chat for every other.
func runCallPurpose(rid tools.RunIdentityValue) string {
	if rid.InternalAgent {
		return store.UsagePurposeConsolidation
	}
	return store.UsagePurposeChat
}

// callObserver is the RunOptions / ctx observer for the calls a run makes that
// emit no usage event — its compaction and recap summaries. Each is charged to
// runID under the purpose on its ctx and fed to the throughput estimate.
// runID "" is a summary made for a chat with no run (a History recap, a
// replay's compaction): charged to the identity on ctx with no run.
func (s *Server) callObserver(runID string) providers.CallObserver {
	return func(ctx context.Context, u *providers.Usage) { s.chargeObservedCall(ctx, runID, u) }
}

// BackgroundCallObserver is the observer for a model call the runtime makes
// for purpose outside any loop — Document unit generation, an image
// description — charged to the run on ctx if there is one, else to the tenant
// and user whose data the call worked on (the identity the operator route put
// on ctx), and fed to the throughput estimate.
//
// Unlike a side call, a call with no run is billed: it is real spend on that
// tenant's behalf, and an operator route is the only way these calls are made.
func (s *Server) BackgroundCallObserver(purpose string) providers.CallObserver {
	return func(ctx context.Context, u *providers.Usage) {
		s.chargeObservedCall(providers.WithCallPurpose(ctx, purpose), tools.RunID(ctx), u)
	}
}

// chargeObservedCall writes one observed call's usage row: to runID when set,
// under the purpose on ctx, with the run identity on ctx. A call whose ctx
// names no purpose is measured and not charged, since its row could not say
// what it was for.
func (s *Server) chargeObservedCall(ctx context.Context, runID string, u *providers.Usage) {
	if u == nil {
		return
	}
	purpose := providers.CallPurpose(ctx)
	if purpose == "" {
		s.observeCall(runID, u)
		return
	}
	rid := tools.RunIdentity(ctx)
	// On a ctx that outlives the caller: a summary or a Document job that
	// finished is spent even if its caller has gone, and a cancelled insert
	// would drop a row the budget had already counted.
	_ = s.recordCallUsage(context.WithoutCancel(ctx), runID, rid, rid.SessionID, loop.IterationOf(ctx), u, purpose)
}

// RecordEmbedCall writes one embedder call to the usage ledger as an
// embedding row: the characters sent, and the tokens when the provider
// reported them. It is charged like a background call — to the run on ctx, or
// to the tenant and user the identity on ctx names (the operator route or the
// gateway caller that asked for it). Its tokens count against the budgets; a
// call with no count is recorded by its characters and counts nothing there.
//
// The embedder is built once and shared by every subsystem; this is its
// ObservedEmbedder's call hook, so no call site has to remember to bill.
func (s *Server) RecordEmbedCall(ctx context.Context, c providers.EmbedCall) {
	if s.store == nil {
		return
	}
	runID := tools.RunID(ctx)
	rid := tools.RunIdentity(ctx)
	parentRunID := ""
	if rid.RootRunID != "" && rid.RootRunID != runID {
		parentRunID = rid.RootRunID
	}
	row := store.TokenUsageRow{
		RunID:            runID,
		SessionID:        rid.SessionID,
		TenantID:         rid.TenantID,
		UserID:           rid.UserID,
		AgentID:          rid.AgentID,
		ParentRunID:      parentRunID,
		Iteration:        loop.IterationOf(ctx),
		Provider:         c.Provider,
		Model:            c.Model,
		CredentialSource: "operator", // the shared embedder is built on the operator's key
		InputTokens:      c.InputTokens,
		InputChars:       c.Chars,
		Purpose:          store.UsagePurposeEmbedding,
		UserTier:         rid.UserTier,
		TS:               time.Now(),
	}
	if cfg := s.cfg(); cfg != nil {
		if cost, currency, ok := cfg.Pricing.CostEmbedding(c.Provider, c.Model, c.InputTokens, c.TokensReported, c.Chars); ok {
			row.Cost, row.CostCurrency = cost, currency
		}
	}
	// On a ctx that outlives the caller: the tokens were spent whether or not
	// the gateway client or the run is still there to hear the answer, and a
	// cancelled insert would drop a row the budget had already counted.
	_, _ = s.appendUsageRow(context.WithoutCancel(ctx), row)
}
