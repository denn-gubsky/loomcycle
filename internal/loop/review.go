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
	// HeldBy names the agent_stop hook that took the hold; empty for a hold
	// review arming took.
	HeldBy string
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

// StopReasonDeniedByHook is the stop reason of a run an agent_start hook
// denied before any model call.
const StopReasonDeniedByHook = "denied_by_hook"

// StopReasonStopBlocked is the stop reason of a run whose answer agent_stop
// hooks blocked more than MaxStopBlocks times in a row.
const StopReasonStopBlocked = "stop_blocked"

// MaxStopBlocks is how many times in a row agent_stop hooks may block an
// answer and send the model back. One block past it fails the run: a
// validator that can never be satisfied must not loop a run forever.
const MaxStopBlocks = 3

// blockTurn is the user turn an agent_stop block sends back to the model.
func blockTurn(reason string) providers.Message {
	return providers.Message{Role: "user", Content: []providers.ContentBlock{{Type: "text", Text: reason}}}
}

// appendStartContext adds agent_start hooks' context to the prompt: into the
// last user turn, as text of its own, so the prompt still ends on the user and
// a transcript replay can put it back in the same place.
func appendStartContext(messages []providers.Message, extra []string) []providers.Message {
	if len(extra) == 0 {
		return messages
	}
	blocks := make([]providers.ContentBlock, 0, len(extra))
	for _, t := range extra {
		blocks = append(blocks, providers.ContentBlock{Type: "text", Text: t})
	}
	if n := len(messages); n > 0 && messages[n-1].Role == "user" {
		last := messages[n-1]
		last.Content = append(append([]providers.ContentBlock(nil), last.Content...), blocks...)
		out := append([]providers.Message(nil), messages[:n-1]...)
		return append(out, last)
	}
	return append(messages, providers.Message{Role: "user", Content: blocks})
}

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
// paused waits for the pause to lift: a paused runtime does not end runs. For
// the same reason a verdict that arrives while paused waits for the lift too,
// and one that waited out the pause wins over a deadline that passed in it.
//
// heldBy names the agent_stop hook that took the hold, or is empty when review
// arming took it. A hook's hold is not released by disarming review: arming
// did not take it.
func parkForReview(ctx context.Context, opts *RunOptions, messages []providers.Message, sinceTurn, round, lastCtxTokens, preambleTokens int, acceptFrom, heldSince time.Time, heldBy string, emit func(providers.Event)) ([]providers.Message, int, reviewOutcome) {
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
			AwaitingReview: &providers.AwaitingReviewEventInfo{SinceTurn: sinceTurn, Round: round, ExpiresAt: expiresAt, HeldBy: heldBy}})
	}
	announce()
	t := time.NewTicker(parkHeartbeatInterval)
	defer t.Stop()
	pp := newParkPause(opts.PauseGate)
	defer pp.done()
	expiredWhilePaused := false
	// handle acts on one message from the steer queue; the bool reports that
	// it ended the hold, with the outcome.
	handle := func(m steer.Message) (reviewOutcome, bool) {
		if m.IsVerdict() && m.EnqueuedAt.Before(heldAt) {
			return 0, false
		}
		switch {
		case m.Kind == steer.KindCompact:
			messages = applyCompactSummary(messages, m.Text, m.KeepN, m.KeepFirst, emit)
			lastCtxTokens = estimatePromptTokens(preambleTokens, messages)
			announce()
			return 0, false
		case m.Kind == steer.KindApprove:
			return reviewApproved, true
		case strings.TrimSpace(m.Text) == "":
			if m.Kind == steer.KindReject {
				return reviewRejected, true
			}
			return 0, false // an empty operator message says nothing
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
		return reviewRevise, true
	}
	for {
		// While the park records a pause, the queue is not read: a verdict
		// that arrives then waits in it, in order, and is acted on when the
		// pause lifts. A paused runtime does not end runs, and a verdict
		// would end this one (or start its next turn) under the pause.
		queue := opts.SteerQueue
		if pp.recording() {
			queue = nil
		}
		select {
		case <-pp.declared():
			pp.onDeclared()
			continue
		case <-pp.lifted():
			pp.onLifted()
			if expiredWhilePaused {
				// A verdict that waited out the pause was given before anyone
				// could act on the deadline; it stands over the expiry.
				for {
					select {
					case m, ok := <-opts.SteerQueue:
						if !ok {
							return messages, lastCtxTokens, reviewAborted
						}
						if outcome, done := handle(m); done {
							return messages, lastCtxTokens, outcome
						}
					default:
						return messages, lastCtxTokens, reviewExpired
					}
				}
			}
			continue
		case <-deadline:
			if pp.recording() {
				expiredWhilePaused = true
				continue
			}
			return messages, lastCtxTokens, reviewExpired
		case m, ok := <-queue:
			if !ok {
				return messages, lastCtxTokens, reviewAborted
			}
			if outcome, done := handle(m); done {
				return messages, lastCtxTokens, outcome
			}
			continue
		case <-t.C:
			if opts.OnHeartbeat != nil {
				opts.OnHeartbeat()
			}
			// A disarm is an approval, held back while paused like any other.
			if heldBy == "" && !pp.recording() && !opts.reviewAtBoundary(ctx) {
				return messages, lastCtxTokens, reviewApproved
			}
		case <-ctx.Done():
			return messages, lastCtxTokens, reviewAborted
		}
	}
}
