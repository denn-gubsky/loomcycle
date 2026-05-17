package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// stubTool is a tools.Tool that captures the input and returns a canned text.
type stubTool struct {
	name string
}

func (s *stubTool) Name() string                 { return s.name }
func (s *stubTool) Description() string          { return "" }
func (s *stubTool) InputSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (s *stubTool) Execute(_ context.Context, in json.RawMessage) (Result, error) {
	return Result{Text: "ran:" + s.name + ":" + string(in)}, nil
}

func TestDispatcher_NoFallback_HitReturnsToolResult(t *testing.T) {
	d := NewDispatcher([]Tool{&stubTool{name: "Read"}})
	r := d.Execute(context.Background(), "Read", json.RawMessage(`{}`))
	if r.IsError {
		t.Fatalf("hit returned IsError=true: %q", r.Text)
	}
	if !strings.HasPrefix(r.Text, "ran:Read:") {
		t.Errorf("hit text = %q, want prefix ran:Read:", r.Text)
	}
}

func TestDispatcher_NoFallback_MissReturnsToolNotFound(t *testing.T) {
	d := NewDispatcher([]Tool{&stubTool{name: "Read"}})
	r := d.Execute(context.Background(), "Write", json.RawMessage(`{}`))
	if !r.IsError {
		t.Fatalf("miss returned IsError=false: %q", r.Text)
	}
	if !strings.Contains(r.Text, "tool not found") {
		t.Errorf("miss text = %q, want substring 'tool not found'", r.Text)
	}
}

// TestDispatcher_FallbackHandlesMiss verifies the fallback is consulted on
// a static-map miss and that its returned Result is propagated unchanged.
// This is the core contract LazyResolver depends on.
func TestDispatcher_FallbackHandlesMiss(t *testing.T) {
	called := 0
	fb := func(_ context.Context, name string, _ json.RawMessage) (Result, bool) {
		called++
		return Result{Text: "fallback:" + name, IsError: false}, true
	}
	d := NewDispatcherWithFallback([]Tool{&stubTool{name: "Read"}}, fb)
	r := d.Execute(context.Background(), "mcp__jobs__getAgentContext", json.RawMessage(`{}`))
	if r.IsError {
		t.Fatalf("fallback returned IsError=true: %q", r.Text)
	}
	if r.Text != "fallback:mcp__jobs__getAgentContext" {
		t.Errorf("fallback text = %q, want fallback:mcp__jobs__getAgentContext", r.Text)
	}
	if called != 1 {
		t.Errorf("fallback called %d times, want 1", called)
	}
}

// TestDispatcher_FallbackNotCalledOnHit guards against the fallback being
// invoked for tools that ARE in the static map. The static map is the fast
// path; touching the fallback would re-handshake the MCP pool unnecessarily
// for every native tool call.
func TestDispatcher_FallbackNotCalledOnHit(t *testing.T) {
	called := 0
	fb := func(_ context.Context, _ string, _ json.RawMessage) (Result, bool) {
		called++
		return Result{}, true
	}
	d := NewDispatcherWithFallback([]Tool{&stubTool{name: "Read"}}, fb)
	r := d.Execute(context.Background(), "Read", json.RawMessage(`{}`))
	if r.IsError {
		t.Fatalf("hit returned IsError=true: %q", r.Text)
	}
	if called != 0 {
		t.Errorf("fallback called %d times on a hit, want 0", called)
	}
}

// TestDispatcher_FallbackHandledFalseFallsThrough verifies the (handled=false)
// signal makes Execute return the standard "tool not found" error. The
// LazyResolver uses this to opt out of names it can't help with (non-MCP
// names, unconfigured server segments).
func TestDispatcher_FallbackHandledFalseFallsThrough(t *testing.T) {
	fb := func(_ context.Context, _ string, _ json.RawMessage) (Result, bool) {
		return Result{Text: "should be ignored"}, false
	}
	d := NewDispatcherWithFallback([]Tool{&stubTool{name: "Read"}}, fb)
	r := d.Execute(context.Background(), "Unknown", json.RawMessage(`{}`))
	if !r.IsError {
		t.Fatalf("expected IsError=true; got %q", r.Text)
	}
	if !strings.Contains(r.Text, "tool not found: Unknown") {
		t.Errorf("expected 'tool not found: Unknown' substring; got %q", r.Text)
	}
}

// TestExtraAllowedHosts_RoundTripsThroughCtx pins the basic ctx
// helper contract for the v0.8.17 per-call host-widening mechanism.
// The list is attached unmodified and retrieved unmodified — no
// normalisation in this layer (the dispatcher already normalised
// before calling WithExtraAllowedHosts; httptool will pattern-match
// at the enforcement site).
func TestExtraAllowedHosts_RoundTripsThroughCtx(t *testing.T) {
	ctx := context.Background()
	if got := ExtraAllowedHosts(ctx); got != nil {
		t.Errorf("bare ctx returned %v, want nil", got)
	}

	in := []string{"acme.com", ".trusted-cdn.com"}
	ctx2 := WithExtraAllowedHosts(ctx, in)
	got := ExtraAllowedHosts(ctx2)
	if len(got) != 2 || got[0] != "acme.com" || got[1] != ".trusted-cdn.com" {
		t.Errorf("ExtraAllowedHosts = %v, want %v (unmodified)", got, in)
	}

	// Parent ctx is unaffected (Go context immutability).
	if got := ExtraAllowedHosts(ctx); got != nil {
		t.Errorf("parent ctx mutated; ExtraAllowedHosts now = %v", got)
	}
}

// TestWithExtraAllowedHosts_NilEmptyIsNoOp confirms the optimization:
// passing nil or [] returns the input ctx unchanged. This matters
// because loop.dispatchOneTool calls WithExtraAllowedHosts on every
// tool dispatch — the common case (no permitted hook contributed)
// must not allocate a new ctx for nothing.
func TestWithExtraAllowedHosts_NilEmptyIsNoOp(t *testing.T) {
	ctx := context.Background()
	if WithExtraAllowedHosts(ctx, nil) != ctx {
		t.Error("nil extras: WithExtraAllowedHosts must return the input ctx unchanged")
	}
	if WithExtraAllowedHosts(ctx, []string{}) != ctx {
		t.Error("empty extras: WithExtraAllowedHosts must return the input ctx unchanged")
	}
}

// TestExtraAllowedHosts_NoLeakAcrossSiblingDerivations pins the
// scope invariant: a ctx derived AFTER WithExtraAllowedHosts sees the
// extras; a SIBLING derivation off the parent ctx does NOT. This is
// the property that keeps per-tool-call grants from leaking into
// sub-agent calls (sub-agents derive a fresh ctx from the loop's
// pre-dispatch ctx, not from the per-call execCtx).
func TestExtraAllowedHosts_NoLeakAcrossSiblingDerivations(t *testing.T) {
	parent := context.Background()
	widened := WithExtraAllowedHosts(parent, []string{"acme.com"})

	// Child of widened sees the extras.
	if got := ExtraAllowedHosts(widened); len(got) != 1 {
		t.Errorf("widened ctx: extras = %v, want [acme.com]", got)
	}

	// Sibling of widened (derived off `parent`) does NOT see them.
	sibling := WithExtraAllowedHosts(parent, nil) // == parent
	if got := ExtraAllowedHosts(sibling); got != nil {
		t.Errorf("sibling ctx (not derived from widened): extras = %v, want nil", got)
	}
}
