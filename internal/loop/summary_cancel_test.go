package loop

import (
	"context"
	"errors"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// cutSummaryCtx returns a ctx that is cancelled as soon as p has streamed its
// partial text, so the summary call is cut mid-stream.
func cutSummaryCtx(t *testing.T, p *cutStreamProvider) context.Context {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { <-p.streamed; cancel() }()
	return ctx
}

// A summary call whose stream the cancelled ctx closed returned the partial
// text with a nil error, which every caller took for a finished summary.
func TestSummarize_StreamCutByCancelledCtxReturnsTheCtxError(t *testing.T) {
	p := &cutStreamProvider{streamed: make(chan struct{})}
	summary, err := Summarize(cutSummaryCtx(t, p), p, "m", distillableConvo(), 10)
	if !errors.Is(err, context.Canceled) || summary != "" {
		t.Fatalf("Summarize = (%q, %v), want no summary and context.Canceled", summary, err)
	}
}

// A driver's error frame racing the cancel is the cancel too, not a provider
// fault.
func TestSummarize_ErrorFrameUnderCancelledCtxReturnsTheCtxError(t *testing.T) {
	p := &cutStreamProvider{errFrame: true, streamed: make(chan struct{})}
	summary, err := Summarize(cutSummaryCtx(t, p), p, "m", distillableConvo(), 10)
	if !errors.Is(err, context.Canceled) || summary != "" {
		t.Fatalf("Summarize = (%q, %v), want no summary and context.Canceled", summary, err)
	}
}

// An auto/self compaction whose summary call is cut by the run's cancel did
// not happen: the history is left as it was and no context_compaction marker
// (the event a transcript replay rebuilds the history from) is emitted. It used
// to apply the truncated summary and emit a marker built from it.
func TestMaybeAutoCompact_SummaryCutByCancelAppliesNothing(t *testing.T) {
	msgs := distillableConvo()
	p := &cutStreamProvider{streamed: make(chan struct{})}
	opts := RunOptions{
		Provider:   p,
		Model:      "x",
		Compaction: &config.Compaction{KeepLastN: cptr(2), KeepFirst: cptr(true), TargetPercentage: cptr(10)},
	}
	var markers int
	out, did := maybeAutoCompact(cutSummaryCtx(t, p), opts, msgs, 0, 0, func(ev providers.Event) {
		if ev.Type == providers.EventContextCompaction {
			markers++
		}
	}, "auto")
	if did || markers != 0 {
		t.Fatalf("compacted = %v, markers = %d, want no compaction and no marker from a summary the cancel cut", did, markers)
	}
	if len(out) != len(msgs) || firstTextOf(out[len(out)-1]) != firstTextOf(msgs[len(msgs)-1]) {
		t.Errorf("history changed: %d messages, want the %d it had", len(out), len(msgs))
	}
}

// A summary whose done arrived before the cancel is a finished summary, and
// the compaction applies.
func TestMaybeAutoCompact_SummaryDoneBeforeCancelStillApplies(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := &cutStreamProvider{finishFirst: cancel}
	opts := RunOptions{
		Provider:   p,
		Model:      "x",
		Compaction: &config.Compaction{KeepLastN: cptr(2), KeepFirst: cptr(true), TargetPercentage: cptr(10)},
	}
	var info *providers.ContextCompactionEventInfo
	_, did := maybeAutoCompact(ctx, opts, distillableConvo(), 0, 0, func(ev providers.Event) {
		if ev.Type == providers.EventContextCompaction {
			info = ev.ContextCompaction
		}
	}, "auto")
	if !did || info == nil || info.Summary != "partial" {
		t.Fatalf("compacted = %v, marker = %+v, want the finished summary applied", did, info)
	}
}

func firstTextOf(m providers.Message) string {
	if len(m.Content) == 0 {
		return ""
	}
	return m.Content[0].Text
}
