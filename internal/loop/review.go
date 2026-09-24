package loop

import (
	"context"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/steer"
)

// StopReasonRejected is the stop reason of a run a reviewer rejected without
// feedback. The server maps it to the rejected run status.
const StopReasonRejected = "rejected"

// reviewOutcome is how a hold for review ended.
type reviewOutcome int

const (
	// reviewApproved: the run completes (or, if interactive, parks for more
	// input) with the answer it was held on.
	reviewApproved reviewOutcome = iota
	// reviewRevise: feedback was appended as a user turn; the loop runs
	// another turn and, still armed, is held again.
	reviewRevise
	// reviewRejected: rejected with no feedback; the run ends rejected.
	reviewRejected
	// reviewAborted: ctx cancelled or the queue closed while held.
	reviewAborted
)

// parkForReview holds a run whose model has finished until an operator's
// verdict arrives on the steer queue.
//
// A verdict queued BEFORE the hold began is discarded. The server only accepts
// a verdict for a run whose latest event says it is held, so an old one can
// only be a duplicate of a verdict already acted on, and acting on it would
// approve (or reject) the revision nobody has read yet. An ordinary operator
// message is the exception: one that arrived after the last turn drained the
// queue is still the operator's intent, so it is read as feedback.
//
// A compaction is applied in place and the hold is announced again, so the
// run's latest event still says it is held. Disarming review while held (a
// retune) releases the hold as approved; it is noticed at the next heartbeat.
func parkForReview(ctx context.Context, opts *RunOptions, messages []providers.Message, sinceTurn, round, lastCtxTokens, preambleTokens int, emit func(providers.Event)) ([]providers.Message, int, reviewOutcome) {
	heldAt := time.Now()
	announce := func() {
		emit(providers.Event{Type: providers.EventAwaitingReview,
			AwaitingReview: &providers.AwaitingReviewEventInfo{SinceTurn: sinceTurn, Round: round}})
	}
	announce()
	t := time.NewTicker(parkHeartbeatInterval)
	defer t.Stop()
	for {
		select {
		case m, ok := <-opts.SteerQueue:
			if !ok {
				return messages, lastCtxTokens, reviewAborted
			}
			if m.IsVerdict() && m.EnqueuedAt.Before(heldAt) {
				continue
			}
			switch {
			case m.Kind == steer.KindCompact:
				messages = applyCompactSummary(messages, m.Text, m.KeepN, m.KeepFirst, emit)
				lastCtxTokens = estimatePromptTokens(preambleTokens, messages)
				announce()
				continue
			case m.Kind == steer.KindApprove:
				return messages, lastCtxTokens, reviewApproved
			case strings.TrimSpace(m.Text) == "":
				if m.Kind == steer.KindReject {
					return messages, lastCtxTokens, reviewRejected
				}
				continue // an empty operator message says nothing
			}
			// Feedback: a reject with text, or a plain operator message.
			messages = append(messages, providers.Message{
				Role:    "user",
				Content: []providers.ContentBlock{{Type: "text", Text: m.Text}},
			})
			if opts.OnSteer != nil {
				opts.OnSteer(m)
			}
			reResolveForOperatorTurn(ctx, opts, emit)
			return messages, lastCtxTokens, reviewRevise
		case <-t.C:
			if opts.OnHeartbeat != nil {
				opts.OnHeartbeat()
			}
			if !opts.reviewAtBoundary(ctx) {
				return messages, lastCtxTokens, reviewApproved
			}
		case <-ctx.Done():
			return messages, lastCtxTokens, reviewAborted
		}
	}
}
