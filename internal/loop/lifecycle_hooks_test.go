package loop

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/hooks"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/steer"
)

// scriptedHook answers its n-th call with resps[n], repeating the last.
type scriptedHook struct {
	mu    sync.Mutex
	resps []string
	calls int
	srv   *httptest.Server
}

func newScriptedHook(t *testing.T, resps ...string) *scriptedHook {
	t.Helper()
	h := &scriptedHook{resps: resps}
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		h.mu.Lock()
		i := h.calls
		h.calls++
		if i >= len(h.resps) {
			i = len(h.resps) - 1
		}
		resp := h.resps[i]
		h.mu.Unlock()
		_, _ = w.Write([]byte(resp))
	}))
	t.Cleanup(h.srv.Close)
	return h
}

func (h *scriptedHook) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.calls
}

func lifecycleHooks(t *testing.T, hs ...*hooks.Hook) *hooks.Dispatcher {
	t.Helper()
	reg := hooks.NewRegistry()
	for _, h := range hs {
		if _, err := reg.Register(h); err != nil {
			t.Fatal(err)
		}
	}
	return hooks.NewDispatcher(reg, nil)
}

// withHooks is a startReviewRun mutation: the given hooks, review not armed.
func withHooks(d *hooks.Dispatcher, more func(*RunOptions)) func(*RunOptions) {
	return func(o *RunOptions) {
		o.Review = false
		o.Hooks = d
		o.AgentName = "writer"
		if more != nil {
			more(o)
		}
	}
}

// An agent_start deny stops the run before any model call, and says why.
func TestLifecycle_AnAgentStartDenyStopsTheRunBeforeAnyModelCall(t *testing.T) {
	h := newScriptedHook(t, `{"decision":"deny","reason":"writer is disabled"}`)
	r := startReviewRun(t, context.Background(), withHooks(lifecycleHooks(t,
		&hooks.Hook{Owner: "ops", Name: "gate", Phase: hooks.PhaseAgentStart, CallbackURL: h.srv.URL}), nil))
	o := r.finish(t)
	if o.err == nil || !strings.Contains(o.err.Error(), "writer is disabled") || o.res.StopReason != StopReasonDeniedByHook {
		t.Fatalf("outcome = %+v, %v", o.res, o.err)
	}
	if r.prov.calls() != 0 {
		t.Errorf("the model was called %d times", r.prov.calls())
	}
}

// agent_start context goes into the prompt, and the hook does not run again
// for a resumed run, which has already started.
func TestLifecycle_AgentStartContextReachesThePromptOnce(t *testing.T) {
	h := newScriptedHook(t, `{"additional_context":"The user prefers short answers."}`)
	d := lifecycleHooks(t, &hooks.Hook{Owner: "ops", Name: "ctx", Phase: hooks.PhaseAgentStart, CallbackURL: h.srv.URL})
	r := startReviewRun(t, context.Background(), withHooks(d, nil))
	r.result(t)
	r.prov.mu.Lock()
	first := r.prov.requests[0]
	r.prov.mu.Unlock()
	last := first[len(first)-1]
	if last.Role != "user" || len(last.Content) != 2 || last.Content[1].Text != "The user prefers short answers." {
		t.Fatalf("prompt = %+v", first)
	}

	resumed := startReviewRun(t, context.Background(), withHooks(d, func(o *RunOptions) { o.Resumed = true }))
	resumed.result(t)
	if h.count() != 1 {
		t.Errorf("agent_start ran %d times, want once (not for the resumed run)", h.count())
	}
}

// A block sends its reason back as a user turn; the model answers again, and
// the hook sees that this is a retry.
func TestLifecycle_ABlockSendsTheModelBackWithTheReason(t *testing.T) {
	h := newScriptedHook(t, `{"decision":"block","reason":"cite a source"}`, `{}`)
	r := startReviewRun(t, context.Background(), withHooks(lifecycleHooks(t,
		&hooks.Hook{Owner: "ops", Name: "check", Phase: hooks.PhaseAgentStop, CallbackURL: h.srv.URL}), nil))
	res := r.result(t)
	if r.prov.calls() != 2 || r.prov.lastUserText() != "cite a source" {
		t.Fatalf("calls = %d, last user turn %q", r.prov.calls(), r.prov.lastUserText())
	}
	if res.FinalText != "answer 2" {
		t.Errorf("final = %q", res.FinalText)
	}
}

