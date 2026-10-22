package loop

import (
	"context"
	"errors"
	"testing"
)

// A stateful step whose prose reply the run's cancel cut mid-stream. With no
// patch retries left, the last prose reply is taken as the run's final answer,
// so the cut text used to end the run end_turn with no error — recorded
// completed. It is the cancel: the run ends cancelled with the ctx's error.
func TestStatefulRun_ProseCutByRunCancelEndsCancelled(t *testing.T) {
	p := &cutStreamProvider{streamed: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { <-p.streamed; cancel() }()
	cx := statefulCtx(nil)
	zero := 0
	cx.MaxPatchRetries = &zero
	res, err := Run(ctx, RunOptions{
		Provider: p, Model: "x", Context: cx,
		Segments: []PromptSegment{{Role: "user", Content: []PromptContentBlock{{Type: "trusted-text", Text: "go"}}}},
	})
	if !errors.Is(err, context.Canceled) || res.StopReason != "cancelled" {
		t.Fatalf("Run = (stop_reason %q, err %v), want cancelled with context.Canceled", res.StopReason, err)
	}
}

// The same cut on a driver that surfaces its failed read as an error frame.
func TestStatefulRun_ErrorFrameUnderRunCancelEndsCancelled(t *testing.T) {
	p := &cutStreamProvider{errFrame: true, streamed: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { <-p.streamed; cancel() }()
	cx := statefulCtx(nil)
	zero := 0
	cx.MaxPatchRetries = &zero
	res, err := Run(ctx, RunOptions{
		Provider: p, Model: "x", Context: cx,
		Segments: []PromptSegment{{Role: "user", Content: []PromptContentBlock{{Type: "trusted-text", Text: "go"}}}},
	})
	if !errors.Is(err, context.Canceled) || res.StopReason != "cancelled" {
		t.Fatalf("Run = (stop_reason %q, err %v), want cancelled with context.Canceled", res.StopReason, err)
	}
}
