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

// ---- RFC F per-run credentials sugar in WithRunIdentity -----------

// TestWithRunIdentity_PromotesUserBearerToDefaultCred pins the v0.8.x
// back-compat sugar: callers that set UserBearer get an automatic
// UserCredentials["default"] entry pointing at the same value so the
// new ${run.credentials.default} substitution resolves identically
// to the legacy ${run.user_bearer} substitution.
func TestWithRunIdentity_PromotesUserBearerToDefaultCred(t *testing.T) {
	ctx := WithRunIdentity(context.Background(), RunIdentityValue{
		UserID:     "alice",
		UserBearer: "tok-xyz",
	})
	got := RunIdentity(ctx)
	if got.UserBearer != "tok-xyz" {
		t.Errorf("UserBearer = %q, want %q", got.UserBearer, "tok-xyz")
	}
	if v, ok := got.UserCredentials["default"]; !ok || v != "tok-xyz" {
		t.Errorf("UserCredentials[default] = %q (ok=%v), want %q", v, ok, "tok-xyz")
	}
}

// TestWithRunIdentity_DoesNotOverrideExplicitDefault pins the sugar's
// safety: if the caller explicitly populated UserCredentials["default"]
// (different from UserBearer, or with UserBearer empty), the sugar
// MUST NOT clobber it. Otherwise a v1.x caller migrating to the map
// shape would silently have their explicit default overwritten by a
// stale legacy bearer.
func TestWithRunIdentity_DoesNotOverrideExplicitDefault(t *testing.T) {
	ctx := WithRunIdentity(context.Background(), RunIdentityValue{
		UserID:          "alice",
		UserBearer:      "legacy-bearer",
		UserCredentials: map[string]string{"default": "explicit-default"},
	})
	got := RunIdentity(ctx)
	if got.UserCredentials["default"] != "explicit-default" {
		t.Errorf("UserCredentials[default] = %q, want %q (sugar must not clobber)",
			got.UserCredentials["default"], "explicit-default")
	}
}

// TestWithRunIdentity_PromotesOverEmptyDefault pins the
// empty-value-equals-missing case: when the caller's
// UserCredentials map carries `{default: ""}` (e.g., a sloppy
// orchestrator initialising the map with empty placeholders),
// the sugar STILL promotes UserBearer into the default slot.
// Otherwise ${run.user_bearer} would resolve to UserBearer while
// ${run.credentials.default} would silently drop the header
// (substitution treats empty-value as missing per RFC F Decision 4).
func TestWithRunIdentity_PromotesOverEmptyDefault(t *testing.T) {
	ctx := WithRunIdentity(context.Background(), RunIdentityValue{
		UserID:          "alice",
		UserBearer:      "tok-xyz",
		UserCredentials: map[string]string{"default": ""},
	})
	got := RunIdentity(ctx)
	if got.UserCredentials["default"] != "tok-xyz" {
		t.Errorf("UserCredentials[default] = %q, want %q (empty value should be promoted over)",
			got.UserCredentials["default"], "tok-xyz")
	}
}

// TestWithRunIdentity_EmptyBearerNoSugar pins that the sugar fires
// only when UserBearer is non-empty. A zero-RunIdentityValue with
// no fields set produces a nil/empty UserCredentials map — no
// surprise {default:""} entry that downstream code would have to
// special-case.
func TestWithRunIdentity_EmptyBearerNoSugar(t *testing.T) {
	ctx := WithRunIdentity(context.Background(), RunIdentityValue{UserID: "alice"})
	got := RunIdentity(ctx)
	if _, ok := got.UserCredentials["default"]; ok {
		t.Errorf("UserCredentials[default] should be absent when UserBearer is empty, got map = %v", got.UserCredentials)
	}
}

// TestWithRunIdentity_DoesNotMutateCallerMap pins the non-mutation
// invariant: the sugar must clone the credentials map before adding
// the default entry. Concurrent callers of WithRunIdentity must not
// see each other's writes through a shared map reference.
func TestWithRunIdentity_DoesNotMutateCallerMap(t *testing.T) {
	callerMap := map[string]string{"jobs": "jobs-tok"}
	_ = WithRunIdentity(context.Background(), RunIdentityValue{
		UserID:          "alice",
		UserBearer:      "tok-xyz",
		UserCredentials: callerMap,
	})
	if _, ok := callerMap["default"]; ok {
		t.Errorf("WithRunIdentity mutated caller's UserCredentials map; got %v", callerMap)
	}
	if len(callerMap) != 1 {
		t.Errorf("caller's map length changed; got %d entries: %v", len(callerMap), callerMap)
	}
}

// TestWithRunIdentity_PreservesOtherCreds pins that the sugar's
// promotion path preserves other credentials in the map (jobs,
// slack, etc.) when it adds the back-compat default entry.
func TestWithRunIdentity_PreservesOtherCreds(t *testing.T) {
	ctx := WithRunIdentity(context.Background(), RunIdentityValue{
		UserID:          "alice",
		UserBearer:      "legacy-tok",
		UserCredentials: map[string]string{"jobs": "jobs-tok", "slack": "slack-tok"},
	})
	got := RunIdentity(ctx)
	want := map[string]string{
		"jobs":    "jobs-tok",
		"slack":   "slack-tok",
		"default": "legacy-tok",
	}
	if len(got.UserCredentials) != len(want) {
		t.Fatalf("len(UserCredentials) = %d, want %d (got %v)",
			len(got.UserCredentials), len(want), got.UserCredentials)
	}
	for k, v := range want {
		if got.UserCredentials[k] != v {
			t.Errorf("UserCredentials[%q] = %q, want %q", k, got.UserCredentials[k], v)
		}
	}
}
