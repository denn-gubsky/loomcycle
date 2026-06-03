package codejs

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// runErr drives a single Call with the given RunMeta and returns the EventError
// text (empty if the run produced none). Unlike runOnce it does not fail on an
// error — these tests assert on the error classification.
func runErr(t *testing.T, p *Provider, meta providers.RunMeta) string {
	t.Helper()
	ctx := providers.WithRunMeta(context.Background(), meta)
	ch, err := p.Call(ctx, providers.Request{
		Model:    "code-js",
		Messages: []providers.Message{{Role: "user", Content: []providers.ContentBlock{{Type: "text", Text: "go"}}}},
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	var errText string
	for ev := range ch {
		if ev.Type == providers.EventError {
			errText = ev.Error
		}
	}
	return errText
}

// TestCodeJS_BudgetTimeout_ClassifiedAsTimeout pins the review fix: a whole-run
// wall-clock budget exhaustion is reported as code_agent_timeout (with the
// budget, no source line), NOT code_agent_threw at whatever line the replay was
// interrupted. Drives it by stamping StartedAt in the past so the resume turn
// starts already over budget; the CPU loop is then interrupted by the timer.
func TestCodeJS_BudgetTimeout_ClassifiedAsTimeout(t *testing.T) {
	root := writeAgent(t, "spin", `function run(){ while(true){} }`)
	p := New(Config{CodeRoot: root, RunTimeout: 5 * time.Second})

	got := runErr(t, p, providers.RunMeta{
		AgentName: "spin",
		StartedAt: time.Now().Add(-10 * time.Second), // already past the 5s budget
	})
	if !strings.HasPrefix(got, "code_agent_timeout:") {
		t.Fatalf("budget exhaustion must classify as code_agent_timeout; got %q", got)
	}
	if strings.Contains(got, "code_agent_threw") || strings.Contains(got, "index.js:") {
		t.Errorf("timeout must not be reported as a throw at a source line; got %q", got)
	}
}

// TestCodeJS_CtxCancel_ClassifiedAsCancelled pins the cause-based
// classification: a parent/operator ctx cancellation is reported as
// code_agent_cancelled, NOT code_agent_timeout. With the budget far in the
// future, only interruptWatch's ctx.Done branch can fire, so cause==causeCancel
// — this guards the ordering fix that made the watcher cause (not a racy
// ctx.Err() read) authoritative for the stop reason.
func TestCodeJS_CtxCancel_ClassifiedAsCancelled(t *testing.T) {
	root := writeAgent(t, "spin", `function run(){ while(true){} }`)
	p := New(Config{CodeRoot: root, RunTimeout: time.Hour}) // huge budget ⇒ timer never fires

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancelled: interruptWatch's ctx.Done branch interrupts the spin loop
	ctx = providers.WithRunMeta(ctx, providers.RunMeta{AgentName: "spin", StartedAt: time.Now()})

	ch, err := p.Call(ctx, providers.Request{
		Model:    "code-js",
		Messages: []providers.Message{{Role: "user", Content: []providers.ContentBlock{{Type: "text", Text: "go"}}}},
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	var got string
	for ev := range ch {
		if ev.Type == providers.EventError {
			got = ev.Error
		}
	}
	if !strings.HasPrefix(got, "code_agent_cancelled:") {
		t.Fatalf("ctx cancel must classify as code_agent_cancelled; got %q", got)
	}
	if strings.Contains(got, "code_agent_timeout") {
		t.Errorf("a cancel must not be reported as a budget timeout; got %q", got)
	}
}

// TestCodeJS_RunTimeoutOverride_ExtendsBudget pins that RunMeta.RunTimeoutSeconds
// overrides the global default: with a tiny global but a large per-run override,
// a run that would be over-budget under the global completes instead.
func TestCodeJS_RunTimeoutOverride_ExtendsBudget(t *testing.T) {
	root := writeAgent(t, "quick", `function run(){ return {final_text:"done"}; }`)
	p := New(Config{CodeRoot: root, RunTimeout: time.Millisecond}) // tiny global

	// StartedAt 10s ago: under the 1ms global the budget is already < 0 (timeout);
	// the 3600s override gives ~3590s of headroom, so the run completes.
	got := runErr(t, p, providers.RunMeta{
		AgentName:         "quick",
		StartedAt:         time.Now().Add(-10 * time.Second),
		RunTimeoutSeconds: 3600,
	})
	if got != "" {
		t.Fatalf("per-run override should extend the budget so the run completes; got error %q", got)
	}
}