// A validator that blocks every answer fails the run after MaxStopBlocks
// retries, naming the hook and its reason.
func TestLifecycle_ABlockEveryTimeFailsTheRunAtTheCap(t *testing.T) {
	h := newScriptedHook(t, `{"decision":"block","reason":"never good enough"}`)
	r := startReviewRun(t, context.Background(), withHooks(lifecycleHooks(t,
		&hooks.Hook{Owner: "ops", Name: "picky", Phase: hooks.PhaseAgentStop, CallbackURL: h.srv.URL}), nil))
	o := r.finish(t)
	if o.err == nil || o.res.StopReason != StopReasonStopBlocked || !strings.Contains(o.err.Error(), "ops/picky") ||
		!strings.Contains(o.err.Error(), "never good enough") {
		t.Fatalf("outcome = %+v, %v", o.res, o.err)
	}
	if got := r.prov.calls(); got != MaxStopBlocks+1 {
		t.Errorf("the model answered %d times, want %d", got, MaxStopBlocks+1)
	}
}

// A hold is the review hold, naming the hook that took it; an approval lets
// the answer through.
func TestLifecycle_AHoldIsAReviewHoldNamingTheHook(t *testing.T) {
	h := newScriptedHook(t, `{"decision":"hold","reason":"a person should read this"}`)
	r := startReviewRun(t, context.Background(), withHooks(lifecycleHooks(t,
		&hooks.Hook{Owner: "ops", Name: "hold", Phase: hooks.PhaseAgentStop, CallbackURL: h.srv.URL}), nil))
	ev := r.waitFor(t, providers.EventAwaitingReview)
	if ev.AwaitingReview == nil || ev.AwaitingReview.HeldBy != "ops/hold" {
		t.Fatalf("hold = %+v", ev.AwaitingReview)
	}
	r.q <- steer.Message{Kind: steer.KindApprove, EnqueuedAt: time.Now()}
	if res := r.result(t); res.FinalText != "answer 1" || res.StopReason != "end_turn" {
		t.Errorf("result = %+v", res)
	}
}

// A hook's hold is not released by the heartbeat that releases a hold once
// review is disarmed: arming did not take it.
func TestLifecycle_AHooksHoldOutlastsTheDisarmedReviewHeartbeat(t *testing.T) {
	orig := parkHeartbeatInterval
	parkHeartbeatInterval = 10 * time.Millisecond
	defer func() { parkHeartbeatInterval = orig }()
	h := newScriptedHook(t, `{"decision":"hold"}`)
	r := startReviewRun(t, context.Background(), withHooks(lifecycleHooks(t,
		&hooks.Hook{Owner: "ops", Name: "hold", Phase: hooks.PhaseAgentStop, CallbackURL: h.srv.URL}), nil))
	r.waitFor(t, providers.EventAwaitingReview)
	select {
	case o := <-r.done:
		t.Fatalf("the hold was released without a verdict: %+v, %v", o.res, o.err)
	case <-time.After(150 * time.Millisecond):
	}
	r.q <- steer.Message{Kind: steer.KindReject, EnqueuedAt: time.Now()}
	if res := r.result(t); res.StopReason != StopReasonRejected {
		t.Errorf("result = %+v", res)
	}
}

// A run nothing can deliver a verdict to (no steer queue) cannot be held; a
// hold ends it rejected rather than accepting the answer.
func TestLifecycle_AHoldOnARunThatCannotBeHeldEndsItRejected(t *testing.T) {
	h := newScriptedHook(t, `{"decision":"hold"}`)
	r := startReviewRun(t, context.Background(), withHooks(lifecycleHooks(t,
		&hooks.Hook{Owner: "ops", Name: "hold", Phase: hooks.PhaseAgentStop, CallbackURL: h.srv.URL}),
		func(o *RunOptions) { o.SteerQueue = nil }))
	if res := r.result(t); res.StopReason != StopReasonRejected {
		t.Errorf("result = %+v", res)
	}
}

