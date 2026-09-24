package loop

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/steer"
)

// HeldReview describes a hold a run was in when it paused: the turn it was held
// at and its round. A resumed run given one is held again before any model call.
type HeldReview struct {
	SinceTurn int
	Round     int
	// HeldAt is when the hold began. A review deadline runs from it, so a
	// restart does not hand an unreviewed hold a fresh window.
	HeldAt time.Time
}

// lastAssistantText is the text of the conversation's last assistant turn —
// the answer a restored hold was holding.
func lastAssistantText(messages []providers.Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role != "assistant" {
			continue
		}
		var b strings.Builder
		for _, c := range messages[i].Content {
			if c.Type == "text" {
				b.WriteString(c.Text)
			}
		}
		return b.String()
	}
	return ""
}

// StopReasonRejected is the stop reason of a run a reviewer rejected without
// feedback. The server maps it to the rejected run status.
const StopReasonRejected = "rejected"

// StopReasonReviewExpired is the stop reason of a run whose hold outlived its
// review deadline (RunOptions.ReviewTTL) with no verdict. The server maps it
// to the rejected run status: an answer nobody looked at is never approved.
const StopReasonReviewExpired = "review_expired"

// errReviewAbandoned ends a run whose hold stopped without a verdict for a
// reason other than its context (the steer queue closed).
var errReviewAbandoned = errors.New("the review hold ended without a verdict")

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
	// reviewExpired: the review deadline passed with no verdict; the run
	// ends rejected.
	reviewExpired
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
// A compaction is applied in place and the hold is announced again, so a live
// view that just rendered the compaction shows the run held. Disarming review
// while held releases the hold as approved: the retune pushes an approval, and
// the heartbeat re-reads the arming as a backstop for a disarm that lands just
// as the hold begins.
//
// acceptFrom is when verdicts start to count: the moment the hold began, or the
// zero time for a hold restored after a restart, whose steer queue is new to
// this process and so cannot hold a stale verdict — and may already hold a
// fresh one, sent before the restored hold got here.
//
// heldSince is when the hold began, which the review deadline runs from
// (opts.ReviewTTL; none when zero). A deadline that passes while the runtime is
// paused waits for the pause to lift: a paused runtime does not end runs.
func parkForReview(ctx context.Context, opts *RunOptions, messages []providers.Message, sinceTurn, round, lastCtxTokens, preambleTokens int, acceptFrom, heldSince time.Time, emit func(providers.Event)) ([]providers.Message, int, reviewOutcome) {
	heldAt := acceptFrom
	var deadline <-chan time.Time
	expiresAt := ""
	if opts.ReviewTTL > 0 {
		due := heldSince.Add(opts.ReviewTTL)
		expiresAt = due.UTC().Format(time.RFC3339)
		timer := time.NewTimer(time.Until(due))
		defer timer.Stop()
		deadline = timer.C
	}
	announce := func() {
		emit(providers.Event{Type: providers.EventAwaitingReview,
			AwaitingReview: &providers.AwaitingReviewEventInfo{SinceTurn: sinceTurn, Round: round, ExpiresAt: expiresAt}})
	}
	announce()
	t := time.NewTicker(parkHeartbeatInterval)
	defer t.Stop()
	pp := newParkPause(opts.PauseGate)
	defer pp.done()
	expiredWhilePaused := false
	for {
		select {
		case <-pp.declared():
			pp.onDeclared()
			continue
		case <-pp.lifted():
			pp.onLifted()
			if expiredWhilePaused {
				return messages, lastCtxTokens, reviewExpired
			}
			continue
		case <-deadline:
			if pp.recording() {
				expiredWhilePaused = true
				continue
			}
			return messages, lastCtxTokens, reviewExpired
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
