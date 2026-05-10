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