// A closed agent_stop hook that fails holds the answer for a person rather
// than letting it through.
func TestLifecycle_AFailedClosedStopHookHolds(t *testing.T) {
	r := startReviewRun(t, context.Background(), withHooks(lifecycleHooks(t,
		&hooks.Hook{Owner: "ops", Name: "down", Phase: hooks.PhaseAgentStop, CallbackURL: "http://127.0.0.1:1/nope", FailMode: hooks.FailClosed}), nil))
	ev := r.waitFor(t, providers.EventAwaitingReview)
	if ev.AwaitingReview.HeldBy != "ops/down" {
		t.Fatalf("hold = %+v", ev.AwaitingReview)
	}
	r.q <- steer.Message{Kind: steer.KindApprove, EnqueuedAt: time.Now()}
	r.result(t)
}

// A pre_compact hook may refuse a compaction: the history stays as it was, and
// the refusal is a declined compaction naming the hook's reason — never
// silent. post_compact reports one that happened, with its sizes, and runs off
// the loop's path.
func TestLifecycle_CompactionHooksGateAndReport(t *testing.T) {
	deny := newScriptedHook(t, `{"decision":"deny","reason":"keep the whole history for the audit"}`)
	opts := RunOptions{
		Provider: &steerProvider{}, Model: "x", AgentName: "writer",
		Compaction: &config.Compaction{KeepLastN: cptr(2), KeepFirst: cptr(true), TargetPercentage: cptr(10)},
		Hooks:      lifecycleHooks(t, &hooks.Hook{Owner: "ops", Name: "keep", Phase: hooks.PhasePreCompact, CallbackURL: deny.srv.URL}),
	}
	msgs := distillableConvo()
	var declined *providers.ContextDistillDeclinedInfo
	out, did := maybeAutoCompact(context.Background(), opts, msgs, 900, 1000, func(ev providers.Event) {
		if ev.ContextDistill != nil {
			declined = ev.ContextDistill
		}
	}, "auto")
	if did || len(out) != len(msgs) {
		t.Fatalf("a denied compaction ran: did=%v, %d -> %d messages", did, len(msgs), len(out))
	}
	if declined == nil || declined.Reason != providers.DistillDeclineDeniedByHook || !strings.Contains(declined.Message, "keep the whole history") {
		t.Fatalf("declined = %+v", declined)
	}

	var mu sync.Mutex
	var got string
	report := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = string(b)
		mu.Unlock()
	}))
	defer report.Close()
	opts.Hooks = lifecycleHooks(t, &hooks.Hook{Owner: "ops", Name: "log", Phase: hooks.PhasePostCompact, CallbackURL: report.URL})
	if _, did := maybeAutoCompact(context.Background(), opts, msgs, 900, 1000, func(providers.Event) {}, "self"); !did {
		t.Fatal("the compaction did not run")
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		body := got
		mu.Unlock()
		if body != "" {
			for _, want := range []string{`"phase":"post_compact"`, `"trigger":"self"`, `"before_tokens":`, `"after_tokens":`} {
				if !strings.Contains(body, want) {
					t.Errorf("payload lacks %s: %s", want, body)
				}
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("post_compact never reported")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A run that ends while an agent_stop hook is deciding ends cancelled: nobody
// approved its answer. It used to leave the loop normally and return the
// answer with no error, recorded as a completion.
func TestLifecycle_ARunCancelledWhileAStopHookDecidesEndsCancelled(t *testing.T) {
	deciding := make(chan struct{}, 1)
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		deciding <- struct{}{}
		<-r.Context().Done()
	}))
	t.Cleanup(hs.Close)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := startReviewRun(t, ctx, withHooks(lifecycleHooks(t,
		&hooks.Hook{Owner: "ops", Name: "slow", Phase: hooks.PhaseAgentStop, CallbackURL: hs.URL}), nil))
	select {
	case <-deciding:
	case <-time.After(2 * time.Second):
		t.Fatal("the agent_stop hook was never called")
	}
	cancel()
	o := r.finish(t)
	if !errors.Is(o.err, context.Canceled) || o.res.StopReason != "cancelled" {
		t.Fatalf("outcome = %+v, %v; want the run cancelled", o.res, o.err)
	}
}
