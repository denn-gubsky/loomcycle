package hooks

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeHook stands up a tiny HTTP server that returns the provided
// response body for every POST it receives. Records the bodies it
// received so tests can assert what the dispatcher sent.
type fakeHook struct {
	srv      *httptest.Server
	respBody string
	status   int
	delay    time.Duration
	calls    int32
	bodies   []string
}

func newFakeHook(t *testing.T, respBody string) *fakeHook {
	t.Helper()
	f := &fakeHook{respBody: respBody, status: 200}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if f.delay > 0 {
			time.Sleep(f.delay)
		}
		atomic.AddInt32(&f.calls, 1)
		buf := make([]byte, 1<<14)
		n, _ := r.Body.Read(buf)
		f.bodies = append(f.bodies, string(buf[:n]))
		w.WriteHeader(f.status)
		_, _ = w.Write([]byte(f.respBody))
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func TestDispatcher_PreRewriteInput(t *testing.T) {
	hook := newFakeHook(t, `{"input":{"url":"https://safe.example/redacted"}}`)
	r := NewSet()
	mustRegister(t, r, &Hook{
		Owner: "x", Name: "redact", Phase: PhasePre, CallbackURL: hook.srv.URL,
		Tools: []string{"WebFetch"},
	})
	d := NewDispatcher(r, nil)

	out := d.RunPre(context.Background(),
		Identity{Agent: "a", UserID: "u", AgentID: "ag"},
		ToolCall{ID: "t1", Name: "WebFetch", Input: json.RawMessage(`{"url":"https://attacker/"}`)},
	)
	if out.Deny != nil {
		t.Fatalf("unexpected deny: %+v", out.Deny)
	}
	if string(out.Input) != `{"url":"https://safe.example/redacted"}` {
		t.Errorf("Input = %s, want rewritten", out.Input)
	}
}

func TestDispatcher_PreDenyShortCircuits(t *testing.T) {
	denyHook := newFakeHook(t, `{"deny":{"is_error":true,"text":"blocked by policy"}}`)
	// A second hook AFTER the deny one. It should NOT be called when a
	// prior hook short-circuits.
	laterHook := newFakeHook(t, `{"input":{"never":"reached"}}`)

	r := NewSet()
	mustRegister(t, r, &Hook{
		Owner: "x", Name: "deny", Phase: PhasePre, CallbackURL: denyHook.srv.URL,
		Tools: []string{"WebFetch"},
	})
	mustRegister(t, r, &Hook{
		Owner: "x", Name: "later", Phase: PhasePre, CallbackURL: laterHook.srv.URL,
		Tools: []string{"WebFetch"},
	})
	d := NewDispatcher(r, nil)

	out := d.RunPre(context.Background(),
		Identity{Agent: "a"},
		ToolCall{ID: "t1", Name: "WebFetch", Input: json.RawMessage(`{}`)},
	)
	if out.Deny == nil {
		t.Fatal("expected deny, got pass-through")
	}
	if !out.Deny.IsError || !strings.Contains(out.Deny.Text, "blocked") {
		t.Errorf("deny payload = %+v, want IsError + text~blocked", out.Deny)
	}
	if atomic.LoadInt32(&laterHook.calls) != 0 {
		t.Errorf("later hook was called %d time(s); deny must short-circuit the chain", laterHook.calls)
	}
}

func TestDispatcher_PostLIFORewrite(t *testing.T) {
	// Two post-hooks. Registered A, B. Post chain runs B (innermost)
	// then A (outermost). Each prepends its name so we can read the
	// final order.
	hookA := newFakeHook(t, `{"result":{"text":"A(B(orig))","is_error":false}}`)
	hookB := newFakeHook(t, `{"result":{"text":"B(orig)","is_error":false}}`)
	r := NewSet()
	mustRegister(t, r, &Hook{Owner: "x", Name: "A", Phase: PhasePost, CallbackURL: hookA.srv.URL, Tools: []string{"WebFetch"}})
	mustRegister(t, r, &Hook{Owner: "x", Name: "B", Phase: PhasePost, CallbackURL: hookB.srv.URL, Tools: []string{"WebFetch"}})
	d := NewDispatcher(r, nil)

	got := d.RunPost(context.Background(),
		Identity{Agent: "a"},
		ToolCall{ID: "t1", Name: "WebFetch", Input: json.RawMessage(`{}`)},
		ToolResult{Text: "orig"},
	)
	if got.Result.Text != "A(B(orig))" {
		t.Errorf("Post chain output = %q, want %q (LIFO: B inner, A outer)", got.Result.Text, "A(B(orig))")
	}
	// Confirm B saw the original, A saw B's rewrite.
	if !strings.Contains(hookB.bodies[0], `"text":"orig"`) {
		t.Errorf("B's payload didn't include original orig: %s", hookB.bodies[0])
	}
	if !strings.Contains(hookA.bodies[0], `"text":"B(orig)"`) {
		t.Errorf("A's payload didn't include B's rewrite: %s", hookA.bodies[0])
	}
}

func TestDispatcher_FailOpenPassesThroughOnTimeout(t *testing.T) {
	hook := newFakeHook(t, ``)
	hook.delay = 200 * time.Millisecond // exceeds our 50 ms timeout

	r := NewSet()
	mustRegister(t, r, &Hook{
		Owner: "x", Name: "slow", Phase: PhasePre, CallbackURL: hook.srv.URL,
		Tools: []string{"WebFetch"}, FailMode: FailOpen, TimeoutMs: 50,
	})
	d := NewDispatcher(r, nil)

	out := d.RunPre(context.Background(),
		Identity{Agent: "a"},
		ToolCall{ID: "t1", Name: "WebFetch", Input: json.RawMessage(`{"original":true}`)},
	)
	if out.Deny != nil {
		t.Errorf("fail_mode=open: timeout produced deny=%+v, want pass-through", out.Deny)
	}
	if string(out.Input) != `{"original":true}` {
		t.Errorf("Input = %s, want original (fail-open should preserve)", out.Input)
	}
}

func TestDispatcher_FailClosedDeniesOnTimeout(t *testing.T) {
	hook := newFakeHook(t, ``)
	hook.delay = 200 * time.Millisecond

	r := NewSet()
	mustRegister(t, r, &Hook{
		Owner: "x", Name: "slow", Phase: PhasePre, CallbackURL: hook.srv.URL,
		Tools: []string{"WebFetch"}, FailMode: FailClosed, TimeoutMs: 50,
	})
	d := NewDispatcher(r, nil)

	out := d.RunPre(context.Background(),
		Identity{Agent: "a"},
		ToolCall{ID: "t1", Name: "WebFetch", Input: json.RawMessage(`{}`)},
	)
	if out.Deny == nil {
		t.Fatal("fail_mode=closed: timeout did NOT produce deny; security-shaped hook must fail closed")
	}
	if !out.Deny.IsError {
		t.Errorf("deny.IsError = false, want true")
	}
}

func TestDispatcher_EmptyResponseBodyIsNoOp(t *testing.T) {
	// 204 No Content (or empty 200 body) → no rewrite. Common case
	// for telemetry-shaped hooks that just want to observe.
	hook := newFakeHook(t, ``)
	hook.status = 204

	r := NewSet()
	mustRegister(t, r, &Hook{
		Owner: "x", Name: "telem", Phase: PhasePre, CallbackURL: hook.srv.URL,
		Tools: []string{"WebFetch"},
	})
	d := NewDispatcher(r, nil)

	out := d.RunPre(context.Background(),
		Identity{Agent: "a"},
		ToolCall{ID: "t1", Name: "WebFetch", Input: json.RawMessage(`{"x":1}`)},
	)
	if out.Deny != nil {
		t.Errorf("204 produced deny: %+v", out.Deny)
	}
	if string(out.Input) != `{"x":1}` {
		t.Errorf("Input = %s, want unchanged on 204", out.Input)
	}
	if atomic.LoadInt32(&hook.calls) != 1 {
		t.Errorf("hook calls = %d, want 1 (still invoked, just no-op)", hook.calls)
	}
}

// TestDispatcher_NoMatchIsCheap pins the empty-registry fast path:
// TestDispatcher_PreAllowHosts_PermittedOwnerFlows: a single Pre-hook
// whose owner is on the operator-yaml permit list returns allow_hosts;
// the dispatcher's PreOutcome carries the grant + attribution + bumps
// the permitted counter.
func TestDispatcher_PreAllowHosts_PermittedOwnerFlows(t *testing.T) {
	hook := newFakeHook(t, `{"allow_hosts":["acme.com",".trusted-cdn.com"]}`)
	r := NewSet()
	mustRegister(t, r, &Hook{
		Owner: "jobs-search-web", WidenPermitted: true, Name: "url-gate", Phase: PhasePre,
		CallbackURL: hook.srv.URL, Tools: []string{"WebFetch"},
	})
	d := NewDispatcher(r, nil)

	out := d.RunPre(context.Background(),
		Identity{Agent: "a"},
		ToolCall{ID: "t1", Name: "WebFetch", Input: json.RawMessage(`{}`)},
	)
	if out.Deny != nil {
		t.Fatalf("unexpected deny: %+v", out.Deny)
	}
	if len(out.AllowHosts) != 2 {
		t.Fatalf("AllowHosts = %v, want 2 entries", out.AllowHosts)
	}
	if out.AllowHosts[0] != "acme.com" || out.AllowHosts[1] != ".trusted-cdn.com" {
		t.Errorf("AllowHosts = %v, want [acme.com .trusted-cdn.com] (order preserved)", out.AllowHosts)
	}
	if out.GrantingHookOwner != "jobs-search-web" || out.GrantingHookName != "url-gate" {
		t.Errorf("attribution = %s/%s, want jobs-search-web/url-gate",
			out.GrantingHookOwner, out.GrantingHookName)
	}
	if d.Stats().HostWidenPermitted != 1 {
		t.Errorf("Stats.HostWidenPermitted = %d, want 1", d.Stats().HostWidenPermitted)
	}
	if d.Stats().HostWidenDenied != 0 {
		t.Errorf("Stats.HostWidenDenied = %d, want 0", d.Stats().HostWidenDenied)
	}
}

// TestDispatcher_PreAllowHosts_UnpermittedOwnerDropped: a hook whose
// owner is NOT in the permit list returns allow_hosts; the
// dispatcher drops the grant, bumps the denied counter, but does not
// fail the call (the tool runs with the operator-floor host policy).
func TestDispatcher_PreAllowHosts_UnpermittedOwnerDropped(t *testing.T) {
	hook := newFakeHook(t, `{"allow_hosts":["acme.com"]}`)
	r := NewSet() // NOT our hook's owner
	mustRegister(t, r, &Hook{
		Owner: "jobs-search-web", Name: "rogue-gate", Phase: PhasePre,
		CallbackURL: hook.srv.URL, Tools: []string{"WebFetch"},
	})
	d := NewDispatcher(r, nil)

	out := d.RunPre(context.Background(),
		Identity{Agent: "a"},
		ToolCall{ID: "t1", Name: "WebFetch", Input: json.RawMessage(`{}`)},
	)
	if out.Deny != nil {
		t.Fatalf("unexpected deny: %+v", out.Deny)
	}
	if len(out.AllowHosts) != 0 {
		t.Errorf("AllowHosts = %v, want empty (owner not in permit list)", out.AllowHosts)
	}
	if d.Stats().HostWidenDenied != 1 {
		t.Errorf("Stats.HostWidenDenied = %d, want 1 (un-permitted grant should bump the counter)",
			d.Stats().HostWidenDenied)
	}
	if d.Stats().HostWidenPermitted != 0 {
		t.Errorf("Stats.HostWidenPermitted = %d, want 0", d.Stats().HostWidenPermitted)
	}
}

func TestDispatcher_PreAllowHosts_FailClosedTimeoutDiscardsPriorGrants(t *testing.T) {
	granter := newFakeHook(t, `{"allow_hosts":["acme.com"]}`)
	slowFailClosed := newFakeHook(t, ``)
	slowFailClosed.delay = 200 * time.Millisecond

	r := NewSet()
	mustRegister(t, r, &Hook{
		Owner: "jobs-search-web", WidenPermitted: true, Name: "grant", Phase: PhasePre,
		CallbackURL: granter.srv.URL, Tools: []string{"WebFetch"},
	})
	mustRegister(t, r, &Hook{
		Owner: "jobs-search-web", WidenPermitted: true, Name: "slow", Phase: PhasePre,
		CallbackURL: slowFailClosed.srv.URL, Tools: []string{"WebFetch"},
		FailMode: FailClosed, TimeoutMs: 50,
	})
	d := NewDispatcher(r, nil)

	out := d.RunPre(context.Background(),
		Identity{Agent: "a"},
		ToolCall{ID: "t1", Name: "WebFetch", Input: json.RawMessage(`{}`)},
	)
	if out.Deny == nil {
		t.Fatal("expected deny (FailClosed hook timed out), got pass-through")
	}
	if !out.Deny.IsError {
		t.Errorf("deny.IsError = false, want true")
	}
	if len(out.AllowHosts) != 0 {
		t.Errorf("AllowHosts = %v, want empty (FailClosed-induced deny must discard prior grants — same property as explicit deny)",
			out.AllowHosts)
	}
}

// TestDispatcher_PreAllowHosts_DenyDiscardsPriorGrants: a permitted
// hook contributes allow_hosts, then a later hook denies. The
// outcome must be a clean deny with NO leaked widening — denied
// chains do not influence policy.
func TestDispatcher_PreAllowHosts_DenyDiscardsPriorGrants(t *testing.T) {
	granter := newFakeHook(t, `{"allow_hosts":["acme.com"]}`)
	denier := newFakeHook(t, `{"deny":{"is_error":true,"text":"no"}}`)
	r := NewSet()
	mustRegister(t, r, &Hook{
		Owner: "jobs-search-web", WidenPermitted: true, Name: "grant", Phase: PhasePre,
		CallbackURL: granter.srv.URL, Tools: []string{"WebFetch"},
	})
	mustRegister(t, r, &Hook{
		Owner: "jobs-search-web", WidenPermitted: true, Name: "deny", Phase: PhasePre,
		CallbackURL: denier.srv.URL, Tools: []string{"WebFetch"},
	})
	d := NewDispatcher(r, nil)

	out := d.RunPre(context.Background(),
		Identity{Agent: "a"},
		ToolCall{ID: "t1", Name: "WebFetch", Input: json.RawMessage(`{}`)},
	)
	if out.Deny == nil {
		t.Fatal("expected deny, got pass-through")
	}
	if len(out.AllowHosts) != 0 {
		t.Errorf("AllowHosts = %v, want empty (deny must discard prior widenings)", out.AllowHosts)
	}
}

// TestDispatcher_PreAllowHosts_UnionAcrossPermittedHooks: two
// permitted hooks each return distinct host sets; the outcome
// contains the deduplicated UNION. Attribution names the LAST
// contributing hook.
func TestDispatcher_PreAllowHosts_UnionAcrossPermittedHooks(t *testing.T) {
	hookA := newFakeHook(t, `{"allow_hosts":["acme.com","shared.example"]}`)
	hookB := newFakeHook(t, `{"allow_hosts":["shared.example","other.example"]}`)
	r := NewSet()
	mustRegister(t, r, &Hook{
		Owner: "jobs-search-web", WidenPermitted: true, Name: "A", Phase: PhasePre,
		CallbackURL: hookA.srv.URL, Tools: []string{"WebFetch"},
	})
	mustRegister(t, r, &Hook{
		Owner: "company-research", WidenPermitted: true, Name: "B", Phase: PhasePre,
		CallbackURL: hookB.srv.URL, Tools: []string{"WebFetch"},
	})
	d := NewDispatcher(r, nil)

	out := d.RunPre(context.Background(),
		Identity{Agent: "a"},
		ToolCall{ID: "t1", Name: "WebFetch", Input: json.RawMessage(`{}`)},
	)
	if out.Deny != nil {
		t.Fatalf("unexpected deny: %+v", out.Deny)
	}
	if len(out.AllowHosts) != 3 {
		t.Fatalf("AllowHosts = %v, want 3 entries (acme.com, shared.example, other.example)",
			out.AllowHosts)
	}
	// Order preserved by first-seen: acme.com (A), shared.example (A first),
	// other.example (B).
	want := []string{"acme.com", "shared.example", "other.example"}
	for i, h := range want {
		if out.AllowHosts[i] != h {
			t.Errorf("AllowHosts[%d] = %q, want %q", i, out.AllowHosts[i], h)
		}
	}
	if out.GrantingHookOwner != "company-research" || out.GrantingHookName != "B" {
		t.Errorf("attribution = %s/%s, want company-research/B (last contributor)",
			out.GrantingHookOwner, out.GrantingHookName)
	}
	if d.Stats().HostWidenPermitted != 2 {
		t.Errorf("Stats.HostWidenPermitted = %d, want 2 (both hooks contributed)",
			d.Stats().HostWidenPermitted)
	}
}

// TestDispatcher_PreAllowHosts_MixedPermittedUnpermitted: a permitted
// hook and an un-permitted hook both return allow_hosts. The outcome
// carries ONLY the permitted hook's grant, attribution names the
// permitted hook, and both counters bump (1 permitted, 1 denied).
func TestDispatcher_PreAllowHosts_MixedPermittedUnpermitted(t *testing.T) {
	permitted := newFakeHook(t, `{"allow_hosts":["acme.com"]}`)
	rogue := newFakeHook(t, `{"allow_hosts":["evil.example"]}`)
	r := NewSet() // only jobs-search-web permitted
	mustRegister(t, r, &Hook{
		Owner: "jobs-search-web", WidenPermitted: true, Name: "good", Phase: PhasePre,
		CallbackURL: permitted.srv.URL, Tools: []string{"WebFetch"},
	})
	mustRegister(t, r, &Hook{
		Owner: "unknown-app", Name: "rogue", Phase: PhasePre,
		CallbackURL: rogue.srv.URL, Tools: []string{"WebFetch"},
	})
	d := NewDispatcher(r, nil)

	out := d.RunPre(context.Background(),
		Identity{Agent: "a"},
		ToolCall{ID: "t1", Name: "WebFetch", Input: json.RawMessage(`{}`)},
	)
	if len(out.AllowHosts) != 1 || out.AllowHosts[0] != "acme.com" {
		t.Fatalf("AllowHosts = %v, want [acme.com] only (rogue's evil.example must be dropped)",
			out.AllowHosts)
	}
	if out.GrantingHookOwner != "jobs-search-web" {
		t.Errorf("attribution owner = %q, want jobs-search-web", out.GrantingHookOwner)
	}
	if d.Stats().HostWidenPermitted != 1 {
		t.Errorf("Stats.HostWidenPermitted = %d, want 1", d.Stats().HostWidenPermitted)
	}
	if d.Stats().HostWidenDenied != 1 {
		t.Errorf("Stats.HostWidenDenied = %d, want 1", d.Stats().HostWidenDenied)
	}
}

// TestDispatcher_PreAllowHosts_NormalisesCase confirms hostnames are
// lower-cased and trimmed on entry — so a hook returning "ACME.COM "
// dedupes against a peer returning "acme.com".
func TestDispatcher_PreAllowHosts_NormalisesCase(t *testing.T) {
	hookA := newFakeHook(t, `{"allow_hosts":["ACME.COM ","  empty  ",""]}`)
	hookB := newFakeHook(t, `{"allow_hosts":["acme.com"]}`)
	r := NewSet()
	mustRegister(t, r, &Hook{
		Owner: "jobs-search-web", WidenPermitted: true, Name: "A", Phase: PhasePre,
		CallbackURL: hookA.srv.URL, Tools: []string{"WebFetch"},
	})
	mustRegister(t, r, &Hook{
		Owner: "jobs-search-web", WidenPermitted: true, Name: "B", Phase: PhasePre,
		CallbackURL: hookB.srv.URL, Tools: []string{"WebFetch"},
	})
	d := NewDispatcher(r, nil)

	out := d.RunPre(context.Background(),
		Identity{Agent: "a"},
		ToolCall{ID: "t1", Name: "WebFetch", Input: json.RawMessage(`{}`)},
	)
	if len(out.AllowHosts) != 2 {
		t.Fatalf("AllowHosts = %v, want 2 (acme.com + empty after norm + dup-of-acme-dropped)",
			out.AllowHosts)
	}
	if out.AllowHosts[0] != "acme.com" {
		t.Errorf("AllowHosts[0] = %q, want %q (lower-cased + trimmed)", out.AllowHosts[0], "acme.com")
	}
	if out.AllowHosts[1] != "empty" {
		t.Errorf("AllowHosts[1] = %q, want %q (whitespace-only entry surfaced)",
			out.AllowHosts[1], "empty")
	}
}

// when no hooks match the (agent, tool) the dispatcher does no
// network calls and returns the original input unchanged.
func TestDispatcher_NoMatchIsCheap(t *testing.T) {
	r := NewSet()
	mustRegister(t, r, &Hook{
		Owner: "x", Name: "scoped", Phase: PhasePre, CallbackURL: "https://nope/never",
		Agents: []string{"specific-agent"}, Tools: []string{"WebFetch"},
	})
	d := NewDispatcher(r, nil)

	// Different agent → no match. URL would fail to dial but we never
	// call it; if we do, the test surfaces a transport error.
	out := d.RunPre(context.Background(),
		Identity{Agent: "other-agent"},
		ToolCall{ID: "t1", Name: "WebFetch", Input: json.RawMessage(`{}`)},
	)
	if out.Deny != nil {
		t.Errorf("no-match produced deny: %+v", out.Deny)
	}
	if string(out.Input) != `{}` {
		t.Errorf("no-match Input = %s, want unchanged", out.Input)
	}
}

// tenantHookOnLoopback registers one fail-closed Pre hook whose callback is the
// fake server on 127.0.0.1, owned by the given tenant.
func tenantHookOnLoopback(t *testing.T, tenant string) (*fakeHook, *Set) {
	t.Helper()
	hook := newFakeHook(t, `{"input":{"rewritten":true}}`)
	r := NewSet()
	mustRegister(t, r, &Hook{
		Tenant: tenant, Owner: "app", Name: "gate", Phase: PhasePre,
		CallbackURL: hook.srv.URL, Tools: []string{"WebFetch"}, FailMode: FailClosed,
	})
	return hook, r
}

// A tenant operator's hook receives every matching tool input. Left unguarded,
// its callback URL let a tenant stream those inputs to the cloud metadata
// endpoint or any internal service the runtime can reach — the dial must be
// refused, and the refusal must fail the hook the way its fail_mode says.
func TestDispatcher_TenantHookCannotReachAPrivateAddress(t *testing.T) {
	hook, r := tenantHookOnLoopback(t, "acme")
	d := NewDispatcher(r, nil)

	out := d.RunPre(context.Background(),
		Identity{Agent: "a", Tenant: "acme"},
		ToolCall{ID: "t1", Name: "WebFetch", Input: json.RawMessage(`{"secret":"tool input"}`)},
	)
	if n := atomic.LoadInt32(&hook.calls); n != 0 {
		t.Fatalf("a tenant hook reached a loopback callback %d time(s); the tool input left the runtime", n)
	}
	if out.Deny == nil || !out.Deny.IsError {
		t.Errorf("a refused fail-closed hook must deny the call, got %+v", out)
	}
}

// The operator's own escape hatch: a private host it vouched for is reachable.
func TestDispatcher_TenantHookReachesAnOperatorVouchedPrivateHost(t *testing.T) {
	hook, r := tenantHookOnLoopback(t, "acme")
	d := NewDispatcherWithPrivateHosts(r, nil, []string{"127.0.0.1"})

	out := d.RunPre(context.Background(),
		Identity{Agent: "a", Tenant: "acme"},
		ToolCall{ID: "t1", Name: "WebFetch", Input: json.RawMessage(`{}`)},
	)
	if n := atomic.LoadInt32(&hook.calls); n != 1 {
		t.Fatalf("vouched host: hook called %d times, want 1", n)
	}
	if out.Deny != nil || !strings.Contains(string(out.Input), "rewritten") {
		t.Errorf("vouched host: hook result not applied, got %+v", out)
	}
}

// An operator-global hook (admin / legacy / open mode register with no tenant)
// is the operator's own code — commonly a localhost sidecar — and must keep
// working exactly as before the guard existed.
func TestDispatcher_OperatorGlobalHookStillReachesLoopback(t *testing.T) {
	hook, r := tenantHookOnLoopback(t, "")
	d := NewDispatcher(r, nil)

	d.RunPre(context.Background(),
		Identity{Agent: "a", Tenant: "acme"},
		ToolCall{ID: "t1", Name: "WebFetch", Input: json.RawMessage(`{}`)},
	)
	if n := atomic.LoadInt32(&hook.calls); n != 1 {
		t.Fatalf("operator-global hook: called %d times, want 1 (the guard must not break sidecar hooks)", n)
	}
}
